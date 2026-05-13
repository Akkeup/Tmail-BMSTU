package lks

import (
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const apiBase = "https://lks.bmstu.ru/lks-back/api/v1"

// Client holds an authenticated LKS session (p4sess cookie).
type Client struct {
	hc *http.Client
}

// NewClient authenticates against LKS via Keycloak using BMSTU credentials.
func NewClient(email, password string) (*Client, error) {
	jar, _ := cookiejar.New(nil)
	hc := &http.Client{
		Timeout: 20 * time.Second,
		Jar:     jar,
	}

	// Use a realistic User-Agent — some Keycloak configs check it
	doGet := func(u string) (*http.Response, error) {
		req, _ := http.NewRequest(http.MethodGet, u, nil)
		req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36")
		return hc.Do(req)
	}

	// Step 1: kick off SSO — portal4 redirects to Keycloak
	resp, err := doGet("https://lks.bmstu.ru/portal4/cookie/login?back=/&profile_any=true")
	if err != nil {
		return nil, fmt.Errorf("lks connect: %w", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	// Step 2: extract the Keycloak login form action URL
	formAction := extractFormAction(string(body))
	if formAction == "" {
		return nil, fmt.Errorf("lks: keycloak login form not found (landed on %s)", resp.Request.URL)
	}

	// Step 3: collect all form fields (hidden inputs included)
	formFields := extractFormFields(string(body))

	username := email // Keycloak expects full email as username

	values := url.Values{}
	for k, v := range formFields {
		values.Set(k, v)
	}
	values.Set("username", username)
	values.Set("password", password)
	values.Set("credentialId", "")

	req, _ := http.NewRequest(http.MethodPost, formAction, strings.NewReader(values.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36")
	req.Header.Set("Referer", resp.Request.URL.String())

	resp2, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("lks auth post: %w", err)
	}
	io.Copy(io.Discard, resp2.Body)
	resp2.Body.Close()

	// Session cookies are __portal3_login and __portal3_info
	lksURL, _ := url.Parse("https://lks.bmstu.ru")
	cookies := jar.Cookies(lksURL)
	if len(cookies) > 0 {
		return &Client{hc: hc}, nil
	}

	return nil, fmt.Errorf("неверный логин или пароль")
}

// APIGet makes an authenticated GET to the LKS backend API (exported for diagnostics).
func APIGet(c *Client, path string) ([]byte, error) {
	return c.get(path)
}

// get makes an authenticated GET request to the LKS backend API.
func (c *Client) get(path string) ([]byte, error) {
	resp, err := c.hc.Get(apiBase + path)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("сессия истекла")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("lks api %s: status %d", path, resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

// extractKeycloakError pulls the visible error message from a Keycloak login page.
func extractKeycloakError(html string) string {
	patterns := []string{
		`(?s)<div[^>]*class="[^"]*alert[^"]*error[^"]*"[^>]*>.*?<p[^>]*>([^<]+)</p>`,
		`(?s)<span[^>]*class="[^"]*pf-m-error[^"]*"[^>]*>([^<]+)</span>`,
		`(?s)id="input-error"[^>]*>\s*([^<]+)<`,
		`(?s)<p[^>]*class="[^"]*instruction[^"]*"[^>]*>([^<]+)</p>`,
		`(?s)kc-feedback-text[^>]*>([^<]+)<`,
	}
	for _, p := range patterns {
		if m := regexp.MustCompile(p).FindStringSubmatch(html); len(m) >= 2 {
			return strings.TrimSpace(m[1])
		}
	}
	return ""
}

// extractFormFields parses all <input> fields from the Keycloak login form.
// This ensures hidden fields (execution, etc.) are included in the POST.
func extractFormFields(html string) map[string]string {
	fields := make(map[string]string)
	// Match <input ... name="..." ... value="..." ...>
	re := regexp.MustCompile(`<input[^>]+>`)
	for _, tag := range re.FindAllString(html, -1) {
		name := extractAttr(tag, "name")
		if name == "" {
			continue
		}
		fields[name] = extractAttr(tag, "value")
	}
	return fields
}

// extractAttr pulls a named attribute value from an HTML tag string.
func extractAttr(tag, attr string) string {
	re := regexp.MustCompile(`(?i)` + attr + `="([^"]*)"`)
	if m := re.FindStringSubmatch(tag); len(m) >= 2 {
		return m[1]
	}
	return ""
}

// extractFormAction finds the Keycloak login form action URL in the page HTML.
func extractFormAction(html string) string {
	// Keycloak login form has id="kc-form-login"; action can appear before or after id.
	// Strategy: find the kc-form-login form tag, then extract its action attribute.
	patterns := []string{
		// action before id
		`action="([^"]+)"[^>]*id="kc-form-login"`,
		// id before action
		`id="kc-form-login"[^>]*action="([^"]+)"`,
		// fallback: any action pointing to sso.bmstu.ru
		`action="(https://sso\.bmstu\.ru[^"]+)"`,
	}
	for _, p := range patterns {
		if m := regexp.MustCompile(p).FindStringSubmatch(html); len(m) >= 2 {
			s := strings.ReplaceAll(m[1], "&amp;", "&")
			s = strings.ReplaceAll(s, "&#x2F;", "/")
			return s
		}
	}
	return ""
}
