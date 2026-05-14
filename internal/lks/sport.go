package lks

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// fvStageUUID — идентификатор текущего семестра ФВ.
// Меняется ~2 раза в год (начало осеннего и весеннего семестров).
// Можно переопределить через переменную среды FV_STAGE_UUID.
const fvStageUUIDDefault = "a486d8f5-491d-11ef-8068-005056aa673b"

func fvStageUUID() string {
	if v := os.Getenv("FV_STAGE_UUID"); v != "" {
		return v
	}
	return fvStageUUIDDefault
}

type fvMeResp struct {
	FullName            string `json:"fullName"`
	IsActive            bool   `json:"isActive"`
	StudyPoints         int    `json:"studyPoints"`
	StudyAttends        int    `json:"studyAttends"`
	HasPrevExam         bool   `json:"hasPrevExam"`
	HasCurrExam         bool   `json:"hasCurrExam"`
	MedGroup            string `json:"medGroup"`
	MedDate             string `json:"medDate"`
	MedSwim             bool   `json:"medSwim"`
	CanBookStudyDisplay string `json:"canBookStudyDisplay"`
	CanBookTailDisplay  string `json:"canBookTailDisplay"`
	Groups              []struct {
		Week        string `json:"week"`
		Time        string `json:"time"`
		TeacherName string `json:"teacherName"`
		Section     string `json:"section"`
		Place       string `json:"place"`
	} `json:"groups"`
}

// GetSportAttendance возвращает отформатированную строку с данными о посещаемости ФВ.
func GetSportAttendance(c *Client) (string, error) {
	data, err := c.get("/fv/" + fvStageUUID() + "/me")
	if err != nil {
		return "", fmt.Errorf("fv/me: %w", err)
	}

	var d fvMeResp
	if err := json.Unmarshal(data, &d); err != nil {
		return "", fmt.Errorf("fv parse: %w", err)
	}

	// Нет активных данных
	if !d.IsActive && d.StudyPoints == 0 && d.StudyAttends == 0 {
		return "🏃 Физкультура\n\nДанных о посещениях не найдено.", nil
	}

	var sb strings.Builder
	sb.WriteString("🏃 Физкультура\n\n")

	sb.WriteString(fmt.Sprintf("Баллы: %d\n", d.StudyPoints))
	sb.WriteString(fmt.Sprintf("Посещений: %d\n", d.StudyAttends))

	if d.MedGroup != "" {
		sb.WriteString(fmt.Sprintf("Мед. группа: %s\n", d.MedGroup))
	}

	// Статус зачёта
	switch {
	case d.HasCurrExam:
		sb.WriteString("Зачёт: ✅ сдан в текущем семестре\n")
	case d.HasPrevExam:
		sb.WriteString("Зачёт: ✅ сдан в прошлом семестре\n")
	default:
		sb.WriteString("Зачёт: ❌ не сдан\n")
	}

	// Записанные группы
	if len(d.Groups) > 0 {
		sb.WriteString("\nЗаписан на занятия:\n")
		for _, g := range d.Groups {
			line := fmt.Sprintf("• %s %s — %s", g.Week, g.Time, g.Section)
			if g.Place != "" {
				line += fmt.Sprintf(" (%s)", g.Place)
			}
			if g.TeacherName != "" {
				line += fmt.Sprintf("\n  %s", g.TeacherName)
			}
			sb.WriteString(line + "\n")
		}
	}

	return strings.TrimRight(sb.String(), "\n"), nil
}
