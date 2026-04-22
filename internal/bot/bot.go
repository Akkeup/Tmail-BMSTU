package bot

import (
	"fmt"
	"log"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"tg-mail-bot/internal/mailer"
	"tg-mail-bot/internal/storage"
)

type Bot struct {
	api          *tgbotapi.BotAPI
	store        *storage.Storage
	pollInterval time.Duration
}

func New(token string, store *storage.Storage, pollInterval time.Duration) (*Bot, error) {
	api, err := tgbotapi.NewBotAPI(token)
	if err != nil {
		return nil, err
	}
	log.Printf("Authorized as @%s", api.Self.UserName)
	return &Bot{api: api, store: store, pollInterval: pollInterval}, nil
}

func (b *Bot) Run() {
	go b.pollLoop()

	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60
	for update := range b.api.GetUpdatesChan(u) {
		if update.Message == nil || !update.Message.IsCommand() {
			continue
		}
		b.handle(update.Message)
	}
}

func (b *Bot) handle(msg *tgbotapi.Message) {
	switch msg.Command() {
	case "start":
		b.send(msg.Chat.ID,
			"Привет! Я уведомляю о новых письмах на student.bmstu.ru.\n\n"+
				"Команды:\n"+
				"/login почта пароль — подключить почту\n"+
				"/status — проверить статус подключения\n"+
				"/logout — отключить уведомления",
		)

	case "login":
		b.handleLogin(msg)

	case "logout":
		b.handleLogout(msg)

	case "status":
		b.handleStatus(msg)
	}
}

func (b *Bot) handleLogin(msg *tgbotapi.Message) {
	args := strings.Fields(msg.CommandArguments())
	if len(args) != 2 {
		b.send(msg.Chat.ID, "Использование:\n/login почта@student.bmstu.ru пароль")
		return
	}
	email, password := args[0], args[1]

	b.send(msg.Chat.ID, "Проверяю подключение к почте...")

	// Single session: connect + get baseline UID
	maxUID, err := mailer.GetMaxUID(email, password)
	if err != nil {
		b.send(msg.Chat.ID, fmt.Sprintf("Ошибка подключения: %v", err))
		return
	}

	if err := b.store.Set(&storage.User{
		ChatID:   msg.Chat.ID,
		Email:    email,
		Password: password,
		LastUID:  maxUID,
	}); err != nil {
		b.send(msg.Chat.ID, "Ошибка сохранения данных.")
		return
	}

	b.send(msg.Chat.ID, fmt.Sprintf(
		"Подключено!\nСлежу за почтой: %s\nПроверка каждые %v.",
		email, b.pollInterval,
	))
}

func (b *Bot) handleLogout(msg *tgbotapi.Message) {
	_ = b.store.Delete(msg.Chat.ID)
	b.send(msg.Chat.ID, "Отключено. Уведомления остановлены.")
}

func (b *Bot) handleStatus(msg *tgbotapi.Message) {
	user, ok := b.store.Get(msg.Chat.ID)
	if !ok {
		b.send(msg.Chat.ID, "Не подключено. Используйте /login почта пароль")
		return
	}
	b.send(msg.Chat.ID, fmt.Sprintf(
		"Активно\nПочта: %s\nПроверка каждые %v.",
		user.Email, b.pollInterval,
	))
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
		b.send(user.ChatID, fmt.Sprintf(
			"Новое письмо!\n\nТема: %s\n\nОткрыть почту: %s",
			m.Subject, mailer.WebmailURL,
		))

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
