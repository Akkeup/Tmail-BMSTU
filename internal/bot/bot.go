package bot

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
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

	// Callback data for inline keyboard buttons
	cbMenu     = "menu"
	cbSport    = "sport"
	cbMap      = "map"
	cbOtherDay = "other_day"
	cbSettings = "settings"

	navText = "Бауманец — выбери раздел:"
)

type loginStep int

const (
	stepIdle loginStep = iota
	stepWaitEmail
	stepWaitPassword
	stepWaitGroup
	stepWaitDate
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
	navMsgs      map[int64]int // chatID → nav message ID
	navMu        sync.Mutex
}

func New(token string, store *storage.Storage, pollInterval time.Duration) (*Bot, error) {
	api, err := tgbotapi.NewBotAPIWithClient(token, tgbotapi.APIEndpoint, telegramHTTPClient())
	if err != nil {
		return nil, err
	}
	log.Printf("Authorized as @%s", api.Self.UserName)
	b := &Bot{
		api:          api,
		store:        store,
		pollInterval: pollInterval,
		states:       make(map[int64]*loginState),
		navMsgs:      make(map[int64]int),
	}
	go b.expireStatesLoop()
	return b, nil
}

func telegramHTTPClient() *http.Client {
	dialer := &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		ForceAttemptHTTP2:   false,
		TLSHandshakeTimeout: 20 * time.Second,
		// Telegram long polling waits up to 60 seconds for getUpdates.
		ResponseHeaderTimeout: 90 * time.Second,
		IdleConnTimeout:       90 * time.Second,
		DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
			// The default Go resolver may select an unreachable IPv6 route in
			// the current environment while curl reaches Telegram over IPv4.
			return dialer.DialContext(ctx, "tcp4", addr)
		},
	}
	return &http.Client{Transport: transport, Timeout: 120 * time.Second}
}

func (b *Bot) Run() {
	go b.pollLoop()

	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60
	for update := range b.api.GetUpdatesChan(u) {
		if update.Message != nil {
			b.handle(update.Message)
		} else if update.CallbackQuery != nil {
			b.handleCallback(update.CallbackQuery)
		}
	}
}

// --- Keyboards ---

func mainKeyboard() tgbotapi.InlineKeyboardMarkup {
	return tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("📅 Другой день", cbOtherDay),
			tgbotapi.NewInlineKeyboardButtonData("⚙ Настройки", cbSettings),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🏃 Физкультура", cbSport),
			tgbotapi.NewInlineKeyboardButtonData("🗺 Карта", cbMap),
		),
	)
}

func scheduleKeyboard() tgbotapi.ReplyKeyboardMarkup {
	return tgbotapi.NewReplyKeyboard(
		tgbotapi.NewKeyboardButtonRow(tgbotapi.NewKeyboardButton("📅 Расписание")),
		tgbotapi.NewKeyboardButtonRow(
			tgbotapi.NewKeyboardButton("📅 Сегодня"),
			tgbotapi.NewKeyboardButton("📆 Завтра"),
		),
		tgbotapi.NewKeyboardButtonRow(
			tgbotapi.NewKeyboardButton("🗓 Неделя"),
			tgbotapi.NewKeyboardButton("📋 Меню"),
		),
	)
}

func backKeyboard() tgbotapi.InlineKeyboardMarkup {
	return tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("← Меню", cbMenu),
		),
	)
}

// --- Nav message helpers ---

const startText = "👋 Привет! Я Бауманец — бот для студентов МГТУ им. Баумана.\n\n" +
	"Пока умею:\n" +
	"• 📬 Уведомлять о новых письмах на student.bmstu.ru\n" +
	"• 📅 Показывать расписание твоей группы\n" +
	"• 🏃 Отслеживать посещения физкультуры\n\n" +
	"Команды:\n" +
	"/login — подключить почту\n" +
	"/status — проверить статус подключения\n" +
	"/logout — отключить уведомления\n\n" +
	"Бауманец — выбери раздел:"

// sendNav sends a new nav message and remembers its ID.
func (b *Bot) sendNav(chatID int64) {
	msg := tgbotapi.NewMessage(chatID, startText)
	msg.ReplyMarkup = scheduleKeyboard()
	sent, err := b.api.Send(msg)
	if err != nil {
		log.Printf("sendNav: %v", err)
		return
	}
	b.navMu.Lock()
	b.navMsgs[chatID] = sent.MessageID
	b.navMu.Unlock()
}

// editNav edits the nav message to new text + keyboard. Falls back to sending if not found.
func (b *Bot) editNav(chatID int64, text string, kb tgbotapi.InlineKeyboardMarkup) {
	b.navMu.Lock()
	msgID := b.navMsgs[chatID]
	b.navMu.Unlock()

	if msgID == 0 {
		b.sendNav(chatID)
		return
	}
	edit := tgbotapi.NewEditMessageText(chatID, msgID, text)
	edit.ReplyMarkup = &kb
	if _, err := b.api.Send(edit); err != nil {
		// Nav message was deleted — send a fresh one
		log.Printf("editNav (fallback to send): %v", err)
		b.navMu.Lock()
		b.navMsgs[chatID] = 0
		b.navMu.Unlock()
		b.sendNav(chatID)
	}
}

// editNavFromCallback edits the message that was the source of the callback.
func (b *Bot) editNavFromCallback(cq *tgbotapi.CallbackQuery, text string, kb tgbotapi.InlineKeyboardMarkup) {
	chatID := cq.Message.Chat.ID
	msgID := cq.Message.MessageID

	b.navMu.Lock()
	b.navMsgs[chatID] = msgID
	b.navMu.Unlock()

	edit := tgbotapi.NewEditMessageText(chatID, msgID, text)
	edit.ReplyMarkup = &kb
	if _, err := b.api.Send(edit); err != nil {
		log.Printf("editNavFromCallback: %v", err)
	}
}

// --- Commands ---

func (b *Bot) handle(msg *tgbotapi.Message) {
	if msg.IsCommand() {
		b.clearState(msg.Chat.ID)
		switch msg.Command() {
		case "start":
			b.sendNav(msg.Chat.ID)

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
	if b.handleScheduleButton(msg) {
		return
	}

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
		b.clearState(msg.Chat.ID)
		b.processGroupSetup(msg.Chat.ID, msg.Text)

	case stepWaitDate:
		b.clearState(msg.Chat.ID)
		date, err := time.Parse("02.01.2006", strings.TrimSpace(msg.Text))
		if err != nil {
			b.send(msg.Chat.ID, "Не удалось распознать дату. Используй формат ДД.ММ.ГГГГ, например 12.11.2026")
			return
		}
		b.showDaySchedule(msg.Chat.ID, date)
	}
}

func (b *Bot) handleScheduleButton(msg *tgbotapi.Message) bool {
	var date *time.Time
	switch strings.TrimSpace(msg.Text) {
	case "📅 Расписание", "🗓 Неделя":
		b.showWeekSchedule(msg.Chat.ID)
		return true
	case "📅 Сегодня":
		now := time.Now()
		date = &now
	case "📆 Завтра":
		tomorrow := time.Now().AddDate(0, 0, 1)
		date = &tomorrow
	case "📋 Меню":
		b.sendMenu(msg.Chat.ID)
		return true
	default:
		return false
	}
	b.showDaySchedule(msg.Chat.ID, *date)
	return true
}

func (b *Bot) sendMenu(chatID int64) {
	msg := tgbotapi.NewMessage(chatID,
		"📋 Меню\n\nВыбери нужный раздел:")
	msg.ReplyMarkup = mainKeyboard()
	if _, err := b.api.Send(msg); err != nil {
		log.Printf("sendMenu: %v", err)
	}
}

func (b *Bot) showWeekSchedule(chatID int64) {
	user, ok := b.store.Get(chatID)
	if !ok {
		b.send(chatID, "Сначала подключи почту через /login")
		return
	}
	if user.GroupUUID == "" {
		b.send(chatID, "Сначала укажи группу через 📋 Меню → ⚙ Настройки")
		return
	}
	text, err := lks.GetWeekSchedule(user.GroupUUID)
	if err != nil {
		b.send(chatID, fmt.Sprintf("Ошибка загрузки расписания: %v", err))
		return
	}
	b.send(chatID, text)
}

func (b *Bot) showDaySchedule(chatID int64, date time.Time) {
	user, ok := b.store.Get(chatID)
	if !ok {
		b.send(chatID, "Сначала подключи почту через /login")
		return
	}
	if user.GroupUUID == "" {
		b.send(chatID, "Сначала укажи группу через 📋 Меню → ⚙ Настройки")
		return
	}
	text, err := lks.GetDaySchedule(user.GroupUUID, date)
	if err != nil {
		b.send(chatID, fmt.Sprintf("Ошибка загрузки расписания: %v", err))
		return
	}
	b.send(chatID, text)
}

// --- Callback handler ---

func (b *Bot) handleCallback(cq *tgbotapi.CallbackQuery) {
	// Acknowledge the callback to remove the loading spinner
	b.api.Send(tgbotapi.NewCallback(cq.ID, ""))

	switch cq.Data {
	case cbMenu:
		b.editNavFromCallback(cq, navText, mainKeyboard())

	case cbOtherDay:
		b.askOtherDay(cq)

	case cbSettings:
		b.showSettings(cq)

	case cbSport:
		b.callbackSport(cq)

	case cbMap:
		b.editNavFromCallback(cq, "🗺 Карта университета будет добавлена в ближайшее время.", backKeyboard())
	}
}

// --- Login ---

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

	b.send(chatID, fmt.Sprintf("Подключено! Слежу за почтой: %s\nПроверка каждые %v.", email, b.pollInterval))
	b.sendNav(chatID)
}

// --- Schedule ---

func (b *Bot) processGroupSetup(chatID int64, groupName string) {
	uuid, err := lks.FindGroupUUID(groupName)
	if err != nil {
		b.editNav(chatID, fmt.Sprintf("📅 Расписание\n\n%v\n\nПроверь название и нажми кнопку снова.", err), backKeyboard())
		return
	}
	if err := b.store.UpdateGroup(chatID, groupName, uuid); err != nil {
		b.editNav(chatID, "Ошибка сохранения группы.", backKeyboard())
		return
	}

	text, err := lks.GetWeekSchedule(uuid)
	if err != nil {
		b.editNav(chatID, fmt.Sprintf("Ошибка загрузки расписания: %v", err), backKeyboard())
		return
	}
	b.editNav(chatID, text, backKeyboard())
}

// --- Sport ---

func (b *Bot) callbackSport(cq *tgbotapi.CallbackQuery) {
	user, ok := b.store.Get(cq.Message.Chat.ID)
	if !ok {
		b.editNavFromCallback(cq, "Сначала подключи почту через /login", backKeyboard())
		return
	}
	b.editNavFromCallback(cq, "🏃 Загружаю данные физкультуры...", backKeyboard())
	client, err := lks.NewClient(user.Email, user.Password)
	if err != nil {
		b.editNav(cq.Message.Chat.ID, fmt.Sprintf("Ошибка загрузки физкультуры: %v", err), backKeyboard())
		return
	}
	text, err := lks.GetSportAttendance(client)
	if err != nil {
		b.editNav(cq.Message.Chat.ID, fmt.Sprintf("Ошибка загрузки физкультуры: %v", err), backKeyboard())
		return
	}
	b.editNav(cq.Message.Chat.ID, text, backKeyboard())
}

func (b *Bot) askOtherDay(cq *tgbotapi.CallbackQuery) {
	chatID := cq.Message.Chat.ID
	b.setState(chatID, &loginState{step: stepWaitDate, createdAt: time.Now()})
	b.editNavFromCallback(cq,
		"📅 Другой день\n\nВведи дату в формате ДД.ММ.ГГГГ:",
		backKeyboard())
}

func (b *Bot) showSettings(cq *tgbotapi.CallbackQuery) {
	user, ok := b.store.Get(cq.Message.Chat.ID)
	if !ok {
		b.editNavFromCallback(cq, "Сначала подключи почту через /login", backKeyboard())
		return
	}
	text := fmt.Sprintf("⚙ Настройки\n\nТекущая группа: %s\n\nВведи новую группу следующим сообщением.", user.Group)
	b.setState(cq.Message.Chat.ID, &loginState{step: stepWaitGroup, createdAt: time.Now()})
	b.editNavFromCallback(cq, text, backKeyboard())
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

func (b *Bot) send(chatID int64, text string) {
	msg := tgbotapi.NewMessage(chatID, text)
	if _, err := b.api.Send(msg); err != nil {
		log.Printf("send: %v", err)
	}
}
