package mailer

import (
	"encoding/xml"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	BaseURL    = "https://student.bmstu.ru"
	WebmailURL = "https://student.bmstu.ru/hpronto/"
	inbox      = "INBOX-MM-1"
)

type Message struct {
	UID     uint32
	Subject string
}

type ximssSession struct {
	urlID  string
	client *http.Client
	seq    int
}

func (s *ximssSession) call(xmlBody string) ([]byte, error) {
	s.seq++
	u := fmt.Sprintf("%s/Session/%s/sync?reqSeq=%d&random=%d",
		BaseURL, s.urlID, s.seq, s.seq)
	resp, err := s.client.Post(u, "text/xml", strings.NewReader(xmlBody))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

func (s *ximssSession) close() {
	_, _ = s.call(`<XIMSS><logout/></XIMSS>`)
}

func openSession(email, password string) (*ximssSession, error) {
	c := &http.Client{Timeout: 15 * time.Second}

	resp, err := c.PostForm(BaseURL+"/XIMSSLogin/", url.Values{
		"userName":        {email},
		"password":        {password},
		"errorAsXML":      {"1"},
		"EnableUseCookie": {"1"},
		"x2auth":          {"1"},
		"canUpdatePwd":    {"1"},
		"version":         {"6.1"},
	})
	if err != nil {
		return nil, fmt.Errorf("не удалось подключиться к серверу: %w", err)
	}
	defer resp.Body.Close()

	data, _ := io.ReadAll(resp.Body)
	log.Printf("[ximss] login: %s", data)

	var r struct {
		XMLName  xml.Name `xml:"XIMSS"`
		Session  *struct {
			UrlID string `xml:"urlID,attr"`
		} `xml:"session"`
		Response *struct {
			Status string `xml:"status,attr"`
		} `xml:"response"`
	}

	if err := xml.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("parse error: %w", err)
	}
	if r.Response != nil || r.Session == nil || r.Session.UrlID == "" {
		return nil, fmt.Errorf("неверный логин или пароль")
	}

	return &ximssSession{urlID: r.Session.UrlID, client: c}, nil
}

// TestConnection checks that credentials are valid.
func TestConnection(email, password string) error {
	s, err := openSession(email, password)
	if err != nil {
		return err
	}
	s.close()
	return nil
}

// GetMaxUID returns the current highest UID in INBOX (baseline on first login).
func GetMaxUID(email, password string) (uint32, error) {
	s, err := openSession(email, password)
	if err != nil {
		return 0, err
	}
	defer s.close()

	msgs, err := browseFolder(s)
	if err != nil {
		return 0, err
	}

	var max uint32
	for _, m := range msgs {
		if m.UID > max {
			max = m.UID
		}
	}
	return max, nil
}

// GetNewMessages returns messages with UID > lastUID.
func GetNewMessages(email, password string, lastUID uint32) ([]Message, error) {
	s, err := openSession(email, password)
	if err != nil {
		return nil, err
	}
	defer s.close()

	msgs, err := browseFolder(s)
	if err != nil {
		return nil, err
	}

	var result []Message
	for _, m := range msgs {
		if m.UID > lastUID {
			result = append(result, m)
		}
	}
	return result, nil
}

func browseFolder(s *ximssSession) ([]Message, error) {
	// folderOpen requires both mailbox (real) and folder (virtual) attributes,
	// plus the list of fields to return in folderReport responses.
	openData, err := s.call(
		`<XIMSS><folderOpen mailbox="INBOX" sortField="INTERNALDATE" sortOrder="desc" folder="INBOX-MM-1" id="1">` +
			`<field>FLAGS</field><field>Subject</field><field>INTERNALDATE</field>` +
			`</folderOpen></XIMSS>`,
	)
	if err != nil {
		return nil, err
	}
	log.Printf("[ximss] folderOpen: %s", openData)

	data, err := s.call(
		`<XIMSS><folderBrowse folder="INBOX-MM-1" id="2"><index from="0" till="49"/></folderBrowse></XIMSS>`,
	)
	if err != nil {
		return nil, err
	}
	log.Printf("[ximss] folderBrowse: %s", data)

	var r struct {
		XMLName xml.Name `xml:"XIMSS"`
		Reports []struct {
			UID     uint32 `xml:"UID,attr"`
			Subject string `xml:"Subject"`
		} `xml:"folderReport"`
	}

	if err := xml.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("parse folder: %w", err)
	}

	var msgs []Message
	for _, rep := range r.Reports {
		subject := rep.Subject
		if subject == "" {
			subject = "(без темы)"
		}
		msgs = append(msgs, Message{UID: rep.UID, Subject: subject})
	}
	return msgs, nil
}
