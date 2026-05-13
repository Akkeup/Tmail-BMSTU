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
// email must be the full address (e.g. abc@student.bmstu.ru); the local part
// is used as the Keycloak username.
func NewClient(email, password string) (*Client, error) {
	jar, _ := cookiejar.New(nil)
	hc := &http.Client{
		Timeout: 20 * time.Second,
		Jar:     jar,
	}

	// Step 1: kick off SSO — portal4 sets an initial p4sess and redirects to Keycloak
	resp, err := hc.Get("https://lks.bmstu.ru/portal4/cookie/login?back=/&profile_any=true")
	if err != nil {
		return nil, fmt.Errorf("lks connect: %w", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	// Step 2: extract the Keycloak login form action URL
	formAction := extractFormAction(string(body))
	if formAction == "" {
		return nil, fmt.Errorf("lks: keycloak login form not found")
	}

	// Step 3: POST credentials — Keycloak will redirect back to portal4 on success
	username := email
	if i := strings.Index(email, "@"); i > 0 {
		username = email[:i]
	}
	resp2, err := hc.PostForm(formAction, url.Values{
		"username":     {username},
		"password":     {password},
		"credentialId": {""},
	})
	if err != nil {
		return nil, fmt.Errorf("lks auth post: %w", err)
	}
	io.Copy(io.Discard, resp2.Body)
	resp2.Body.Close()

	// Step 4: verify the session cookie was set
	lksURL, _ := url.Parse("https://lks.bmstu.ru")
	for _, c := range jar.Cookies(lksURL) {
		if c.Name == "p4sess" {
			return &Client{hc: hc}, nil
		}
	}
	return nil, fmt.Errorf("неверный логин или пароль")
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

// extractFormAction finds the Keycloak login form action URL in the page HTML.
func extractFormAction(html string) string {
	// Keycloak renders <form id="kc-form-login" ... action="...">
	patterns := []string{
		`id="kc-form-login"[^>]*action="([^"]+)"`,
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
