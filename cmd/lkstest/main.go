// lkstest — быстрая проверка LKS-функций без запуска бота.
//
// Использование:
//
//	go run ./cmd/lkstest -email you@student.bmstu.ru -password secret -group ИУ7-21Б
package main

import (
	"flag"
	"fmt"
	"log"

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
