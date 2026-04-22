package mailer

import (
	"crypto/tls"
	"fmt"
	"net"
	"time"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"
)

const dialTimeout = 15 * time.Second

const (
	IMAPServer = "student.bmstu.ru:993"
	WebmailURL = "https://student.bmstu.ru/hpronto/"
)

type Message struct {
	UID     uint32
	Subject string
}

func dialTLS(cfg *tls.Config) (*client.Client, error) {
	dialer := &net.Dialer{Timeout: dialTimeout}
	conn, err := tls.DialWithDialer(dialer, "tcp", IMAPServer, cfg)
	if err != nil {
		return nil, err
	}
	conn.SetDeadline(time.Now().Add(dialTimeout))
	c, err := client.New(conn)
	if err != nil {
		conn.Close()
		return nil, err
	}
	conn.SetDeadline(time.Time{}) // снять дедлайн после handshake
	return c, nil
}

func dial(email, password string) (*client.Client, error) {
	cfg := &tls.Config{
		ServerName: "student.bmstu.ru",
		MinVersion: tls.VersionTLS10,
	}
	c, err := dialTLS(cfg)
	if err != nil {
		// Повтор с отключённой проверкой сертификата (self-signed)
		cfg2 := &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS10}
		c, err = dialTLS(cfg2)
		if err != nil {
			return nil, fmt.Errorf("не удалось подключиться к почтовому серверу: %w", err)
		}
	}
	if err := c.Login(email, password); err != nil {
		_ = c.Logout()
		return nil, fmt.Errorf("неверный логин или пароль")
	}
	return c, nil
}

// TestConnection checks that the credentials are valid.
func TestConnection(email, password string) error {
	c, err := dial(email, password)
	if err != nil {
		return err
	}
	return c.Logout()
}

// GetMaxUID returns the current maximum UID in INBOX (used as baseline on first login).
func GetMaxUID(email, password string) (uint32, error) {
	c, err := dial(email, password)
	if err != nil {
		return 0, err
	}
	defer c.Logout()

	mbox, err := c.Select("INBOX", true)
	if err != nil {
		return 0, fmt.Errorf("select inbox: %w", err)
	}
	if mbox.Messages == 0 {
		return 0, nil
	}

	// Fetch only the last message to get its UID
	seqSet := new(imap.SeqSet)
	seqSet.AddRange(mbox.Messages, mbox.Messages)

	ch := make(chan *imap.Message, 1)
	done := make(chan error, 1)
	go func() {
		done <- c.Fetch(seqSet, []imap.FetchItem{imap.FetchUid}, ch)
	}()

	var maxUID uint32
	for msg := range ch {
		if msg.Uid > maxUID {
			maxUID = msg.Uid
		}
	}
	return maxUID, <-done
}

// GetNewMessages returns all messages with UID > lastUID.
func GetNewMessages(email, password string, lastUID uint32) ([]Message, error) {
	c, err := dial(email, password)
	if err != nil {
		return nil, err
	}
	defer c.Logout()

	mbox, err := c.Select("INBOX", true)
	if err != nil {
		return nil, fmt.Errorf("select inbox: %w", err)
	}
	if mbox.Messages == 0 {
		return nil, nil
	}

	// Get all UIDs and filter client-side
	criteria := imap.NewSearchCriteria()
	allUIDs, err := c.UidSearch(criteria)
	if err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}

	var newUIDs []uint32
	for _, uid := range allUIDs {
		if uid > lastUID {
			newUIDs = append(newUIDs, uid)
		}
	}
	if len(newUIDs) == 0 {
		return nil, nil
	}

	fetchSet := new(imap.SeqSet)
	for _, uid := range newUIDs {
		fetchSet.AddNum(uid)
	}

	msgChan := make(chan *imap.Message, len(newUIDs))
	done := make(chan error, 1)
	go func() {
		done <- c.UidFetch(fetchSet, []imap.FetchItem{imap.FetchEnvelope, imap.FetchUid}, msgChan)
	}()

	var result []Message
	for msg := range msgChan {
		subject := "(без темы)"
		if msg.Envelope != nil && msg.Envelope.Subject != "" {
			subject = msg.Envelope.Subject
		}
		result = append(result, Message{UID: msg.Uid, Subject: subject})
	}

	return result, <-done
}
