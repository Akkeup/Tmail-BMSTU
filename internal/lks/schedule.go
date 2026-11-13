package lks

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	scheduleRequestTimeout = 30 * time.Second
	scheduleRequestRetries = 2
	scheduleCacheTTL       = 90 * time.Second
)

type scheduleCacheEntry struct {
	data      []byte
	expiresAt time.Time
}

var scheduleCache = struct {
	sync.RWMutex
	entries map[string]scheduleCacheEntry
}{entries: make(map[string]scheduleCacheEntry)}

var scheduleHTTPClient = &http.Client{Timeout: scheduleRequestTimeout}

// FindGroupUUID searches the public university structure for a group by name.
func FindGroupUUID(groupName string) (string, error) {
	data, err := getScheduleData(apiBase + "/structure")
	if err != nil {
		return "", fmt.Errorf("structure fetch: %w", err)
	}

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

	sr, err := fetchGroupSchedule(groupUUID)
	if err != nil {
		return "", err
	}

	return formatWeek(sr.Data.Title, sr.Data.Schedule, weekKey), nil
}

// GetDaySchedule returns the schedule for a single calendar date. It is used
// by the Today and Tomorrow buttons in the Telegram UI.
func GetDaySchedule(groupUUID string, date time.Time) (string, error) {
	weekKey, err := currentWeekKey()
	if err != nil {
		return "", err
	}
	if year, week := date.ISOWeek(); func() bool { y, w := time.Now().ISOWeek(); return y != year || w != week }() {
		// The API reports the current week's parity. Today/Tomorrow can cross
		// into the next week, whose parity is the opposite one.
		if weekKey == "ch" {
			weekKey = "zn"
		} else {
			weekKey = "ch"
		}
	}

	sr, err := fetchGroupSchedule(groupUUID)
	if err != nil {
		return "", err
	}
	day := int(date.Weekday())
	if day == 0 {
		return fmt.Sprintf("📅 %s\n\nНа этот день пар нет.", formatDate(date)), nil
	}

	var lessons []scheduleLesson
	for _, lesson := range sr.Data.Schedule {
		if lesson.Day == day && (lesson.Week == weekKey || lesson.Week == "all") {
			lessons = append(lessons, lesson)
		}
	}
	return formatDay(sr.Data.Title, lessons, weekKey, date), nil
}

func fetchGroupSchedule(groupUUID string) (groupScheduleResp, error) {
	url := fmt.Sprintf("%s/schedules/groups/%s/public", apiBase, groupUUID)
	data, err := getScheduleData(url)
	if err != nil {
		return groupScheduleResp{}, fmt.Errorf("schedule fetch: %w", err)
	}

	var sr groupScheduleResp
	if err := json.Unmarshal(data, &sr); err != nil {
		return groupScheduleResp{}, fmt.Errorf("schedule parse: %w", err)
	}
	return sr, nil
}

// currentWeekKey returns "ch" or "zn" for the current week.
func currentWeekKey() (string, error) {
	data, err := getScheduleData(apiBase + "/schedules/current")
	if err != nil {
		return "", fmt.Errorf("current week fetch: %w", err)
	}

	var cw currentWeekResp
	if err := json.Unmarshal(data, &cw); err != nil {
		return "", fmt.Errorf("current week parse: %w", err)
	}
	return normalizeWeekKey(cw.Data.WeekShortName)
}

func normalizeWeekKey(value string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "зн", "zn", "знаменатель":
		return "zn", nil
	case "чн", "ch", "числитель":
		return "ch", nil
	default:
		return "", fmt.Errorf("неизвестный тип недели: %q", value)
	}
}

// getScheduleData tolerates temporary LKS outages and slow responses. The
// schedule API is external to the bot, so a single missed response should not
// immediately become a user-visible failure.
func getScheduleData(endpoint string) ([]byte, error) {
	now := time.Now()
	scheduleCache.RLock()
	cached, ok := scheduleCache.entries[endpoint]
	scheduleCache.RUnlock()
	if ok && now.Before(cached.expiresAt) {
		return append([]byte(nil), cached.data...), nil
	}

	var lastErr error

	for attempt := 1; attempt <= scheduleRequestRetries; attempt++ {
		resp, err := scheduleHTTPClient.Get(endpoint)
		if err != nil {
			lastErr = err
		} else {
			data, readErr := io.ReadAll(resp.Body)
			resp.Body.Close()
			if readErr != nil {
				lastErr = readErr
			} else if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				scheduleCache.Lock()
				scheduleCache.entries[endpoint] = scheduleCacheEntry{
					data:      append([]byte(nil), data...),
					expiresAt: time.Now().Add(scheduleCacheTTL),
				}
				scheduleCache.Unlock()
				return data, nil
			} else if resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode < 500 {
				return nil, fmt.Errorf("status %d body=%s", resp.StatusCode, shortBody(data))
			} else {
				lastErr = fmt.Errorf("status %d body=%s", resp.StatusCode, shortBody(data))
			}
		}

		if attempt < scheduleRequestRetries {
			time.Sleep(500 * time.Millisecond)
		}
	}
	return nil, fmt.Errorf("after %d attempts: %w", scheduleRequestRetries, lastErr)
}

func shortBody(data []byte) string {
	const max = 200
	if len(data) > max {
		data = data[:max]
	}
	return strings.TrimSpace(string(data))
}

func formatWeek(title string, lessons []scheduleLesson, weekKey string) string {
	dayNames := []string{"", "Понедельник", "Вторник", "Среда", "Четверг", "Пятница", "Суббота"}

	// Filter by current week; "all" means every week
	byDay := make(map[int][]scheduleLesson)
	for _, l := range lessons {
		if l.Week != weekKey && l.Week != "all" {
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

func formatDay(title string, lessons []scheduleLesson, weekKey string, date time.Time) string {
	weekLabel := "числитель"
	if weekKey == "zn" {
		weekLabel = "знаменатель"
	}
	header := fmt.Sprintf("📅 %s\n%s · %s", formatDate(date), weekLabel, title)
	if len(lessons) == 0 {
		return header + "\n\nНа этот день пар нет."
	}

	sort.Slice(lessons, func(i, j int) bool { return lessons[i].Time < lessons[j].Time })
	var sb strings.Builder
	sb.WriteString(header)
	for _, l := range lessons {
		name := l.Discipline.ShortName
		if name == "" {
			name = l.Discipline.FullName
		}
		sb.WriteString(fmt.Sprintf("\n\n%s–%s\n%s", l.StartTime, l.EndTime, name))
		if actType := formatActType(l.Discipline.ActType); actType != "" {
			sb.WriteString("\n" + actType)
		}
		if len(l.Teachers) > 0 {
			t := l.Teachers[0]
			sb.WriteString(fmt.Sprintf(" · %s %s %s", t.LastName, t.FirstName, t.MiddleName))
		}
		if len(l.Audiences) > 0 && l.Audiences[0].Name != "" {
			sb.WriteString(fmt.Sprintf("\nауд. %s", l.Audiences[0].Name))
		}
	}
	return sb.String()
}

func formatDate(date time.Time) string {
	dayNames := []string{"Воскресенье", "Понедельник", "Вторник", "Среда", "Четверг", "Пятница", "Суббота"}
	monthNames := []string{"января", "февраля", "марта", "апреля", "мая", "июня", "июля", "августа", "сентября", "октября", "ноября", "декабря"}
	return fmt.Sprintf("%s, %d %s", dayNames[int(date.Weekday())], date.Day(), monthNames[int(date.Month())-1])
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
