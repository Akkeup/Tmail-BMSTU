package lks

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"
)

// GetSportAttendance returns a formatted string with the user's FV attendance.
func GetSportAttendance(c *Client) (string, error) {
	// Try regular FV (military / general)
	regular, err := fetchMilitaryEvents(c)
	if err != nil {
		log.Printf("[lks/fv] military events error: %v", err)
	}

	// Try swim FV
	swim, err := fetchSwimEvents(c)
	if err != nil {
		log.Printf("[lks/fv] swim events error: %v", err)
	}

	if regular == "" && swim == "" {
		return "🏃 Физкультура\n\nДанных о посещениях не найдено.", nil
	}

	var sb strings.Builder
	sb.WriteString("🏃 Физкультура\n")
	if regular != "" {
		sb.WriteString("\n")
		sb.WriteString(regular)
	}
	if swim != "" {
		sb.WriteString("\n")
		sb.WriteString(swim)
	}
	return strings.TrimRight(sb.String(), "\n"), nil
}

type militaryEventsResp struct {
	Items []struct {
		Date   string `json:"date"`
		Points int    `json:"points"`
		Status string `json:"status"`
	} `json:"items"`
}

func fetchMilitaryEvents(c *Client) (string, error) {
	data, err := c.get("/fv/me/military_events")
	if err != nil {
		return "", err
	}
	log.Printf("[lks/fv] military_events: %s", data)

	var resp militaryEventsResp
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", fmt.Errorf("parse: %w", err)
	}
	if len(resp.Items) == 0 {
		return "", nil
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Физподготовка — %d посещений:\n", len(resp.Items)))
	totalPoints := 0
	for _, e := range resp.Items {
		line := fmt.Sprintf("• %s", e.Date)
		if e.Points > 0 {
			line += fmt.Sprintf(" (+%d б.)", e.Points)
			totalPoints += e.Points
		}
		if e.Status != "" {
			line += fmt.Sprintf(" [%s]", e.Status)
		}
		sb.WriteString(line + "\n")
	}
	if totalPoints > 0 {
		sb.WriteString(fmt.Sprintf("Итого баллов: %d\n", totalPoints))
	}
	return sb.String(), nil
}

type swimEventsResp struct {
	Items []struct {
		Date   string `json:"date"`
		Points int    `json:"points"`
		Status string `json:"status"`
	} `json:"items"`
}

func fetchSwimEvents(c *Client) (string, error) {
	data, err := c.get("/fv-swim/me/events")
	if err != nil {
		return "", err
	}
	log.Printf("[lks/fv] swim events: %s", data)

	var resp swimEventsResp
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", fmt.Errorf("parse: %w", err)
	}
	if len(resp.Items) == 0 {
		return "", nil
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Плавание — %d посещений:\n", len(resp.Items)))
	totalPoints := 0
	for _, e := range resp.Items {
		line := fmt.Sprintf("• %s", e.Date)
		if e.Points > 0 {
			line += fmt.Sprintf(" (+%d б.)", e.Points)
			totalPoints += e.Points
		}
		if e.Status != "" {
			line += fmt.Sprintf(" [%s]", e.Status)
		}
		sb.WriteString(line + "\n")
	}
	if totalPoints > 0 {
		sb.WriteString(fmt.Sprintf("Итого баллов: %d\n", totalPoints))
	}
	return sb.String(), nil
}
