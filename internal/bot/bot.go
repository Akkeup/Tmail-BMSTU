package bot

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"tg-mail-bot/internal/lks"
	"tg-mail-bot/internal/mailer"
	"tg-mail-bot/internal/storage"
)

var inlineImageMarkerPattern = regexp.MustCompile(`(?i)\[img\]\s*https://[^\s]+(?:\s*[).][^\s]*)*`)

const (
	mailPageSize     = 4
	mailFetchLimit   = 6
	mailCacheTTL     = 2 * time.Minute
	mailDetailTTL    = 15 * time.Minute
	mailDetailBudget = 32 * 1024 * 1024
	loginStateExpiry = 10 * time.Minute

	// Callback data for inline keyboard buttons
	cbMenu     = "menu"
	cbMail     = "mail"
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
	chatLocks    map[int64]*sync.Mutex
	chatLocksMu  sync.Mutex
	updateSlots  chan struct{}
	pollMu       sync.Mutex
	mailCache    map[int64]mailCacheEntry
	mailCacheMu  sync.RWMutex
	mailDetails  map[int64]map[uint32]mailDetailCacheEntry
	mailDetailMu sync.Mutex
	mailWarming  map[int64]bool
}

type mailCacheEntry struct {
	messages  []mailer.Message
	expiresAt time.Time
}

type mailDetailCacheEntry struct {
	message   mailer.Message
	expiresAt time.Time
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
		chatLocks:    make(map[int64]*sync.Mutex),
		updateSlots:  make(chan struct{}, 16),
		mailCache:    make(map[int64]mailCacheEntry),
		mailDetails:  make(map[int64]map[uint32]mailDetailCacheEntry),
		mailWarming:  make(map[int64]bool),
	}
	go b.expireStatesLoop()
	return b, nil
}

func telegramHTTPClient() *http.Client {
	dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		ForceAttemptHTTP2:   false,
		TLSHandshakeTimeout: 20 * time.Second,
		// Telegram long polling waits up to 60 seconds for getUpdates.
		ResponseHeaderTimeout: 75 * time.Second,
		IdleConnTimeout:       90 * time.Second,
		DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
			// The default Go resolver may select an unreachable IPv6 route in
			// the current environment while curl reaches Telegram over IPv4.
			return dialer.DialContext(ctx, "tcp4", addr)
		},
	}
	return &http.Client{
		Transport: retryTelegramTransport{base: transport, attempts: 2},
		Timeout:   90 * time.Second,
	}
}

type retryTelegramTransport struct {
	base     http.RoundTripper
	attempts int
}

func (t retryTelegramTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	attempts := t.attempts
	if attempts < 1 {
		attempts = 1
	}
	// telegram-bot-api retries getUpdates itself. Retrying the 60-second
	// long poll here can block update delivery for several minutes.
	if strings.HasSuffix(req.URL.Path, "/getUpdates") {
		attempts = 1
	}
	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		current := req
		if attempt > 0 {
			if req.GetBody == nil {
				return nil, lastErr
			}
			body, err := req.GetBody()
			if err != nil {
				return nil, lastErr
			}
			current = req.Clone(req.Context())
			current.Body = body
		}

		resp, err := t.base.RoundTrip(current)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		if !isRetryableTelegramError(err) || attempt+1 == attempts {
			break
		}
		time.Sleep(time.Duration(attempt+1) * 500 * time.Millisecond)
	}
	return nil, lastErr
}

func isRetryableTelegramError(err error) bool {
	if err == nil {
		return false
	}
	if networkErr, ok := err.(net.Error); ok {
		return networkErr.Timeout() || networkErr.Temporary()
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "tls handshake timeout") ||
		strings.Contains(message, "connection reset") ||
		strings.Contains(message, "unexpected eof") ||
		strings.Contains(message, "eof")
}

func (b *Bot) Run() {
	go b.pollLoop()

	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60
	for update := range b.api.GetUpdatesChan(u) {
		b.dispatchUpdate(update)
	}
}

// dispatchUpdate prevents a slow external service from blocking Telegram's
// update reader. Updates from the same chat remain ordered, while different
// chats can be processed independently.
func (b *Bot) dispatchUpdate(update tgbotapi.Update) {
	chatID := int64(0)
	if update.Message != nil {
		chatID = update.Message.Chat.ID
	} else if update.CallbackQuery != nil && update.CallbackQuery.Message != nil {
		chatID = update.CallbackQuery.Message.Chat.ID
	}

	go func() {
		b.updateSlots <- struct{}{}
		defer func() { <-b.updateSlots }()

		if chatID != 0 {
			b.chatLocksMu.Lock()
			lock := b.chatLocks[chatID]
			if lock == nil {
				lock = &sync.Mutex{}
				b.chatLocks[chatID] = lock
			}
			b.chatLocksMu.Unlock()
			lock.Lock()
			defer lock.Unlock()
		}

		if update.Message != nil {
			b.handle(update.Message)
		} else if update.CallbackQuery != nil {
			b.handleCallback(update.CallbackQuery)
		}
	}()
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
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("📬 Почта", cbMail),
		),
	)
}

func scheduleKeyboard() tgbotapi.ReplyKeyboardMarkup {
	return tgbotapi.NewReplyKeyboard(
		tgbotapi.NewKeyboardButtonRow(tgbotapi.NewKeyboardButton("📬 Почта")),
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
	if _, err := b.api.Send(edit); err != nil && !isHarmlessTelegramEditError(err) {
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

	if cq.Message.Photo != nil || cq.Message.Document != nil {
		b.replaceMediaCallback(cq, text, kb)
		return
	}
	edit := tgbotapi.NewEditMessageText(chatID, msgID, text)
	edit.ReplyMarkup = &kb
	if _, err := b.api.Send(edit); err != nil && !isHarmlessTelegramEditError(err) {
		log.Printf("editNavFromCallback: %v", err)
	}
}

func (b *Bot) replaceMediaCallback(cq *tgbotapi.CallbackQuery, text string, kb tgbotapi.InlineKeyboardMarkup) {
	chatID := cq.Message.Chat.ID
	if _, err := b.api.Request(tgbotapi.NewDeleteMessage(chatID, cq.Message.MessageID)); err != nil {
		log.Printf("delete media callback message: %v", err)
	}
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ReplyMarkup = kb
	sent, err := b.api.Send(msg)
	if err != nil {
		log.Printf("replace media callback: %v", err)
		return
	}
	b.navMu.Lock()
	b.navMsgs[chatID] = sent.MessageID
	b.navMu.Unlock()
}

func isHarmlessTelegramEditError(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "message is not modified") ||
		strings.Contains(message, "there is no text in the message to edit")
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
	if state == nil {
		b.statesMu.Unlock()
		return
	}
	step := state.step
	if step == stepWaitEmail {
		email := strings.TrimSpace(msg.Text)
		if strings.Contains(email, "@") && strings.HasSuffix(email, "student.bmstu.ru") {
			state.email = email
			state.step = stepWaitPassword
		}
	}
	email := state.email
	b.statesMu.Unlock()

	switch step {
	case stepWaitEmail:
		email := strings.TrimSpace(msg.Text)
		if !strings.Contains(email, "@") || !strings.HasSuffix(email, "student.bmstu.ru") {
			b.send(msg.Chat.ID, "Неверный формат. Введи адрес вида имя@student.bmstu.ru:")
			return
		}
		b.send(msg.Chat.ID, "Теперь введи пароль (сообщение будет удалено автоматически):")

	case stepWaitPassword:
		password := msg.Text
		b.clearState(msg.Chat.ID)

		del := tgbotapi.NewDeleteMessage(msg.Chat.ID, msg.MessageID)
		if _, err := b.api.Request(del); err != nil {
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
	case "📬 Почта":
		b.showMailPage(msg.Chat.ID, 0)
		return true
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

func (b *Bot) showMailPage(chatID int64, page int) {
	user, ok := b.store.Get(chatID)
	if !ok {
		b.send(chatID, "Сначала подключи почту через /login")
		return
	}
	messages, err := b.loadMailMessages(chatID, user)
	if err != nil {
		b.send(chatID, fmt.Sprintf("Ошибка загрузки почты: %v", err))
		return
	}
	text, keyboard, ok := renderMailPage(messages, page)
	if !ok {
		b.send(chatID, "📬 Входящих писем пока нет.")
		return
	}
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ReplyMarkup = keyboard
	if _, err := b.api.Send(msg); err != nil {
		log.Printf("sendMailPage: %v", err)
	}
}

func (b *Bot) editMailPage(cq *tgbotapi.CallbackQuery, page int) {
	user, ok := b.store.Get(cq.Message.Chat.ID)
	if !ok {
		b.editNavFromCallback(cq, "Сначала подключи почту через /login", backKeyboard())
		return
	}
	messages, err := b.loadMailMessages(cq.Message.Chat.ID, user)
	if err != nil {
		b.editNavFromCallback(cq, fmt.Sprintf("Ошибка загрузки почты: %v", err), backKeyboard())
		return
	}
	text, keyboard, ok := renderMailPage(messages, page)
	if !ok {
		b.editNavFromCallback(cq, "📬 Входящих писем пока нет.", backKeyboard())
		return
	}
	if cq.Message.Photo != nil || cq.Message.Document != nil {
		b.replaceMediaCallback(cq, text, keyboard)
		return
	}
	edit := tgbotapi.NewEditMessageText(cq.Message.Chat.ID, cq.Message.MessageID, text)
	edit.ReplyMarkup = &keyboard
	if _, err := b.api.Send(edit); err != nil && !isHarmlessTelegramEditError(err) {
		log.Printf("editMailPage: %v", err)
	}
}

func (b *Bot) loadMailMessages(chatID int64, user storage.User) ([]mailer.Message, error) {
	now := time.Now()
	b.mailCacheMu.RLock()
	cached, hasCache := b.mailCache[chatID]
	b.mailCacheMu.RUnlock()
	if hasCache && now.Before(cached.expiresAt) {
		return cached.messages, nil
	}
	if stored, cachedAt, cacheErr := b.store.GetMailCache(chatID, mailFetchLimit); cacheErr == nil && len(stored) > 0 && now.Sub(cachedAt) < mailCacheTTL {
		messages := storageMailMessages(stored)
		b.mailCacheMu.Lock()
		b.mailCache[chatID] = mailCacheEntry{messages: messages, expiresAt: now.Add(mailCacheTTL)}
		b.mailCacheMu.Unlock()
		return messages, nil
	}

	messages, err := mailer.GetInboxMessages(user.Email, user.Password)
	if err != nil {
		if stored, _, cacheErr := b.store.GetMailCache(chatID, mailFetchLimit); cacheErr == nil && len(stored) > 0 {
			return storageMailMessages(stored), nil
		}
		if hasCache && len(cached.messages) > 0 {
			log.Printf("mail refresh failed, using cached list chat=%d: %v", chatID, err)
			return cached.messages, nil
		}
		return nil, err
	}
	_ = b.store.SaveMailCache(chatID, mailerMailMessages(messages))
	b.mailCacheMu.Lock()
	b.mailCache[chatID] = mailCacheEntry{messages: messages, expiresAt: now.Add(mailCacheTTL)}
	b.mailCacheMu.Unlock()
	return messages, nil
}

func storageMailMessages(messages []storage.MailMessage) []mailer.Message {
	result := make([]mailer.Message, 0, len(messages))
	for _, message := range messages {
		result = append(result, mailer.Message{
			UID: message.UID, Subject: message.Subject, From: message.From,
			Date: message.Date, ContentType: message.ContentType,
		})
	}
	return result
}

func mailerMailMessages(messages []mailer.Message) []storage.MailMessage {
	result := make([]storage.MailMessage, 0, len(messages))
	for _, message := range messages {
		result = append(result, storage.MailMessage{
			UID: message.UID, Subject: message.Subject, From: message.From,
			Date: message.Date, ContentType: message.ContentType,
		})
	}
	return result
}

func (b *Bot) invalidateMailCache(chatID int64) {
	b.mailCacheMu.Lock()
	delete(b.mailCache, chatID)
	b.mailCacheMu.Unlock()
	b.mailDetailMu.Lock()
	delete(b.mailDetails, chatID)
	delete(b.mailWarming, chatID)
	b.mailDetailMu.Unlock()
}

func (b *Bot) cachedMailDetail(chatID int64, uid uint32) (mailer.Message, bool) {
	b.mailDetailMu.Lock()
	defer b.mailDetailMu.Unlock()
	byUID := b.mailDetails[chatID]
	entry, ok := byUID[uid]
	if !ok {
		return mailer.Message{}, false
	}
	if time.Now().After(entry.expiresAt) {
		delete(byUID, uid)
		return mailer.Message{}, false
	}
	if !mailDetailHasAllInlineImages(entry.message) {
		delete(byUID, uid)
		return mailer.Message{}, false
	}
	return entry.message, true
}

func (b *Bot) cacheMailDetail(chatID int64, message mailer.Message) {
	if !mailDetailHasAllInlineImages(message) {
		return
	}
	size := len(message.Body)
	for _, attachment := range message.Attachments {
		size += len(attachment.Data)
	}
	if size > mailDetailBudget {
		return
	}
	b.mailDetailMu.Lock()
	defer b.mailDetailMu.Unlock()
	if b.mailDetails[chatID] == nil {
		b.mailDetails[chatID] = make(map[uint32]mailDetailCacheEntry)
	}
	b.mailDetails[chatID][message.UID] = mailDetailCacheEntry{
		message:   message,
		expiresAt: time.Now().Add(mailDetailTTL),
	}
}

func mailDetailHasAllInlineImages(message mailer.Message) bool {
	inlineCount := len(inlineImageMarkerPattern.FindAllString(message.Body, -1))
	if inlineCount == 0 {
		return true
	}
	imageCount := 0
	for _, attachment := range message.Attachments {
		if strings.HasPrefix(strings.ToLower(attachment.ContentType), "image/") && len(attachment.Data) > 0 {
			imageCount++
		}
	}
	return imageCount >= inlineCount
}

func (b *Bot) loadMailDetail(chatID int64, user storage.User, uid uint32) (mailer.Message, error) {
	if message, ok := b.cachedMailDetail(chatID, uid); ok {
		return message, nil
	}
	message, err := mailer.GetMessage(user.Email, user.Password, uid)
	if err != nil {
		return mailer.Message{}, err
	}
	b.cacheMailDetail(chatID, message)
	return message, nil
}

func (b *Bot) warmMailDetails(chatID int64, user storage.User, messages []mailer.Message) {
	limit := mailPageSize
	if len(messages) < limit {
		limit = len(messages)
	}
	b.mailDetailMu.Lock()
	if b.mailWarming[chatID] || limit == 0 {
		b.mailDetailMu.Unlock()
		return
	}
	b.mailWarming[chatID] = true
	b.mailDetailMu.Unlock()

	go func() {
		defer func() {
			b.mailDetailMu.Lock()
			delete(b.mailWarming, chatID)
			b.mailDetailMu.Unlock()
		}()
		for i := 0; i < limit; i++ {
			if _, ok := b.cachedMailDetail(chatID, messages[i].UID); ok {
				continue
			}
			message, err := mailer.GetMessage(user.Email, user.Password, messages[i].UID)
			if err != nil {
				log.Printf("mail cache warm chat=%d uid=%d: %v", chatID, messages[i].UID, err)
				continue
			}
			b.cacheMailDetail(chatID, message)
		}
	}()
}

func renderMailPage(messages []mailer.Message, page int) (string, tgbotapi.InlineKeyboardMarkup, bool) {
	if len(messages) == 0 {
		return "", tgbotapi.InlineKeyboardMarkup{}, false
	}
	totalPages := (len(messages) + mailPageSize - 1) / mailPageSize
	if page < 0 {
		page = 0
	}
	if page >= totalPages {
		page = totalPages - 1
	}
	start := page * mailPageSize
	end := start + mailPageSize
	if end > len(messages) {
		end = len(messages)
	}

	text := fmt.Sprintf("📬 Почта\n\nСтраница %d из %d", page+1, totalPages)
	rows := make([][]tgbotapi.InlineKeyboardButton, 0, mailPageSize+2)
	for i := start; i < end; i++ {
		m := messages[i]
		label := truncateMailText(m.Subject, 42)
		if m.From != "" {
			label += " · " + truncateMailText(m.From, 24)
		}
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData(label, fmt.Sprintf("mail_open:%d:page:%d", m.UID, page)),
		))
	}
	navigation := make([]tgbotapi.InlineKeyboardButton, 0, 3)
	if page > 0 {
		navigation = append(navigation, tgbotapi.NewInlineKeyboardButtonData("←", fmt.Sprintf("mail_page:%d", page-1)))
	}
	navigation = append(navigation, tgbotapi.NewInlineKeyboardButtonData("📋 Меню", cbMenu))
	if page+1 < totalPages {
		navigation = append(navigation, tgbotapi.NewInlineKeyboardButtonData("→", fmt.Sprintf("mail_page:%d", page+1)))
	}
	rows = append(rows, navigation)
	return text, tgbotapi.InlineKeyboardMarkup{InlineKeyboard: rows}, true
}

func (b *Bot) openMail(cq *tgbotapi.CallbackQuery, uid uint32, page int) {
	user, ok := b.store.Get(cq.Message.Chat.ID)
	if !ok {
		b.editNavFromCallback(cq, "Сначала подключи почту через /login", backKeyboard())
		return
	}
	b.editNavFromCallback(cq, "📬 Загружаю письмо...", backKeyboard())
	message, err := b.loadMailDetail(cq.Message.Chat.ID, user, uid)
	if err != nil {
		b.editNav(cq.Message.Chat.ID, fmt.Sprintf("Ошибка загрузки письма: %v", err), mailBackKeyboard(page))
		return
	}

	text := formatMailDetails(message)
	kb := mailBackKeyboard(page)
	if len(message.Attachments) == 1 && !message.Attachments[0].TooLarge {
		caption := mailCaption(text)
		if len([]rune(caption)) <= 1024 && b.sendMailWithAttachment(cq.Message.Chat.ID, caption, kb, message.Attachments[0]) {
			if _, err := b.api.Request(tgbotapi.NewDeleteMessage(cq.Message.Chat.ID, cq.Message.MessageID)); err != nil {
				log.Printf("delete mail loading message: %v", err)
			}
			return
		}
	}

	edit := tgbotapi.NewEditMessageText(cq.Message.Chat.ID, cq.Message.MessageID, text)
	edit.ReplyMarkup = &kb
	if _, err := b.api.Send(edit); err != nil {
		log.Printf("openMail: %v", err)
		return
	}
	b.sendMailAttachments(cq.Message.Chat.ID, message.Attachments)
}

func mailCaption(text string) string {
	text = strings.ReplaceAll(text, "\n🖼 Изображение прикреплено.", "")
	text = strings.ReplaceAll(text, "🖼 Изображение прикреплено.", "")
	return strings.TrimSpace(text)
}

func (b *Bot) sendMailWithAttachment(chatID int64, caption string, kb tgbotapi.InlineKeyboardMarkup, attachment mailer.Attachment) bool {
	if strings.HasPrefix(strings.ToLower(attachment.ContentType), "image/") && len(attachment.Data) <= 10*1024*1024 {
		photo := tgbotapi.NewPhoto(chatID, tgbotapi.FileBytes{Name: attachment.Name, Bytes: attachment.Data})
		photo.Caption = caption
		photo.ReplyMarkup = kb
		_, err := b.api.Send(photo)
		if err != nil {
			log.Printf("send combined mail photo: %v", err)
			return false
		}
		return true
	}
	document := tgbotapi.NewDocument(chatID, tgbotapi.FileBytes{Name: attachment.Name, Bytes: attachment.Data})
	document.Caption = caption
	document.ReplyMarkup = kb
	if _, err := b.api.Send(document); err != nil {
		log.Printf("send combined mail document: %v", err)
		return false
	}
	return true
}

func mailBackKeyboard(page int) tgbotapi.InlineKeyboardMarkup {
	return tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("← К списку писем", fmt.Sprintf("mail_page:%d", page)),
		),
	)
}

func formatMailDetails(message mailer.Message) string {
	text := fmt.Sprintf("📬 Письмо\n\nОт: %s\nТема: %s", message.From, message.Subject)
	if message.Date != "" {
		text += "\nДата: " + formatMailDate(message.Date)
	}
	body := strings.TrimSpace(message.Body)
	for _, attachment := range message.Attachments {
		if strings.HasPrefix(strings.ToLower(attachment.ContentType), "image/") && len(attachment.Data) > 0 {
			body = strings.TrimSpace(inlineImageMarkerPattern.ReplaceAllString(body, "🖼 Изображение прикреплено."))
			break
		}
	}
	if body == "" {
		body = "Текст письма недоступен."
	}
	text += "\n\n" + body
	if len([]rune(text)) > 3900 {
		text = string([]rune(text)[:3899]) + "…"
	}
	return text
}

func formatMailDate(value string) string {
	value = strings.TrimSpace(value)
	for _, layout := range []string{"20060102T150405Z", "20060102T150405"} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed.Format("02.01.2006 15:04")
		}
	}
	return value
}

func (b *Bot) sendMailAttachments(chatID int64, attachments []mailer.Attachment) {
	var files []mailer.Attachment
	for _, attachment := range attachments {
		if !attachment.TooLarge {
			files = append(files, attachment)
		}
	}
	if len(files) == 0 {
		return
	}
	for _, file := range files {
		var err error
		if strings.HasPrefix(strings.ToLower(file.ContentType), "image/") && len(file.Data) <= 10*1024*1024 {
			_, err = b.api.Send(tgbotapi.NewPhoto(chatID, tgbotapi.FileBytes{Name: file.Name, Bytes: file.Data}))
		} else {
			_, err = b.api.Send(tgbotapi.NewDocument(chatID, tgbotapi.FileBytes{Name: file.Name, Bytes: file.Data}))
		}
		if err != nil {
			log.Printf("send mail attachment %s: %v", file.Name, err)
		}
	}
}

func truncateMailText(value string, limit int) string {
	value = strings.Join(strings.Fields(value), " ")
	if len([]rune(value)) <= limit {
		return value
	}
	return string([]rune(value)[:limit-1]) + "…"
}

// --- Callback handler ---

func (b *Bot) handleCallback(cq *tgbotapi.CallbackQuery) {
	// Acknowledge the callback to remove the loading spinner
	go func() {
		if _, err := b.api.Request(tgbotapi.NewCallback(cq.ID, "")); err != nil {
			log.Printf("callback ack: %v", err)
		}
	}()
	if strings.HasPrefix(cq.Data, "mail_page:") {
		page, err := strconv.Atoi(strings.TrimPrefix(cq.Data, "mail_page:"))
		if err == nil {
			b.editMailPage(cq, page)
		}
		return
	}
	if strings.HasPrefix(cq.Data, "mail_open:") {
		value := strings.TrimPrefix(cq.Data, "mail_open:")
		parts := strings.SplitN(value, ":page:", 2)
		uid, err := strconv.ParseUint(parts[0], 10, 32)
		if err == nil {
			page := 0
			if len(parts) == 2 {
				page, _ = strconv.Atoi(parts[1])
			}
			b.openMail(cq, uint32(uid), page)
		}
		return
	}

	switch cq.Data {
	case cbMenu:
		b.editNavFromCallback(cq, navText, mainKeyboard())

	case cbMail:
		b.editMailPage(cq, 0)

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
		b.pollUsers()
	}
}

func (b *Bot) pollUsers() {
	// Do not let a slow mail server start overlapping polling rounds.
	if !b.pollMu.TryLock() {
		log.Printf("poll skipped: previous round is still running")
		return
	}
	defer b.pollMu.Unlock()

	users := b.store.All()
	workers := 4
	if len(users) < workers {
		workers = len(users)
	}
	if workers == 0 {
		return
	}
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	for _, user := range users {
		user := user
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if err := b.checkUser(user); err != nil {
				log.Printf("poll error chat=%d: %v", user.ChatID, err)
			}
		}()
	}
	wg.Wait()
}

func (b *Bot) checkUser(user storage.User) error {
	msgs, err := mailer.GetNewMessages(user.Email, user.Password, user.LastUID)
	if err != nil {
		return err
	}
	var latestUID uint32
	for _, m := range msgs {
		b.invalidateMailCache(user.ChatID)
		if !b.send(user.ChatID, "📬 Получено новое письмо!\n\nОткрой раздел 📬 Почта, чтобы посмотреть список писем.") {
			return fmt.Errorf("notification send failed for UID %d", m.UID)
		}
		if m.UID > latestUID {
			latestUID = m.UID
		}
	}
	if latestUID > user.LastUID {
		if err := b.store.UpdateLastUID(user.ChatID, latestUID); err != nil {
			return fmt.Errorf("update uid: %w", err)
		}
	}
	_ = b.store.SaveMailCache(user.ChatID, mailerMailMessages(msgs))
	return nil
}

func (b *Bot) send(chatID int64, text string) bool {
	msg := tgbotapi.NewMessage(chatID, text)
	if _, err := b.api.Send(msg); err != nil {
		log.Printf("send: %v", err)
		return false
	}
	return true
}
