// lkstest — быстрая проверка LKS-функций без запуска бота.
//
// Использование:
//
//	go run ./cmd/lkstest -email you@student.bmstu.ru -password secret -group ИУ7-21Б
package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"

	"tg-mail-bot/internal/lks"
)

func main() {
	email := flag.String("email", "", "Бауманская почта (обязательно)")
	password := flag.String("password", "", "Пароль (обязательно)")
	group := flag.String("group", "", "Группа для расписания, напр. ИУ7-21Б (опционально)")
	flag.Parse()

	if *email == "" || *password == "" {
		flag.Usage()
		log.Fatal("укажи -email и -password")
	}

	// --- Авторизация ---
	fmt.Println("=== Авторизация в ЛКС ===")
	client, err := lks.NewClient(*email, *password)
	if err != nil {
		log.Fatalf("ОШИБКА: %v", err)
	}
	fmt.Println("OK — сессия установлена")

	// --- Диагностика: пробуем известные FV-эндпоинты ---
	fmt.Println("\n=== Диагностика FV API ===")
	for _, path := range []string{
		"/fv",
		"/fv/stages",
		"/fv/me",
		"/fv/dates",
		"/fv/me/military_events",
	} {
		data, err := lks.APIGet(client, path)
		if err != nil {
			fmt.Printf("  GET %s → ОШИБКА: %v\n", path, err)
		} else {
			snippet := string(data)
			if len(snippet) > 300 {
				snippet = snippet[:300] + "..."
			}
			fmt.Printf("  GET %s → %s\n\n", path, snippet)
		}
	}

	// --- Диагностика: текущая неделя и полное расписание ---
	fmt.Println("\n=== Диагностика Schedule API ===")
	for _, path := range []string{
		"/schedules/current",
		"/schedules/groups/814a726c-bc7e-11ee-b32d-df9b99f124c0/public",
	} {
		resp, err := (&http.Client{Timeout: 10 * time.Second}).Get("https://lks.bmstu.ru/lks-back/api/v1" + path)
		if err != nil {
			fmt.Printf("  GET %s → ОШИБКА: %v\n", path, err)
			continue
		}
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		snippet := string(data)
		if len(snippet) > 1200 {
			snippet = snippet[:1200] + "..."
		}
		fmt.Printf("  GET %s →\n%s\n\n", path, snippet)
	}

	// --- Физкультура ---
	fmt.Println("\n=== Физкультура ===")
	sport, err := lks.GetSportAttendance(client)
	if err != nil {
		fmt.Printf("ОШИБКА: %v\n", err)
	} else {
		fmt.Println(sport)
	}

	// --- Расписание ---
	if *group != "" {
		fmt.Println("\n=== Расписание ===")
		uuid, err := lks.FindGroupUUID(*group)
		if err != nil {
			fmt.Printf("ОШИБКА поиска группы: %v\n", err)
		} else {
			fmt.Printf("UUID группы %q: %s\n", *group, uuid)
			schedule, err := lks.GetWeekSchedule(uuid)
			if err != nil {
				fmt.Printf("ОШИБКА расписания: %v\n", err)
			} else {
				fmt.Println(schedule)
			}
		}
	}
}
