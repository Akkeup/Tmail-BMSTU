package mailer

import (
	"encoding/xml"
	"net/url"
	"strings"
	"testing"
)

func TestParseFolderMessage(t *testing.T) {
	data := []byte(`<XIMSS><folderMessage folder="INBOX-MM-1" UID="42"><EMail><From realName="Отправитель">sender@example.com</From><Subject>Тема</Subject><Date>2026-11-12</Date><MIME type="text" subtype="plain">Текст письма</MIME></EMail></folderMessage></XIMSS>`)
	message := parseFolderMessage(data, 42)
	if message.UID != 42 || message.From != "Отправитель <sender@example.com>" || message.Subject != "Тема" || message.Body != "Текст письма" {
		t.Fatalf("unexpected parsed message: %+v", message)
	}
}

func TestParseFolderMessageNormalizesHTMLAndFindsMIMEPart(t *testing.T) {
	data := []byte(`<XIMSS><folderMessage><EMail><Subject>Квитанция</Subject><MIME type="text" subtype="html">&lt;html&gt;&lt;body&gt;Добрый день&lt;br&gt;&lt;strong&gt;Оплата&lt;/strong&gt;&lt;/body&gt;&lt;/html&gt;</MIME><MIME type="application" subtype="pdf" partID="2" disposition="attachment" Disposition-filename="receipt.pdf"/></EMail></folderMessage></XIMSS>`)
	message := parseFolderMessage(data, 42)
	if message.Body != "Добрый день\nОплата" {
		t.Fatalf("unexpected normalized body: %q", message.Body)
	}
	if len(message.mimeParts) != 1 || message.mimeParts[0].ID != "2" || message.mimeParts[0].Name != "receipt.pdf" {
		t.Fatalf("unexpected MIME parts: %+v", message.mimeParts)
	}
}

func TestInlineImagePatternKeepsParenthesesInFilename(t *testing.T) {
	match := inlineImagePattern.FindStringSubmatch("[img] https://student.bmstu.ru/path/foto(6).png")
	if len(match) != 2 || match[1] != "https://student.bmstu.ru/path/foto(6).png" {
		t.Fatalf("unexpected inline image match: %#v", match)
	}
}

func TestInlineImagePatternJoinsWrappedFilename(t *testing.T) {
	match := inlineImagePattern.FindStringSubmatch("[img] https://student.bmstu.ru/path/foto(6\n).png")
	if len(match) != 2 || match[1] != "https://student.bmstu.ru/path/foto(6\n).png" {
		t.Fatalf("unexpected wrapped inline image match: %#v", match)
	}
}

func TestInlineImageDownloadPath(t *testing.T) {
	imageURL, err := url.Parse("https://student.bmstu.ru/~zacepin_is/protected/settings/emailFiles/pwd/foto(6).png")
	if err != nil {
		t.Fatal(err)
	}
	path := strings.TrimPrefix(imageURL.Path, "/")
	if slash := strings.IndexByte(path, '/'); strings.HasPrefix(path, "~") && slash >= 0 {
		path = path[slash+1:]
	}
	if path != "protected/settings/emailFiles/pwd/foto(6).png" {
		t.Fatalf("unexpected file path: %s", path)
	}
}

func TestEscapeXMLAttribute(t *testing.T) {
	if got := escapeXMLAttribute(`a&b"c`); got != "a&amp;b&quot;c" {
		t.Fatalf("unexpected escaped attribute: %s", got)
	}
}

func TestProtectedWebImagePath(t *testing.T) {
	if !isProtectedWebImagePath("/~account/protected/settings/emailFiles/pwd/photo.png") {
		t.Fatal("expected protected web image path")
	}
	if isProtectedWebImagePath("/public/photo.png") {
		t.Fatal("public image path must not be treated as protected")
	}
}

func TestLoginResponseAcceptsPositiveResponseWithSession(t *testing.T) {
	data := `<XIMSS><session urlID="session-123"/><response id="A001"/></XIMSS>`
	var r struct {
		Session *struct {
			URLID string `xml:"urlID,attr"`
		} `xml:"session"`
		Response *struct {
			ErrorNum  string `xml:"errorNum,attr"`
			ErrorText string `xml:"errorText,attr"`
		} `xml:"response"`
	}
	if err := xml.Unmarshal([]byte(data), &r); err != nil {
		t.Fatalf("decode login response: %v", err)
	}
	if r.Session == nil || r.Session.URLID != "session-123" || r.Response == nil || r.Response.ErrorText != "" {
		t.Fatalf("unexpected login response: %+v", r)
	}
}

func TestXIMSSResponseError(t *testing.T) {
	num, text := ximssResponseError([]byte(`<XIMSS><response errorNum="550" errorText="Message not found"/></XIMSS>`))
	if num != "550" || text != "Message not found" {
		t.Fatalf("unexpected response error: %q, %q", num, text)
	}
	if err := checkXIMSSResponse([]byte(`<XIMSS><response errorNum="550" errorText="Message not found"/></XIMSS>`), "folderRead"); err == nil || err.Error() != "folderRead: Message not found (550)" {
		t.Fatalf("unexpected checked response error: %v", err)
	}
}
