package mailer

import (
	"context"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"fmt"
	"html"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	BaseURL           = "https://student.bmstu.ru"
	inbox             = "INBOX-MM-1"
	maxAttachmentSize = 50 * 1024 * 1024 // 50 MB — Telegram limit
)

var inlineImagePattern = regexp.MustCompile(`(?i)\[img\]\s*(https://[^\s]+(?:\s*[).][^\s]*)*)`)
var htmlBreakPattern = regexp.MustCompile(`(?i)<\s*/?(?:br|p|div|li|tr|h[1-6])\b[^>]*>`)
var htmlTagPattern = regexp.MustCompile(`(?s)<[^>]*>`)

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
	Date        string
	Body        string
	ContentType string
	Attachments []Attachment
	mimeParts   []mimePart
}

type mimePart struct {
	ID   string
	Name string
}

type ximssSession struct {
	urlID    string
	client   *http.Client
	username string
	password string
	seq      int
}

var accountSessionLocks sync.Map

func lockAccountSession(email string) func() {
	value, _ := accountSessionLocks.LoadOrStore(strings.ToLower(strings.TrimSpace(email)), &sync.Mutex{})
	mutex := value.(*sync.Mutex)
	mutex.Lock()
	return mutex.Unlock
}

func (s *ximssSession) call(xmlBody string) ([]byte, error) {
	return s.callContext(context.Background(), xmlBody, 2)
}

func (s *ximssSession) callContext(ctx context.Context, xmlBody string, attempts int) ([]byte, error) {
	s.seq++
	u := fmt.Sprintf("%s/Session/%s/sync?reqSeq=%d&random=%d",
		BaseURL, s.urlID, s.seq, s.seq)
	if attempts < 1 {
		attempts = 1
	}
	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(xmlBody))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "text/xml")
		resp, err := s.client.Do(req)
		if err == nil {
			data, readErr := io.ReadAll(resp.Body)
			resp.Body.Close()
			if readErr == nil {
				return data, nil
			}
			lastErr = readErr
		} else {
			lastErr = err
		}
		if attempt+1 < attempts {
			time.Sleep(500 * time.Millisecond)
		}
	}
	return nil, fmt.Errorf("XIMSS request failed after retries: %w", lastErr)
}

func (s *ximssSession) close() {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	data, err := s.callContext(ctx, `<XIMSS><logout/></XIMSS>`, 1)
	if err != nil {
		log.Printf("[ximss] logout: %v", err)
	} else if responseErr := checkXIMSSResponse(data, "logout"); responseErr != nil {
		log.Printf("[ximss] logout response: %v", responseErr)
	}

	// Some CommuniGate installations keep a session alive when the logout
	// command is interrupted or its response is lost. KILL is the documented
	// fallback for closing the session without cleanup.
	killURL := fmt.Sprintf("%s/Session/%s/KILL", BaseURL, url.PathEscape(s.urlID))
	killReq, reqErr := http.NewRequestWithContext(ctx, http.MethodGet, killURL, nil)
	if reqErr != nil {
		log.Printf("[ximss] build kill request: %v", reqErr)
	} else if resp, killErr := s.client.Do(killReq); killErr != nil {
		log.Printf("[ximss] kill session: %v", killErr)
	} else {
		resp.Body.Close()
		// 550 means the logout already removed the session; it is harmless.
		if resp.StatusCode >= http.StatusInternalServerError && resp.StatusCode != 550 {
			log.Printf("[ximss] kill session: HTTP %d", resp.StatusCode)
		}
	}
	s.client.CloseIdleConnections()
}

func openSession(email, password string) (*ximssSession, error) {
	usernames := []string{email}
	if at := strings.IndexByte(email, '@'); at > 0 {
		usernames = append(usernames, email[:at])
	}

	var lastErr error
	for _, username := range usernames {
		for attempt := 0; attempt < 2; attempt++ {
			session, err := openSessionOnce(username, password)
			if err == nil {
				return session, nil
			}
			lastErr = err
			lowerErr := strings.ToLower(err.Error())
			if strings.Contains(lowerErr, "неверный логин или пароль") ||
				strings.Contains(lowerErr, "too many concurrent sessions") {
				break
			}
			// Repeating a TLS/connect timeout against the same unavailable
			// server only blocks the bot for another full request timeout.
			if isNetworkFailure(err) {
				return nil, fmt.Errorf("почтовый сервер недоступен: %w", err)
			}
			if attempt < 1 {
				time.Sleep(750 * time.Millisecond)
			}
		}
	}
	return nil, fmt.Errorf("почтовый сервер недоступен или учетные данные не приняты: %w", lastErr)
}

func isNetworkFailure(err error) bool {
	var networkErr net.Error
	return errors.As(err, &networkErr)
}

func openSessionOnce(username, password string) (*ximssSession, error) {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		ForceAttemptHTTP2:     false,
		TLSHandshakeTimeout:   12 * time.Second,
		ResponseHeaderTimeout: 20 * time.Second,
		DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
			return dialer.DialContext(ctx, "tcp4", addr)
		},
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, fmt.Errorf("cookie jar: %w", err)
	}
	c := &http.Client{Transport: transport, Timeout: 30 * time.Second, Jar: jar}

	resp, err := c.PostForm(BaseURL+"/XIMSSLogin/", url.Values{
		"userName":        {username},
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

	var r struct {
		XMLName xml.Name `xml:"XIMSS"`
		Session *struct {
			UrlID string `xml:"urlID,attr"`
		} `xml:"session"`
		Response *struct {
			Status    string `xml:"status,attr"`
			ErrorNum  string `xml:"errorNum,attr"`
			ErrorText string `xml:"errorText,attr"`
		} `xml:"response"`
	}

	if err := xml.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("parse error: %w", err)
	}
	if r.Response != nil && (r.Response.ErrorNum != "" || r.Response.ErrorText != "") {
		if r.Response.ErrorText != "" {
			return nil, fmt.Errorf("%s", r.Response.ErrorText)
		}
		return nil, fmt.Errorf("ошибка авторизации: %s", r.Response.ErrorNum)
	}
	if r.Session == nil || r.Session.UrlID == "" {
		return nil, fmt.Errorf("неверный логин или пароль")
	}

	return &ximssSession{urlID: r.Session.UrlID, client: c, username: username, password: password}, nil
}

func GetMaxUID(email, password string) (uint32, error) {
	unlock := lockAccountSession(email)
	defer unlock()

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
	unlock := lockAccountSession(email)
	defer unlock()

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
		result = append(result, m)
	}
	return result, nil
}

// GetInboxMessages returns recent inbox messages for the mail UI. Message
// bodies and attachments are intentionally not downloaded here; they are
// loaded only after the user opens a message.
func GetInboxMessages(email, password string) ([]Message, error) {
	unlock := lockAccountSession(email)
	defer unlock()

	s, err := openSession(email, password)
	if err != nil {
		return nil, err
	}
	defer s.close()
	// The mail UI intentionally keeps only a small recent window. Full inbox
	// scans are reserved for notification polling.
	return browseFolderRange(s, 0, 5)
}

// GetMessage loads a single message body and its attachments by UID.
func GetMessage(email, password string, uid uint32) (Message, error) {
	unlock := lockAccountSession(email)
	defer unlock()

	s, err := openSession(email, password)
	if err != nil {
		return Message{}, err
	}
	defer s.close()

	if _, err := openInboxFolder(s); err != nil {
		return Message{}, fmt.Errorf("open inbox: %w", err)
	}
	data, err := s.call(fmt.Sprintf(
		`<XIMSS><folderRead id="3" folder="%s" UID="%d" totalSizeLimit="52428800"/></XIMSS>`, inbox, uid,
	))
	if err != nil {
		return Message{}, fmt.Errorf("read message: %w", err)
	}
	if err := checkXIMSSResponse(data, "folderRead"); err != nil {
		return Message{}, err
	}

	msg := parseFolderMessage(data, uid)
	if msg.Subject == "" || msg.From == "" || msg.Body == "" {
		log.Printf("[ximss] folderRead UID=%d incomplete structure: %s", uid, summarizeXMLStructure(data))
	}
	if msg.Subject == "" {
		msg.Subject = "(без темы)"
	}
	if msg.From == "" {
		msg.From = "неизвестный отправитель"
	}
	atts, attachErr := fetchAttachments(s, msg)
	if attachErr != nil {
		return Message{}, fmt.Errorf("read attachments: %w", attachErr)
	}
	inline, inlineErr := fetchInlineImages(s, msg.Body)
	if inlineErr != nil {
		log.Printf("[ximss] inline images UID=%d: %v", uid, inlineErr)
	}
	msg.Attachments = append(atts, inline...)
	return msg, nil
}

func fetchInlineImages(s *ximssSession, body string) ([]Attachment, error) {
	matches := inlineImagePattern.FindAllStringSubmatch(body, -1)
	if len(matches) == 0 {
		return nil, nil
	}

	base, err := url.Parse(BaseURL)
	if err != nil {
		return nil, err
	}
	var images []Attachment
	for _, match := range matches {
		imageURL, err := url.Parse(strings.TrimRight(match[1], ".,;"))
		if err != nil || imageURL.Scheme != "https" || !strings.EqualFold(imageURL.Host, base.Host) {
			continue
		}
		if !isProtectedWebImagePath(imageURL.Path) {
			if data, contentType, ok := readInlineImageViaXIMSS(s, imageURL); ok {
				name := filepath.Base(imageURL.Path)
				if decoded, decodeErr := url.PathUnescape(name); decodeErr == nil {
					name = decoded
				}
				images = append(images, Attachment{Name: filepath.Base(name), ContentType: contentType, Data: data})
				continue
			}
		} else {
			// Protected web images are not downloadable through the regular
			// authenticated HTTP endpoint. Do not spend two 10-second timeouts
			// retrying a URL which the server will reject.
			log.Printf("[ximss] inline image skipped: protected URL %s", imageURL.Path)
			continue
		}
		resp, err := downloadInlineImage(s, imageURL)
		if err != nil {
			return images, fmt.Errorf("download %s: %w", imageURL.Path, err)
		}
		if resp.StatusCode != http.StatusOK {
			return images, fmt.Errorf("download %s: HTTP %d", imageURL.Path, resp.StatusCode)
		}
		contentType := strings.ToLower(strings.TrimSpace(strings.Split(resp.Header.Get("Content-Type"), ";")[0]))
		likelyImage := strings.HasPrefix(contentType, "image/") || isImagePath(imageURL.Path)
		if !likelyImage {
			resp.Body.Close()
			continue
		}
		if resp.ContentLength > maxAttachmentSize {
			resp.Body.Close()
			continue
		}
		data, readErr := io.ReadAll(io.LimitReader(resp.Body, maxAttachmentSize+1))
		resp.Body.Close()
		if readErr != nil {
			return images, fmt.Errorf("read %s: %w", imageURL.Path, readErr)
		}
		if int64(len(data)) > maxAttachmentSize {
			continue
		}
		if !strings.HasPrefix(contentType, "image/") {
			contentType = http.DetectContentType(data)
		}
		if !strings.HasPrefix(contentType, "image/") {
			continue
		}
		name := filepath.Base(imageURL.Path)
		if name == "." || name == "/" || name == "" {
			name = "inline-image"
		}
		if decoded, decodeErr := url.PathUnescape(name); decodeErr == nil {
			name = decoded
		}
		images = append(images, Attachment{Name: filepath.Base(name), ContentType: contentType, Data: data})
	}
	return images, nil
}

func readInlineImageViaXIMSS(s *ximssSession, imageURL *url.URL) ([]byte, string, bool) {
	fileName := strings.TrimPrefix(imageURL.Path, "/")
	if strings.HasPrefix(fileName, "~") {
		if slash := strings.IndexByte(fileName, '/'); slash >= 0 {
			fileName = fileName[slash+1:]
		}
	}
	candidates := []string{fileName}
	if strings.HasPrefix(fileName, "protected/") {
		candidates = append(candidates, strings.TrimPrefix(fileName, "protected/"))
	}
	for _, candidate := range candidates {
		request := fmt.Sprintf(`<XIMSS><fileRead id="11" fileName="%s" type="binary"/></XIMSS>`, escapeXMLAttribute(candidate))
		data, err := s.call(request)
		if err != nil {
			log.Printf("[ximss] inline fileRead %s: %v", candidate, err)
			continue
		}
		if responseErr := checkXIMSSResponse(data, "fileRead"); responseErr != nil {
			log.Printf("[ximss] inline fileRead %s: %v", candidate, responseErr)
			continue
		}
		var response struct {
			FileData *struct {
				Base64 string `xml:"base64"`
			} `xml:"fileData"`
		}
		if xml.Unmarshal(data, &response) != nil || response.FileData == nil || response.FileData.Base64 == "" {
			log.Printf("[ximss] inline fileRead %s: no binary fileData", candidate)
			continue
		}
		decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(response.FileData.Base64))
		if err != nil || len(decoded) == 0 || int64(len(decoded)) > maxAttachmentSize {
			continue
		}
		contentType := http.DetectContentType(decoded)
		if !strings.HasPrefix(contentType, "image/") {
			continue
		}
		return decoded, contentType, true
	}
	return nil, "", false
}

func escapeXMLAttribute(value string) string {
	return strings.NewReplacer("&", "&amp;", "\"", "&quot;", "<", "&lt;", ">", "&gt;").Replace(value)
}

func downloadInlineImage(s *ximssSession, imageURL *url.URL) (*http.Response, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, imageURL.String(), nil)
	if err != nil {
		return nil, err
	}
	request.SetBasicAuth(s.username, s.password)
	resp, err := s.client.Do(request)
	if err == nil && resp.StatusCode == http.StatusOK {
		return resp, nil
	}
	if resp != nil {
		resp.Body.Close()
	}

	fileName := strings.TrimPrefix(imageURL.Path, "/")
	if strings.HasPrefix(fileName, "~") {
		if slash := strings.IndexByte(fileName, '/'); slash >= 0 {
			fileName = fileName[slash+1:]
		}
	}
	if fileName == "" {
		return nil, err
	}
	segments := strings.Split(fileName, "/")
	for i := range segments {
		segments[i] = url.PathEscape(segments[i])
	}
	downloadURL := fmt.Sprintf("%s/Session/%s/DOWNLOAD/%s", BaseURL, url.PathEscape(s.urlID), strings.Join(segments, "/"))
	ctx, cancel = context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	request, err = http.NewRequestWithContext(ctx, http.MethodGet, downloadURL, nil)
	if err != nil {
		return nil, err
	}
	request.SetBasicAuth(s.username, s.password)
	return s.client.Do(request)
}

func isImagePath(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".jpg", ".jpeg", ".png", ".gif", ".webp", ".bmp", ".tif", ".tiff":
		return true
	default:
		return false
	}
}

func isProtectedWebImagePath(path string) bool {
	return strings.Contains(strings.ToLower(path), "/protected/settings/emailfiles/")
}

func ximssResponseError(data []byte) (errorNum, errorText string) {
	var envelope struct {
		Response *struct {
			ErrorNum  string `xml:"errorNum,attr"`
			ErrorText string `xml:"errorText,attr"`
		} `xml:"response"`
	}
	if xml.Unmarshal(data, &envelope) == nil && envelope.Response != nil {
		return envelope.Response.ErrorNum, envelope.Response.ErrorText
	}
	return "", ""
}

func checkXIMSSResponse(data []byte, operation string) error {
	if errorNum, errorText := ximssResponseError(data); errorNum != "" || errorText != "" {
		if errorText == "" {
			errorText = "сервер отклонил запрос"
		}
		if errorNum == "" {
			return fmt.Errorf("%s: %s", operation, errorText)
		}
		return fmt.Errorf("%s: %s (%s)", operation, errorText, errorNum)
	}
	return nil
}

func browseFolder(s *ximssSession) ([]Message, error) {
	return browseFolderRange(s, 0, 49)
}

func openInboxFolder(s *ximssSession) ([]byte, error) {
	data, err := s.call(
		`<XIMSS><folderOpen mailbox="INBOX" sortField="INTERNALDATE" sortOrder="desc" folder="INBOX-MM-1" id="1">` +
			`<field>FLAGS</field><field>Subject</field><field>E-From</field><field>Content-Type</field><field>INTERNALDATE</field>` +
			`</folderOpen></XIMSS>`,
	)
	if err != nil {
		return nil, err
	}
	if err := checkXIMSSResponse(data, "folderOpen"); err != nil {
		return nil, err
	}
	return data, nil
}

func browseFolderRange(s *ximssSession, from, till int) ([]Message, error) {
	openData, err := openInboxFolder(s)
	if err != nil {
		return nil, err
	}
	if till <= 0 {
		var opened struct {
			Reports []struct {
				Messages int `xml:"messages,attr"`
			} `xml:"folderReport"`
		}
		if xml.Unmarshal(openData, &opened) == nil {
			for _, report := range opened.Reports {
				if report.Messages > 0 {
					till = report.Messages - 1
					break
				}
			}
		}
		if till <= 0 {
			till = 0
		}
	}

	data, err := s.call(
		fmt.Sprintf(`<XIMSS><folderBrowse folder="%s" id="2"><index from="%d" till="%d"/></folderBrowse></XIMSS>`, inbox, from, till),
	)
	if err != nil {
		return nil, err
	}
	if err := checkXIMSSResponse(data, "folderBrowse"); err != nil {
		return nil, err
	}

	var r struct {
		XMLName xml.Name `xml:"XIMSS"`
		Reports []struct {
			UID         uint32 `xml:"UID,attr"`
			Subject     string `xml:"Subject"`
			From        string `xml:"E-From"`
			Date        string `xml:"INTERNALDATE"`
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
			Date:        rep.Date,
			ContentType: rep.ContentType,
		})
	}
	return msgs, nil
}

func parseFolderMessage(data []byte, uid uint32) Message {
	msg := Message{UID: uid}
	dec := xml.NewDecoder(strings.NewReader(string(data)))
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		start, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		var value string
		switch strings.ToLower(start.Name.Local) {
		case "subject":
			if dec.DecodeElement(&value, &start) == nil && msg.Subject == "" {
				msg.Subject = strings.TrimSpace(value)
			}
		case "e-from", "from":
			if dec.DecodeElement(&value, &start) == nil && msg.From == "" {
				address := strings.TrimSpace(value)
				realName := strings.TrimSpace(xmlAttribute(start, "realName"))
				if realName != "" && address != "" {
					msg.From = fmt.Sprintf("%s <%s>", realName, address)
				} else if realName != "" {
					msg.From = realName
				} else {
					msg.From = address
				}
			}
		case "internaldate", "date":
			if dec.DecodeElement(&value, &start) == nil && msg.Date == "" {
				msg.Date = strings.TrimSpace(value)
			}
		case "content-type":
			if dec.DecodeElement(&value, &start) == nil && msg.ContentType == "" {
				msg.ContentType = strings.TrimSpace(value)
			}
		case "mime":
			mimeType := strings.ToLower(xmlAttribute(start, "type"))
			mimeSubtype := strings.ToLower(xmlAttribute(start, "subtype"))
			partID := xmlAttribute(start, "partID")
			if partID == "" {
				partID = xmlAttribute(start, "id")
			}
			disposition := strings.ToLower(xmlAttribute(start, "disposition"))
			name := xmlAttribute(start, "Disposition-filename")
			if name == "" {
				name = xmlAttribute(start, "filename")
			}
			if partID != "" && (name != "" || disposition == "attachment" || disposition == "inline") {
				msg.mimeParts = append(msg.mimeParts, mimePart{ID: partID, Name: name})
			}
			if mimeType == "text" && (mimeSubtype == "plain" || (mimeSubtype == "html" && msg.Body == "")) {
				if dec.DecodeElement(&value, &start) == nil && msg.Body == "" {
					msg.Body = strings.TrimSpace(value)
				}
			}
		}
	}
	msg.Body = normalizeMailBody(msg.Body)
	return msg
}

func normalizeMailBody(body string) string {
	if !strings.Contains(body, "<") {
		return strings.TrimSpace(body)
	}
	body = htmlBreakPattern.ReplaceAllString(body, "\n")
	body = htmlTagPattern.ReplaceAllString(body, "")
	body = html.UnescapeString(body)
	lines := strings.Split(body, "\n")
	clean := make([]string, 0, len(lines))
	for _, line := range lines {
		clean = append(clean, strings.TrimSpace(line))
	}
	return strings.TrimSpace(strings.Join(clean, "\n"))
}

func xmlAttribute(element xml.StartElement, name string) string {
	for _, attr := range element.Attr {
		if strings.EqualFold(attr.Name.Local, name) {
			return attr.Value
		}
	}
	return ""
}

func summarizeXMLStructure(data []byte) string {
	dec := xml.NewDecoder(strings.NewReader(string(data)))
	parts := make([]string, 0, 24)
	for len(parts) < 24 {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		start, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		attrs := make([]string, 0, len(start.Attr))
		for _, attr := range start.Attr {
			attrs = append(attrs, attr.Name.Local)
		}
		if len(attrs) > 0 {
			parts = append(parts, fmt.Sprintf("%s[%s]", start.Name.Local, strings.Join(attrs, ",")))
		} else {
			parts = append(parts, start.Name.Local)
		}
	}
	return strings.Join(parts, " > ")
}

func fetchAttachments(s *ximssSession, message Message) ([]Attachment, error) {
	var attachments []Attachment
	if len(message.mimeParts) > 0 {
		for _, part := range message.mimeParts {
			att, skip, err := fetchPart(s, message.UID, part.ID, part.Name)
			if err != nil {
				log.Printf("[ximss] UID=%d MIME part %s: %v", message.UID, part.ID, err)
				continue
			}
			if !skip {
				attachments = append(attachments, att)
			}
		}
		return attachments, nil
	}
	for partNum := 1; partNum <= 20; partNum++ {
		att, skip, err := fetchPart(s, message.UID, fmt.Sprintf("%02d", partNum), "")
		if err != nil {
			continue
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
func fetchPart(s *ximssSession, uid uint32, partID, nameHint string) (Attachment, bool, error) {
	var resp *http.Response
	for _, enc := range []string{"B", "Q"} {
		u := fmt.Sprintf("%s/Session/%s/MIME/%s/%d-%s-%s/x",
			BaseURL, s.urlID, inbox, uid, partID, enc)
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
		return Attachment{}, false, fmt.Errorf("part %s not found", partID)
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
		log.Printf("[ximss] UID=%d part %s: skipping %s", uid, partID, baseType)
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
		filename = fmt.Sprintf("attachment-%s%s", partID, ext)
	}
	// Sanitize: strip any path components a malicious server might inject
	filename = filepath.Base(filename)

	// Check Content-Length before downloading
	if cl := resp.ContentLength; cl > maxAttachmentSize {
		log.Printf("[ximss] UID=%d part %s: too large (%d bytes)", uid, partID, cl)
		return Attachment{Name: filename, TooLarge: true, Size: cl}, false, nil
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxAttachmentSize))
	if err != nil || len(data) == 0 {
		log.Printf("[ximss] UID=%d part %s: read err=%v len=%d", uid, partID, err, len(data))
		return Attachment{}, true, nil
	}
	// If we read exactly the limit, the file was likely truncated
	if int64(len(data)) == maxAttachmentSize {
		log.Printf("[ximss] UID=%d part %s: hit size limit, treating as too large", uid, partID)
		return Attachment{Name: filename, TooLarge: true, Size: maxAttachmentSize}, false, nil
	}
	log.Printf("[ximss] attachment %s (%d bytes) UID=%d part=%s cd=%q", filename, len(data), uid, partID, cd)
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
