package mailer

import (
	"encoding/xml"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const (
	BaseURL           = "https://student.bmstu.ru"
	WebmailURL        = "https://student.bmstu.ru/hpronto/"
	inbox             = "INBOX-MM-1"
	maxAttachmentSize = 50 * 1024 * 1024 // 50 MB — Telegram limit
)

type Attachment struct {
	Name        string
	ContentType string
	Data        []byte
}

type Message struct {
	UID         uint32
	Subject     string
	From        string
	ContentType string
	Attachments []Attachment
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
		if m.UID <= lastUID {
			continue
		}
		if strings.Contains(strings.ToLower(m.ContentType), "multipart") {
			atts, err := fetchAttachments(s, m.UID)
			if err != nil {
				log.Printf("[ximss] attachments UID=%d: %v", m.UID, err)
			} else {
				m.Attachments = atts
			}
		}
		result = append(result, m)
	}
	return result, nil
}

func browseFolder(s *ximssSession) ([]Message, error) {
	openData, err := s.call(
		`<XIMSS><folderOpen mailbox="INBOX" sortField="INTERNALDATE" sortOrder="desc" folder="INBOX-MM-1" id="1">` +
			`<field>FLAGS</field><field>Subject</field><field>E-From</field><field>Content-Type</field><field>INTERNALDATE</field>` +
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
			UID         uint32 `xml:"UID,attr"`
			Subject     string `xml:"Subject"`
			From        string `xml:"E-From"`
			ContentType string `xml:"Content-Type"`
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
		msgs = append(msgs, Message{
			UID:         rep.UID,
			Subject:     subject,
			From:        rep.From,
			ContentType: rep.ContentType,
		})
	}
	return msgs, nil
}

var attachRe = regexp.MustCompile(`MIME/[^"'\s<>]+-\d{2}-[A-Z]/[^"'\s<>]+`)

func fetchAttachments(s *ximssSession, uid uint32) ([]Attachment, error) {
	// Fetch the HTML view of the email to find attachment URLs
	htmlURL := fmt.Sprintf("%s/Session/%s/%d", BaseURL, s.urlID, uid)
	resp, err := s.client.Get(htmlURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d fetching message view %d", resp.StatusCode, uid)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	log.Printf("[ximss] message view UID=%d (%d bytes)", uid, len(body))

	// Find all attachment paths in the HTML
	matches := attachRe.FindAllString(string(body), -1)
	seen := make(map[string]bool)
	var attachments []Attachment
	for _, path := range matches {
		if seen[path] {
			continue
		}
		seen[path] = true

		attURL := fmt.Sprintf("%s/Session/%s/%s", BaseURL, s.urlID, path)
		log.Printf("[ximss] downloading attachment: %s", attURL)

		attResp, err := s.client.Get(attURL)
		if err != nil {
			log.Printf("[ximss] download %s: %v", attURL, err)
			continue
		}
		data, err := io.ReadAll(io.LimitReader(attResp.Body, maxAttachmentSize))
		attResp.Body.Close()
		if err != nil || len(data) == 0 {
			log.Printf("[ximss] read attachment %s: err=%v len=%d", path, err, len(data))
			continue
		}

		// Extract filename from URL path (last segment, URL-decoded)
		segments := strings.Split(path, "/")
		filename, _ := url.PathUnescape(segments[len(segments)-1])
		if filename == "" {
			filename = segments[len(segments)-1]
		}

		ct := attResp.Header.Get("Content-Type")
		if i := strings.Index(ct, ";"); i >= 0 {
			ct = strings.TrimSpace(ct[:i])
		}

		log.Printf("[ximss] attachment %s (%d bytes) UID=%d", filename, len(data), uid)
		attachments = append(attachments, Attachment{
			Name:        filename,
			ContentType: ct,
			Data:        data,
		})
	}
	return attachments, nil
}
