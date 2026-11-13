package bot

import (
	"strings"
	"testing"

	"tg-mail-bot/internal/mailer"
)

func TestRenderMailPageUsesFourMessages(t *testing.T) {
	messages := make([]mailer.Message, 5)
	for i := range messages {
		messages[i] = mailer.Message{UID: uint32(i + 1), Subject: "Subject"}
	}

	text, keyboard, ok := renderMailPage(messages, 0)
	if !ok {
		t.Fatal("expected a rendered page")
	}
	if !strings.Contains(text, "Страница 1 из 2") {
		t.Fatalf("unexpected page header: %s", text)
	}
	if len(keyboard.InlineKeyboard) != 5 {
		t.Fatalf("expected four mail rows plus navigation, got %d", len(keyboard.InlineKeyboard))
	}
	if keyboard.InlineKeyboard[4][1].CallbackData == nil || *keyboard.InlineKeyboard[4][1].CallbackData != "mail_page:1" {
		t.Fatalf("expected next page callback")
	}
}

func TestRenderMailPageEmpty(t *testing.T) {
	_, _, ok := renderMailPage(nil, 0)
	if ok {
		t.Fatal("expected empty mailbox page to be unavailable")
	}
}

func TestFormatMailDetailsCleansInlineImageURLWithParentheses(t *testing.T) {
	text := formatMailDetails(mailer.Message{
		Subject: "Афиша",
		From:    "sender@example.com",
		Body:    "Афиша: [img] https://student.bmstu.ru/path/foto(6).png",
		Attachments: []mailer.Attachment{{
			ContentType: "image/png",
			Data:        []byte("png"),
		}},
	})
	if strings.Contains(text, "foto(6).png") || strings.Contains(text, ").png") {
		t.Fatalf("inline image URL leaked into message: %s", text)
	}
	if !strings.Contains(text, "🖼 Изображение прикреплено.") {
		t.Fatalf("expected inline image marker: %s", text)
	}
}

func TestMailDetailCache(t *testing.T) {
	b := &Bot{
		mailDetails: make(map[int64]map[uint32]mailDetailCacheEntry),
		mailWarming: make(map[int64]bool),
	}
	b.cacheMailDetail(7, mailer.Message{UID: 42, Subject: "cached"})
	message, ok := b.cachedMailDetail(7, 42)
	if !ok || message.Subject != "cached" {
		t.Fatalf("expected cached detail, got %+v, %v", message, ok)
	}
}

func TestMailCaptionRemovesInlineMarker(t *testing.T) {
	caption := mailCaption("📬 Письмо\n\nТекст\n🖼 Изображение прикреплено.")
	if strings.Contains(caption, "Изображение прикреплено") || !strings.Contains(caption, "Текст") {
		t.Fatalf("unexpected caption: %s", caption)
	}
}

func TestFormatMailDetailsKeepsImageURLWhenDownloadFailed(t *testing.T) {
	text := formatMailDetails(mailer.Message{
		Subject: "Афиша",
		Body:    "Афиша: [img] https://student.bmstu.ru/path/foto(6).png",
	})
	if !strings.Contains(text, "https://student.bmstu.ru/path/foto(6).png") {
		t.Fatalf("expected original image URL as fallback: %s", text)
	}
	if strings.Contains(text, "Изображение прикреплено") {
		t.Fatalf("must not claim image was attached: %s", text)
	}
}

func TestMailDetailCacheRejectsMissingInlineImage(t *testing.T) {
	b := &Bot{
		mailDetails: make(map[int64]map[uint32]mailDetailCacheEntry),
		mailWarming: make(map[int64]bool),
	}
	b.cacheMailDetail(7, mailer.Message{
		UID:  42,
		Body: "[img] https://student.bmstu.ru/path/image.png",
	})
	if _, ok := b.cachedMailDetail(7, 42); ok {
		t.Fatal("incomplete inline-image message must not be cached")
	}
}

func TestFormatMailDate(t *testing.T) {
	if got := formatMailDate("20261020T102216Z"); got != "20.10.2026 10:22" {
		t.Fatalf("unexpected formatted mail date: %s", got)
	}
	if got := formatMailDate("unknown"); got != "unknown" {
		t.Fatalf("unexpected fallback date: %s", got)
	}
}
