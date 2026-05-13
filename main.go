package main

import (
	"log"
	"os"
	"time"

	"tg-mail-bot/internal/bot"
	"tg-mail-bot/internal/storage"
)

func main() {
	token := os.Getenv("BOT_TOKEN")
	if token == "" {
		log.Fatal("BOT_TOKEN env variable not set")
	}

	secret := os.Getenv("SECRET_KEY")
	if secret == "" {
		log.Fatal("SECRET_KEY env variable not set")
	}

	store, err := storage.New("data/users.json", secret)
	if err != nil {
		log.Fatalf("storage: %v", err)
	}

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
		log.Fatalf("bot: %v", err)
	}

	log.Printf("Bot started, polling every %v", pollInterval)
	b.Run()
}
