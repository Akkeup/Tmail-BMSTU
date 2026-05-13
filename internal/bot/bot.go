package bot

import (
	"fmt"
	"log"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"tg-mail-bot/internal/mailer"
	"tg-mail-bot/internal/storage"
)

const mb = 1024 * 1024

type loginStep int

const (
	stepIdle loginStep = iota
	stepWaitEmail
	stepWaitPassword
)

type loginState struct {
	step  loginStep
	email string
}

type Bot struct {
	api          *tgbotapi.BotAPI
	store        *storage.Storage
	pollInterval time.Duration
	states       map[int64]*loginState
	statesMu     sync.Mutex
}

func New(token string, store *storage.Storage, pollInterval time.Duration) (*Bot, error) {
	api, err := tgbotapi.NewBotAPI(token)
	if err != nil {
		return nil, err
	}
	log.Printf("Authorized as @%s", api.Self.UserName)
	return &Bot{
		api:          api,
		store:        store,
		pollInterval: pollInterval,
		states:       make(map[int64]*loginState),
	}, nil
}

func (b *Bot) Run() {
	go b.pollLoop()

	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60
	for update := range b.api.GetUpdatesChan(u) {
		if update.Message == nil {
			continue
		}
		b.handle(update.Message)
	}
}

func (b *Bot) handle(msg *tgbotapi.Message) {
	if msg.IsCommand() {
		b.clearState(msg.Chat.ID)
		switch msg.Command() {
		case "start":
			b.send(msg.Chat.ID,
				"Привет! Я уведомляю о новых письмах на student.bmstu.ru.\n\n"+
					"Команды:\n"+
					"/login — подключить почту\n"+
					"/status — проверить статус\n"+
					"/logout — отключить уведомления",
			)
		case "login":
			b.setState(msg.Chat.ID, &loginState{step: stepWaitEmail})
			b.send(msg.Chat.ID, "Введи адрес студенческой почты:")
		case "logout":
			_ = b.store.Delete(msg.Chat.ID)
			b.send(msg.Chat.ID, "Отключено. Уведомления остановлены.")
		case "status":
			user, ok := b.store.Get(msg.Chat.ID)
			if !ok {
				b.send(msg.Chat.ID, "Не подключено. Используй /login")
				return
			}
			b.send(msg.Chat.ID, fmt.Sprintf("Активно\nПочта: %s\nПроверка каждые %v.", user.Email, b.pollInterval))
		}
		return
	}

	b.handleText(msg)
}

func (b *Bot) handleText(msg *tgbotapi.Message) {
	b.statesMu.Lock()
	state := b.states[msg.Chat.ID]
	b.statesMu.Unlock()

	if state == nil {
		return
	}

	switch state.step {
	case stepWaitEmail:
		state.email = msg.Text
		state.step = stepWaitPassword
		b.send(msg.Chat.ID, "Теперь введи пароль:\n(удали это сообщение после отправки)")

	case stepWaitPassword:
		email := state.email
		password := msg.Text
		b.clearState(msg.Chat.ID)
		b.processLogin(msg.Chat.ID, email, password)
	}
}

func (b *Bot) processLogin(chatID int64, email, password string) {
	b.send(chatID, "Проверяю подключение к почте...")

	maxUID, err := mailer.GetMaxUID(email, password)
	if err != nil {
		b.send(chatID, fmt.Sprintf("Ошибка подключения: %v", err))
		return
	}

	if err := b.store.Set(&storage.User{
		ChatID:   chatID,
		Email:    email,
		Password: password,
		LastUID:  maxUID,
	}); err != nil {
		b.send(chatID, "Ошибка сохранения данных.")
		return
	}

	b.send(chatID, fmt.Sprintf(
		"Подключено!\nСлежу за почтой: %s\nПроверка каждые %v.",
		email, b.pollInterval,
	))
}

func (b *Bot) setState(chatID int64, state *loginState) {
	b.statesMu.Lock()
	defer b.statesMu.Unlock()
	b.states[chatID] = state
}

func (b *Bot) clearState(chatID int64) {
	b.statesMu.Lock()
	defer b.statesMu.Unlock()
	delete(b.states, chatID)
}

func (b *Bot) pollLoop() {
	ticker := time.NewTicker(b.pollInterval)
	defer ticker.Stop()
	for range ticker.C {
		for _, user := range b.store.All() {
			if err := b.checkUser(user); err != nil {
				log.Printf("[chat %d] poll error: %v", user.ChatID, err)
			}
		}
	}
}

func (b *Bot) checkUser(user storage.User) error {
	msgs, err := mailer.GetNewMessages(user.Email, user.Password, user.LastUID)
	if err != nil {
		return err
	}
	for _, m := range msgs {
		// Split attachments into sendable and too-large
		var sendable []mailer.Attachment
		var tooBig []mailer.Attachment
		for _, att := range m.Attachments {
			if att.TooLarge {
				tooBig = append(tooBig, att)
			} else {
				sendable = append(sendable, att)
			}
		}

		text := fmt.Sprintf(
			"Новое письмо!\n\nОт: %s\nТема: %s\n\nОткрыть почту: %s",
			m.From, m.Subject, mailer.WebmailURL,
		)
		for _, att := range tooBig {
			text += fmt.Sprintf(
				"\n\n⚠️ Вложение «%s» (%.1f МБ) слишком большое для отправки через Telegram — скачай напрямую с почты.",
				att.Name, float64(att.Size)/mb,
			)
		}

		if len(sendable) == 0 {
			b.send(user.ChatID, text)
		} else if len(sendable) == 1 {
			att := sendable[0]
			doc := tgbotapi.NewDocument(user.ChatID, tgbotapi.FileBytes{Name: att.Name, Bytes: att.Data})
			doc.Caption = text
			if _, err := b.api.Send(doc); err != nil {
				log.Printf("send attachment %s: %v", att.Name, err)
			}
		} else {
			// All attachments in one media group, caption on first
			media := make([]interface{}, len(sendable))
			for i, att := range sendable {
				item := tgbotapi.NewInputMediaDocument(tgbotapi.FileBytes{Name: att.Name, Bytes: att.Data})
				if i == 0 {
					item.Caption = text
				}
				media[i] = item
			}
			mg := tgbotapi.NewMediaGroup(user.ChatID, media)
			if _, err := b.api.Send(mg); err != nil {
				log.Printf("send media group: %v", err)
			}
		}
		if m.UID > user.LastUID {
			user.LastUID = m.UID
			if err := b.store.UpdateLastUID(user.ChatID, m.UID); err != nil {
				log.Printf("update uid: %v", err)
			}
		}
	}
	return nil
}

func (b *Bot) send(chatID int64, text string) {
	msg := tgbotapi.NewMessage(chatID, text)
	if _, err := b.api.Send(msg); err != nil {
		log.Printf("send to %d: %v", chatID, err)
	}
}
