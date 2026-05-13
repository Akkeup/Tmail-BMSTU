package lks

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// FindGroupUUID searches the public university structure for a group by name.
func FindGroupUUID(groupName string) (string, error) {
	resp, err := (&http.Client{Timeout: 10 * time.Second}).
		Get(apiBase + "/structure")
	if err != nil {
		return "", fmt.Errorf("structure fetch: %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)

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

func searchGroup(raw json.RawMessage, name string) string {
	var arr []json.RawMessage
	if json.Unmarshal(raw, &arr) == nil {
		for _, item := range arr {
			if u := searchGroup(item, name); u != "" {
				return u
			}
		}
		return ""
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return ""
	}
	var objName, objAbbr, uuid string
	if v, ok := obj["name"]; ok {
		json.Unmarshal(v, &objName)
	}
	if v, ok := obj["abbr"]; ok {
		json.Unmarshal(v, &objAbbr)
	}
	if v, ok := obj["uuid"]; ok {
		json.Unmarshal(v, &uuid)
	}
	if uuid != "" && (strings.EqualFold(strings.TrimSpace(objName), name) ||
		strings.EqualFold(strings.TrimSpace(objAbbr), name)) {
		return uuid
	}
	for _, key := range []string{"children", "items", "groups"} {
		if v, ok := obj[key]; ok {
			if u := searchGroup(v, name); u != "" {
				return u
			}
		}
	}
	return ""
}

// --- Schedule ---

type currentWeekResp struct {
	Data struct {
		WeekShortName string `json:"weekShortName"` // "чн" or "зн"
	} `json:"data"`
}

type scheduleLesson struct {
	Day       int    `json:"day"`       // 1=Mon … 6=Sat
	Time      int    `json:"time"`      // lesson slot (1, 2, …)
	Week      string `json:"week"`      // "ch" (числитель) or "zn" (знаменатель)
	StartTime string `json:"startTime"` // "09:00"
	EndTime   string `json:"endTime"`   // "10:35"
	Teachers  []struct {
		LastName   string `json:"lastName"`
		FirstName  string `json:"firstName"`
		MiddleName string `json:"middleName"`
	} `json:"teachers"`
	Audiences []struct {
		Name string `json:"name"`
	} `json:"audiences"`
	Discipline struct {
		FullName  string `json:"fullName"`
		ShortName string `json:"shortName"`
		ActType   string `json:"actType"`
	} `json:"discipline"`
}

type groupScheduleResp struct {
	Data struct {
		Title    string           `json:"title"`
		Schedule []scheduleLesson `json:"schedule"`
	} `json:"data"`
}

// GetWeekSchedule returns the full current-week schedule for the given group UUID.
func GetWeekSchedule(groupUUID string) (string, error) {
	// Fetch current week type ("чн" or "зн")
	weekKey, err := currentWeekKey()
	if err != nil {
		return "", err
	}

	// Fetch group schedule
	url := fmt.Sprintf("%s/schedules/groups/%s/public", apiBase, groupUUID)
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Get(url)
	if err != nil {
		return "", fmt.Errorf("schedule fetch: %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)

	var sr groupScheduleResp
	if err := json.Unmarshal(data, &sr); err != nil {
		return "", fmt.Errorf("schedule parse: %w", err)
	}

	return formatWeek(sr.Data.Title, sr.Data.Schedule, weekKey), nil
}

// currentWeekKey returns "ch" or "zn" for the current week.
func currentWeekKey() (string, error) {
	resp, err := (&http.Client{Timeout: 10 * time.Second}).
		Get(apiBase + "/schedules/current")
	if err != nil {
		return "", fmt.Errorf("current week fetch: %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)

	var cw currentWeekResp
	if err := json.Unmarshal(data, &cw); err != nil {
		return "", fmt.Errorf("current week parse: %w", err)
	}
	// weekShortName: "чн" → "ch", "зн" → "zn"
	if strings.Contains(cw.Data.WeekShortName, "зн") {
		return "zn", nil
	}
	return "ch", nil
}

func formatWeek(title string, lessons []scheduleLesson, weekKey string) string {
	dayNames := []string{"", "Понедельник", "Вторник", "Среда", "Четверг", "Пятница", "Суббота"}

	// Filter by current week, group by day
	byDay := make(map[int][]scheduleLesson)
	for _, l := range lessons {
		if l.Week != weekKey {
			continue
		}
		byDay[l.Day] = append(byDay[l.Day], l)
	}

	if len(byDay) == 0 {
		return fmt.Sprintf("📅 Расписание %s\n\nНа этой неделе пар нет.", title)
	}

	// Sort days
	days := make([]int, 0, len(byDay))
	for d := range byDay {
		days = append(days, d)
	}
	sort.Ints(days)

	weekLabel := "числитель"
	if weekKey == "zn" {
		weekLabel = "знаменатель"
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("📅 Расписание %s — %s\n", title, weekLabel))

	for _, d := range days {
		if d < 1 || d > 6 {
			continue
		}
		sb.WriteString(fmt.Sprintf("\n%s:\n", dayNames[d]))

		// Sort lessons by slot
		dayLessons := byDay[d]
		sort.Slice(dayLessons, func(i, j int) bool {
			return dayLessons[i].Time < dayLessons[j].Time
		})

		for _, l := range dayLessons {
			name := l.Discipline.ShortName
			if name == "" {
				name = l.Discipline.FullName
			}
			sb.WriteString(fmt.Sprintf("  🕐 %s–%s  %s", l.StartTime, l.EndTime, name))

			if actType := formatActType(l.Discipline.ActType); actType != "" {
				sb.WriteString(fmt.Sprintf(" (%s)", actType))
			}

			if len(l.Teachers) > 0 {
				t := l.Teachers[0]
				teacher := t.LastName
				if t.FirstName != "" {
					teacher += " " + string([]rune(t.FirstName)[:1]) + "."
				}
				if t.MiddleName != "" {
					teacher += string([]rune(t.MiddleName)[:1]) + "."
				}
				sb.WriteString(fmt.Sprintf(" · %s", teacher))
			}

			if len(l.Audiences) > 0 && l.Audiences[0].Name != "" {
				sb.WriteString(fmt.Sprintf(" · ауд. %s", l.Audiences[0].Name))
			}
			sb.WriteString("\n")
		}
	}

	return strings.TrimRight(sb.String(), "\n")
}

func formatActType(t string) string {
	switch t {
	case "lecture":
		return "лек"
	case "seminar":
		return "сем"
	case "lab":
		return "лаб"
	case "practice":
		return "пр"
	default:
		return ""
	}
}
