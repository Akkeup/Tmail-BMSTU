// broadcast — отправляет сообщение всем пользователям бота.
//
// Использование на сервере:
//
//	BOT_TOKEN=... SECRET_KEY=... go run ./cmd/broadcast -msg "Текст сообщения"
//
// Или через docker:
//
//	docker compose run --rm bot ./broadcast -msg "Текст сообщения"
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"tg-mail-bot/internal/storage"
)

func main() {
	msg := flag.String("msg", "", "Текст сообщения (обязательно)")
	dataPath := flag.String("data", "data/users.json", "Путь к файлу пользователей")
	flag.Parse()

	if *msg == "" {
		flag.Usage()
		log.Fatal("укажи -msg")
	}

	token := os.Getenv("BOT_TOKEN")
	if token == "" {
		log.Fatal("BOT_TOKEN не задан")
	}
	secret := os.Getenv("SECRET_KEY")
	if secret == "" {
		log.Fatal("SECRET_KEY не задан")
	}

	store, err := storage.New(*dataPath, secret)
	if err != nil {
		log.Fatalf("storage: %v", err)
	}

	api, err := tgbotapi.NewBotAPI(token)
	if err != nil {
		log.Fatalf("bot api: %v", err)
	}

	users := store.All()
	log.Printf("Рассылка %d пользователям...", len(users))

	ok, fail := 0, 0
	for _, u := range users {
		m := tgbotapi.NewMessage(u.ChatID, *msg)
		if _, err := api.Send(m); err != nil {
			log.Printf("  [%d] ОШИБКА: %v", u.ChatID, err)
			fail++
		} else {
			log.Printf("  [%d] отправлено", u.ChatID)
			ok++
		}
		time.Sleep(50 * time.Millisecond) // не превышаем лимит Telegram (~20 msg/s)
	}

	fmt.Printf("\nГотово: %d успешно, %d ошибок.\n", ok, fail)
}
