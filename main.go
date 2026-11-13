package main

import (
	"log"
	"os"
	"strings"
	"syscall"
	"time"

	"tg-mail-bot/internal/bot"
	"tg-mail-bot/internal/storage"
)

func main() {
	lock, err := acquireInstanceLock("data/.bot.lock")
	if err != nil {
		log.Fatalf("bot is already running or lock unavailable: %v", err)
	}
	defer lock()

	token := os.Getenv("BOT_TOKEN")
	if token == "" {
		log.Fatal("BOT_TOKEN env variable not set")
	}

	secret := os.Getenv("SECRET_KEY")
	if secret == "" {
		log.Fatal("SECRET_KEY env variable not set")
	}

	store, err := storage.NewSQLite("data/bot.db", "data/users.json", secret)
	if err != nil {
		log.Fatalf("storage: %v", err)
	}
	defer store.Close()

	const minPollInterval = 30 * time.Second
	pollInterval := 2 * time.Minute
	if v := os.Getenv("POLL_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			pollInterval = d
		}
	}
	if pollInterval < minPollInterval {
		log.Printf("POLL_INTERVAL too low, using minimum %v", minPollInterval)
		pollInterval = minPollInterval
	}

	b, err := bot.New(token, store, pollInterval)
	if err != nil {
		// Do not print the library error verbatim: Telegram API errors can
		// include the bot token in the request URL.
		safeError := strings.ReplaceAll(err.Error(), token, "<redacted-token>")
		log.Fatalf("bot: Telegram API initialization failed: %s", safeError)
	}

	log.Printf("Bot started, polling every %v", pollInterval)
	b.Run()
}

func acquireInstanceLock(path string) (func(), error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		_ = file.Close()
	}, nil
}
