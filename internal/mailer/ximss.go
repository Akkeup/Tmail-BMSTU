package mailer

import (
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"io"
	"log"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/http"
	"net/url"
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

func fetchAttachments(s *ximssSession, uid uint32) ([]Attachment, error) {
	msgURL := fmt.Sprintf("%s/Session/%s/MIME/%s/%d", BaseURL, s.urlID, inbox, uid)
	resp, err := s.client.Get(msgURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d fetching message %d", resp.StatusCode, uid)
	}

	ct := resp.Header.Get("Content-Type")
	log.Printf("[ximss] message UID=%d Content-Type: %s", uid, ct)

	mediaType, params, err := mime.ParseMediaType(ct)
	if err != nil || !strings.HasPrefix(mediaType, "multipart/") {
		return nil, nil
	}

	mr := multipart.NewReader(resp.Body, params["boundary"])
	var attachments []Attachment
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			log.Printf("[ximss] multipart UID=%d: %v", uid, err)
			break
		}

		filename := part.FileName()
		if filename == "" {
			continue
		}

		cte := strings.ToLower(strings.TrimSpace(part.Header.Get("Content-Transfer-Encoding")))
		var data []byte
		switch cte {
		case "base64":
			raw, _ := io.ReadAll(io.LimitReader(part, maxAttachmentSize))
			clean := strings.NewReplacer("\r", "", "\n", "", " ", "", "\t", "").Replace(string(raw))
			data, err = base64.StdEncoding.DecodeString(clean)
			if err != nil {
				data, err = base64.RawStdEncoding.DecodeString(clean)
			}
		case "quoted-printable":
			data, err = io.ReadAll(io.LimitReader(quotedprintable.NewReader(part), maxAttachmentSize))
		default:
			data, err = io.ReadAll(io.LimitReader(part, maxAttachmentSize))
		}

		if err != nil || len(data) == 0 {
			log.Printf("[ximss] read %s UID=%d: err=%v len=%d", filename, uid, err, len(data))
			continue
		}

		partCT := part.Header.Get("Content-Type")
		if i := strings.Index(partCT, ";"); i >= 0 {
			partCT = strings.TrimSpace(partCT[:i])
		}

		log.Printf("[ximss] attachment %s (%d bytes) UID=%d", filename, len(data), uid)
		attachments = append(attachments, Attachment{
			Name:        filename,
			ContentType: partCT,
			Data:        data,
		})
	}
	return attachments, nil
}
