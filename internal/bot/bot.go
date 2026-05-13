package bot

import (
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"tg-mail-bot/internal/lks"
	"tg-mail-bot/internal/mailer"
	"tg-mail-bot/internal/storage"
)

const (
	mb               = 1024 * 1024
	loginStateExpiry = 10 * time.Minute

	btnSchedule = "📅 Расписание"
	btnSport    = "🏃 Физкультура"
	btnMap      = "🗺 Карта"
)

type loginStep int

const (
	stepIdle loginStep = iota
	stepWaitEmail
	stepWaitPassword
	stepWaitGroup
)

type loginState struct {
	step      loginStep
	email     string
	createdAt time.Time
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
	b := &Bot{
		api:          api,
		store:        store,
		pollInterval: pollInterval,
		states:       make(map[int64]*loginState),
	}
	go b.expireStatesLoop()
	return b, nil
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

// mainKeyboard returns the persistent reply keyboard shown to all users.
func mainKeyboard() tgbotapi.ReplyKeyboardMarkup {
	return tgbotapi.NewReplyKeyboard(
		tgbotapi.NewKeyboardButtonRow(
			tgbotapi.NewKeyboardButton(btnSchedule),
			tgbotapi.NewKeyboardButton(btnSport),
		),
		tgbotapi.NewKeyboardButtonRow(
			tgbotapi.NewKeyboardButton(btnMap),
		),
	)
}

func (b *Bot) sendWithKeyboard(chatID int64, text string) {
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ReplyMarkup = mainKeyboard()
	if _, err := b.api.Send(msg); err != nil {
		log.Printf("send: %v", err)
	}
}

func (b *Bot) send(chatID int64, text string) {
	msg := tgbotapi.NewMessage(chatID, text)
	if _, err := b.api.Send(msg); err != nil {
		log.Printf("send: %v", err)
	}
}

func (b *Bot) handle(msg *tgbotapi.Message) {
	if msg.IsCommand() {
		b.clearState(msg.Chat.ID)
		switch msg.Command() {
		case "start":
			b.sendWithKeyboard(msg.Chat.ID,
				"Привет! Я уведомляю о новых письмах на student.bmstu.ru.\n\n"+
					"Команды:\n"+
					"/login — подключить почту\n"+
					"/status — проверить статус\n"+
					"/logout — отключить уведомления",
			)
		case "login":
			b.setState(msg.Chat.ID, &loginState{step: stepWaitEmail, createdAt: time.Now()})
			b.send(msg.Chat.ID,
				"BMSTU Mail Bot — бот для переадресации писем с бауманской почты.\n\n"+
					"Для подключения нужна полная бауманская почта (student.bmstu.ru) и пароль от неё.\n\n"+
					"Введи адрес почты:",
			)
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
	// Reply keyboard buttons
	switch msg.Text {
	case btnSchedule:
		b.handleSchedule(msg.Chat.ID)
		return
	case btnSport:
		b.handleSport(msg.Chat.ID)
		return
	case btnMap:
		b.handleMap(msg.Chat.ID)
		return
	}

	// Login / group setup flow
	b.statesMu.Lock()
	state := b.states[msg.Chat.ID]
	b.statesMu.Unlock()

	if state == nil {
		return
	}

	switch state.step {
	case stepWaitEmail:
		email := strings.TrimSpace(msg.Text)
		if !strings.Contains(email, "@") || !strings.HasSuffix(email, "student.bmstu.ru") {
			b.send(msg.Chat.ID, "Неверный формат. Введи адрес вида имя@student.bmstu.ru:")
			return
		}
		state.email = email
		state.step = stepWaitPassword
		b.send(msg.Chat.ID, "Теперь введи пароль (сообщение будет удалено автоматически):")

	case stepWaitPassword:
		email := state.email
		password := msg.Text
		b.clearState(msg.Chat.ID)

		del := tgbotapi.NewDeleteMessage(msg.Chat.ID, msg.MessageID)
		if _, err := b.api.Send(del); err != nil {
			log.Printf("delete password msg: %v", err)
		}

		b.processLogin(msg.Chat.ID, email, password)

	case stepWaitGroup:
		b.processGroupSetup(msg.Chat.ID, msg.Text)
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

	b.sendWithKeyboard(chatID, fmt.Sprintf(
		"Подключено!\nСлежу за почтой: %s\nПроверка каждые %v.",
		email, b.pollInterval,
	))
}

// --- Schedule ---

func (b *Bot) handleSchedule(chatID int64) {
	user, ok := b.store.Get(chatID)
	if !ok {
		b.send(chatID, "Сначала подключи почту через /login")
		return
	}
	if user.GroupUUID == "" {
		b.setState(chatID, &loginState{step: stepWaitGroup, createdAt: time.Now()})
		b.send(chatID, "Введи название своей группы (например: ИУ7-21Б):")
		return
	}
	b.send(chatID, "Загружаю расписание...")
	text, err := lks.GetTodaySchedule(user.GroupUUID)
	if err != nil {
		b.send(chatID, fmt.Sprintf("Ошибка загрузки расписания: %v", err))
		return
	}
	b.send(chatID, text)
}

func (b *Bot) processGroupSetup(chatID int64, groupName string) {
	b.clearState(chatID)
	b.send(chatID, "Ищу группу...")

	uuid, err := lks.FindGroupUUID(groupName)
	if err != nil {
		b.send(chatID, fmt.Sprintf("%v\nПроверь название и попробуй ещё раз через кнопку «Расписание».", err))
		return
	}
	if err := b.store.UpdateGroup(chatID, groupName, uuid); err != nil {
		b.send(chatID, "Ошибка сохранения группы.")
		return
	}

	text, err := lks.GetTodaySchedule(uuid)
	if err != nil {
		b.send(chatID, fmt.Sprintf("Группа сохранена. Ошибка загрузки расписания: %v", err))
		return
	}
	b.send(chatID, text)
}

// --- Sport ---

func (b *Bot) handleSport(chatID int64) {
	user, ok := b.store.Get(chatID)
	if !ok {
		b.send(chatID, "Сначала подключи почту через /login")
		return
	}
	b.send(chatID, "Загружаю данные физкультуры...")

	c, err := lks.NewClient(user.Email, user.Password)
	if err != nil {
		b.send(chatID, fmt.Sprintf("Ошибка входа в ЛКС: %v", err))
		return
	}
	text, err := lks.GetSportAttendance(c)
	if err != nil {
		b.send(chatID, fmt.Sprintf("Ошибка загрузки данных: %v", err))
		return
	}
	b.send(chatID, text)
}

// --- Map ---

func (b *Bot) handleMap(chatID int64) {
	b.send(chatID, "🗺 Карта университета будет добавлена в ближайшее время.")
}

// --- State management ---

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

func (b *Bot) expireStatesLoop() {
	ticker := time.NewTicker(loginStateExpiry)
	defer ticker.Stop()
	for range ticker.C {
		b.statesMu.Lock()
		now := time.Now()
		for chatID, s := range b.states {
			if now.Sub(s.createdAt) > loginStateExpiry {
				delete(b.states, chatID)
			}
		}
		b.statesMu.Unlock()
	}
}

// --- Mail polling ---

func (b *Bot) pollLoop() {
	ticker := time.NewTicker(b.pollInterval)
	defer ticker.Stop()
	for range ticker.C {
		for _, user := range b.store.All() {
			if err := b.checkUser(user); err != nil {
				log.Printf("poll error: %v", err)
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
