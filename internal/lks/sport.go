package lks

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"
)

// GetSportAttendance returns a formatted string with the user's FV
// (физвоспитание) attendance and points for the current stage.
func GetSportAttendance(c *Client) (string, error) {
	// First discover available stages
	stageUUID, err := getCurrentStageUUID(c)
	if err != nil {
		return "", err
	}
	return fetchAttendance(c, stageUUID)
}

// getCurrentStageUUID tries several endpoints to find the active FV stage UUID.
func getCurrentStageUUID(c *Client) (string, error) {
	// Attempt 1: /fv — might list stages
	data, err := c.get("/fv")
	if err == nil {
		log.Printf("[lks/fv] GET /fv: %s", data)
		if uuid := extractFirstUUID(data); uuid != "" {
			return uuid, nil
		}
	}

	// Attempt 2: /fv/stages
	data, err = c.get("/fv/stages")
	if err == nil {
		log.Printf("[lks/fv] GET /fv/stages: %s", data)
		if uuid := extractFirstUUID(data); uuid != "" {
			return uuid, nil
		}
	}

	return "", fmt.Errorf("не удалось определить текущий этап ФВ — попробуй позже")
}

// extractFirstUUID tries to pull the first uuid field from arbitrary JSON.
func extractFirstUUID(data []byte) string {
	var raw interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return ""
	}
	return findUUIDField(raw)
}

func findUUIDField(v interface{}) string {
	switch val := v.(type) {
	case map[string]interface{}:
		if u, ok := val["uuid"].(string); ok && u != "" {
			return u
		}
		for _, child := range val {
			if u := findUUIDField(child); u != "" {
				return u
			}
		}
	case []interface{}:
		for _, item := range val {
			if u := findUUIDField(item); u != "" {
				return u
			}
		}
	}
	return ""
}

// --- Attendance response ---

type fvMeResponse struct {
	Data struct {
		// Common fields — actual names depend on API; log raw for first run.
		Visits     *int     `json:"visits"`
		VisitCount *int     `json:"visitCount"`
		Points     *float64 `json:"points"`
		Score      *float64 `json:"score"`
		Passed     *bool    `json:"passed"`
		Status     string   `json:"status"`
		Events     []struct {
			Date   string `json:"date"`
			Points int    `json:"points"`
		} `json:"events"`
	} `json:"data"`
}

func fetchAttendance(c *Client, stageUUID string) (string, error) {
	data, err := c.get("/fv/" + stageUUID + "/me")
	if err != nil {
		return "", fmt.Errorf("FV данные: %w", err)
	}
	log.Printf("[lks/fv] GET /fv/%s/me: %s", stageUUID, data)

	var resp fvMeResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", fmt.Errorf("FV parse: %w", err)
	}

	var sb strings.Builder
	sb.WriteString("🏃 Физкультура — текущий семестр\n\n")

	d := resp.Data

	// Visits
	visits := 0
	if d.Visits != nil {
		visits = *d.Visits
	} else if d.VisitCount != nil {
		visits = *d.VisitCount
	}
	sb.WriteString(fmt.Sprintf("Посещений: %d\n", visits))

	// Points
	points := 0.0
	if d.Points != nil {
		points = *d.Points
	} else if d.Score != nil {
		points = *d.Score
	}
	sb.WriteString(fmt.Sprintf("Баллов: %.1f\n", points))

	// Status
	if d.Status != "" {
		sb.WriteString(fmt.Sprintf("Статус: %s\n", d.Status))
	} else if d.Passed != nil {
		if *d.Passed {
			sb.WriteString("Статус: зачтено ✅\n")
		} else {
			sb.WriteString("Статус: не зачтено ❌\n")
		}
	}

	// Last visits list
	if len(d.Events) > 0 {
		sb.WriteString("\nПоследние посещения:\n")
		limit := len(d.Events)
		if limit > 5 {
			limit = 5
		}
		for _, e := range d.Events[:limit] {
			line := fmt.Sprintf("• %s", e.Date)
			if e.Points > 0 {
				line += fmt.Sprintf(" (+%d б.)", e.Points)
			}
			sb.WriteString(line + "\n")
		}
	}

	return strings.TrimRight(sb.String(), "\n"), nil
}
