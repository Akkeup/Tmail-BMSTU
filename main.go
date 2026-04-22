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

	store, err := storage.New("data/users.json")
	if err != nil {
		log.Fatalf("storage: %v", err)
	}

	pollInterval := 2 * time.Minute
	if v := os.Getenv("POLL_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			pollInterval = d
		}
	}

	b, err := bot.New(token, store, pollInterval)
	if err != nil {
		log.Fatalf("bot: %v", err)
	}

	log.Printf("Bot started, polling every %v", pollInterval)
	b.Run()
}
