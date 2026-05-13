package mailer

import (
	"encoding/xml"
	"fmt"
	"io"
	"log"
	"mime"
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
	TooLarge    bool  // file exceeded Telegram's 50 MB limit
	Size        int64 // original size in bytes (set when TooLarge)
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

// fetchMIMEPartNames tries to get real filenames from the message MIME structure
// via the XIMSS messageInfo command. Returns a map of partNum → filename.
func fetchMIMEPartNames(s *ximssSession, uid uint32) map[int]string {
	data, err := s.call(fmt.Sprintf(
		`<XIMSS><messageInfo folder="%s" uid="%d" id="10"/></XIMSS>`,
		inbox, uid,
	))
	if err != nil {
		log.Printf("[ximss] messageInfo UID=%d err: %v", uid, err)
		return nil
	}
	log.Printf("[ximss] messageInfo UID=%d: %s", uid, data)

	var r struct {
		XMLName xml.Name `xml:"XIMSS"`
		Parts   []struct {
			Num      int    `xml:"num,attr"`
			Name     string `xml:"name,attr"`
			Filename string `xml:"filename,attr"`
			Charset  string `xml:"charset,attr"`
		} `xml:"mimePart"`
	}
	if err := xml.Unmarshal(data, &r); err != nil {
		log.Printf("[ximss] messageInfo parse err: %v", err)
		return nil
	}
	names := make(map[int]string)
	for _, p := range r.Parts {
		if p.Num > 0 && (p.Filename != "" || p.Name != "") {
			if p.Filename != "" {
				names[p.Num] = p.Filename
			} else {
				names[p.Num] = p.Name
			}
		}
	}
	return names
}

func fetchAttachments(s *ximssSession, uid uint32) ([]Attachment, error) {
	partNames := fetchMIMEPartNames(s, uid)
	var attachments []Attachment
	for partNum := 1; partNum <= 20; partNum++ {
		hint := partNames[partNum]
		att, skip, err := fetchPart(s, uid, partNum, hint)
		if err != nil {
			break // part not found — no more parts
		}
		if skip {
			continue // body part, not an attachment
		}
		attachments = append(attachments, att)
	}
	return attachments, nil
}

// fetchPart tries to download MIME part partNum of the message.
// Returns (att, false, nil) for an attachment part,
// (zero, true, nil) for a body part to skip,
// (zero, false, err) if the part doesn't exist.
// nameHint, if non-empty, is used as the filename when the server doesn't provide one.
func fetchPart(s *ximssSession, uid uint32, partNum int, nameHint string) (Attachment, bool, error) {
	var resp *http.Response
	for _, enc := range []string{"B", "Q"} {
		u := fmt.Sprintf("%s/Session/%s/MIME/%s/%d-%02d-%s/x",
			BaseURL, s.urlID, inbox, uid, partNum, enc)
		r, err := s.client.Get(u)
		if err != nil {
			continue
		}
		if r.StatusCode == http.StatusOK {
			resp = r
			break
		}
		r.Body.Close()
	}
	if resp == nil {
		return Attachment{}, false, fmt.Errorf("part %d not found", partNum)
	}
	defer resp.Body.Close()

	ct := resp.Header.Get("Content-Type")
	baseType := ct
	if i := strings.Index(ct, ";"); i >= 0 {
		baseType = strings.TrimSpace(ct[:i])
	}
	btLow := strings.ToLower(baseType)

	// Skip body parts (text or multipart wrappers)
	if strings.HasPrefix(btLow, "text/") || strings.HasPrefix(btLow, "multipart/") {
		log.Printf("[ximss] UID=%d part %02d: skipping %s", uid, partNum, baseType)
		return Attachment{}, true, nil
	}

	// Resolve filename before reading body (needed for too-large reporting too)
	cd := resp.Header.Get("Content-Disposition")
	filename := parseContentDisposition(cd)
	if filename == "" || filename == "x" {
		if finalPath := resp.Request.URL.Path; !strings.HasSuffix(finalPath, "/x") {
			parts := strings.Split(finalPath, "/")
			if last := parts[len(parts)-1]; last != "" && last != "x" {
				if decoded, err := url.PathUnescape(last); err == nil {
					filename = decoded
				}
			}
		}
	}
	if (filename == "" || filename == "x") && nameHint != "" {
		filename = nameHint
	}
	if filename == "" || filename == "x" {
		ext := ""
		if exts, _ := mime.ExtensionsByType(baseType); len(exts) > 0 {
			ext = exts[0]
		}
		filename = fmt.Sprintf("attachment-%d%s", partNum, ext)
	}

	// Check Content-Length before downloading
	if cl := resp.ContentLength; cl > maxAttachmentSize {
		log.Printf("[ximss] UID=%d part %02d: too large (%d bytes)", uid, partNum, cl)
		return Attachment{Name: filename, TooLarge: true, Size: cl}, false, nil
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxAttachmentSize))
	if err != nil || len(data) == 0 {
		log.Printf("[ximss] UID=%d part %02d: read err=%v len=%d", uid, partNum, err, len(data))
		return Attachment{}, true, nil
	}
	// If we read exactly the limit, the file was likely truncated
	if int64(len(data)) == maxAttachmentSize {
		log.Printf("[ximss] UID=%d part %02d: hit size limit, treating as too large", uid, partNum)
		return Attachment{Name: filename, TooLarge: true, Size: maxAttachmentSize}, false, nil
	}
	log.Printf("[ximss] attachment %s (%d bytes) UID=%d part=%02d cd=%q", filename, len(data), uid, partNum, cd)
	return Attachment{Name: filename, ContentType: baseType, Data: data}, false, nil
}

// parseContentDisposition extracts filename from Content-Disposition header.
func parseContentDisposition(cd string) string {
	// Try filename*= (RFC 5987): UTF-8''encoded-name
	if i := strings.Index(cd, "filename*="); i >= 0 {
		s := cd[i+10:]
		if j := strings.LastIndex(s, "'"); j >= 0 {
			if decoded, err := url.PathUnescape(s[j+1:]); err == nil && decoded != "" {
				return strings.TrimSpace(decoded)
			}
		}
	}
	// Try filename="..."
	if i := strings.Index(cd, "filename="); i >= 0 {
		s := strings.TrimSpace(cd[i+9:])
		s = strings.Trim(s, "\"")
		if j := strings.Index(s, ";"); j >= 0 {
			s = strings.TrimSpace(s[:j])
		}
		return s
	}
	return ""
}
