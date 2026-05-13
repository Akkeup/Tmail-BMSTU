package lks

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// FindGroupUUID searches the public university structure for a group by name.
// Returns the group UUID used in schedule API calls.
func FindGroupUUID(groupName string) (string, error) {
	resp, err := (&http.Client{Timeout: 10 * time.Second}).
		Get(apiBase + "/structure")
	if err != nil {
		return "", fmt.Errorf("structure fetch: %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)

	// Structure is a nested JSON tree; search recursively for the group name.
	var root struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(data, &root); err != nil {
		return "", fmt.Errorf("structure parse: %w", err)
	}
	uuid := searchGroup(root.Data, strings.ToUpper(strings.TrimSpace(groupName)))
	if uuid == "" {
		return "", fmt.Errorf("группа «%s» не найдена", groupName)
	}
	return uuid, nil
}

// searchGroup recursively walks the structure JSON tree looking for a group name.
func searchGroup(raw json.RawMessage, name string) string {
	// Try array
	var arr []json.RawMessage
	if json.Unmarshal(raw, &arr) == nil {
		for _, item := range arr {
			if u := searchGroup(item, name); u != "" {
				return u
			}
		}
		return ""
	}
	// Try object
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return ""
	}
	// Extract name and uuid fields
	var objName, uuid string
	if v, ok := obj["name"]; ok {
		json.Unmarshal(v, &objName)
	}
	if v, ok := obj["uuid"]; ok {
		json.Unmarshal(v, &uuid)
	}
	if strings.EqualFold(strings.TrimSpace(objName), name) && uuid != "" {
		return uuid
	}
	// Recurse into children / items
	for _, key := range []string{"children", "items", "groups"} {
		if v, ok := obj[key]; ok {
			if u := searchGroup(v, name); u != "" {
				return u
			}
		}
	}
	return ""
}

// GetTodaySchedule returns today's lessons for the given group UUID.
func GetTodaySchedule(groupUUID string) (string, error) {
	url := fmt.Sprintf("%s/schedules/groups/%s/public", apiBase, groupUUID)
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Get(url)
	if err != nil {
		return "", fmt.Errorf("schedule fetch: %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)

	return formatSchedule(data)
}

// --- JSON structures for the schedule response ---

type scheduleResponse struct {
	Data struct {
		Schedule []weekSchedule `json:"schedule"`
	} `json:"data"`
}

type weekSchedule struct {
	WeekIndex int       `json:"weekIndex"`
	Days      []dayData `json:"days"`
}

type dayData struct {
	Day     int      `json:"day"` // 1=Mon … 7=Sun or 0=Mon … 6=Sun — we'll handle both
	Lessons []lesson `json:"lessons"`
}

type lesson struct {
	StartTime string `json:"startTime"`
	EndTime   string `json:"endTime"`
	Name      string `json:"name"`
	Type      string `json:"type"`
	Teacher   string `json:"teacher"`
	Room      string `json:"room"`
	Building  string `json:"building"`
}

func formatSchedule(data []byte) (string, error) {
	var sr scheduleResponse
	if err := json.Unmarshal(data, &sr); err != nil {
		return "", fmt.Errorf("schedule parse: %w", err)
	}

	now := time.Now()
	weekday := int(now.Weekday()) // 0=Sun, 1=Mon … 6=Sat
	if weekday == 0 {
		return "Сегодня воскресенье — пар нет.", nil
	}

	// Find current week and today's lessons
	for _, week := range sr.Data.Schedule {
		for _, day := range week.Days {
			// API may use 1=Mon or 0=Mon, normalise both
			d := day.Day
			if d == 0 {
				continue
			}
			if d != weekday {
				continue
			}
			if len(day.Lessons) == 0 {
				return "Сегодня пар нет.", nil
			}
			return buildDayText(now, day.Lessons), nil
		}
	}
	return "Сегодня пар нет.", nil
}

func buildDayText(now time.Time, lessons []lesson) string {
	days := []string{"", "Понедельник", "Вторник", "Среда", "Четверг", "Пятница", "Суббота"}
	title := fmt.Sprintf("📅 Расписание на %s, %s\n\n",
		days[int(now.Weekday())],
		now.Format("02.01"),
	)
	var sb strings.Builder
	sb.WriteString(title)
	for _, l := range lessons {
		sb.WriteString(fmt.Sprintf("🕐 %s–%s  %s\n", l.StartTime, l.EndTime, l.Name))
		if l.Type != "" {
			sb.WriteString(fmt.Sprintf("   %s", l.Type))
		}
		if l.Teacher != "" {
			sb.WriteString(fmt.Sprintf(" · %s", l.Teacher))
		}
		if l.Room != "" {
			room := l.Room
			if l.Building != "" {
				room = l.Building + "-" + room
			}
			sb.WriteString(fmt.Sprintf(" · ауд. %s", room))
		}
		sb.WriteString("\n\n")
	}
	return strings.TrimRight(sb.String(), "\n")
}
