package storage

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// MailMessage is the lightweight mail metadata kept in the local cache.
// Bodies and attachments are intentionally fetched only when a message opens.
type MailMessage struct {
	UID         uint32
	Subject     string
	From        string
	Date        string
	ContentType string
}

// NewSQLite opens the application database and migrates users from the old
// encrypted JSON file once. The JSON file is deliberately left untouched as a
// recoverable backup and for compatibility with the broadcast utility.
func NewSQLite(path, legacyPath, secret string) (*Storage, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	if _, err := db.Exec(`PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000;`); err != nil {
		db.Close()
		return nil, fmt.Errorf("configure sqlite: %w", err)
	}
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS users (
			chat_id INTEGER PRIMARY KEY,
			email TEXT NOT NULL,
			password TEXT NOT NULL,
			last_uid INTEGER NOT NULL DEFAULT 0,
			group_name TEXT NOT NULL DEFAULT '',
			group_uuid TEXT NOT NULL DEFAULT '',
			updated_at INTEGER NOT NULL
		);
		CREATE TABLE IF NOT EXISTS mail_messages (
			chat_id INTEGER NOT NULL,
			uid INTEGER NOT NULL,
			subject TEXT NOT NULL DEFAULT '',
			from_addr TEXT NOT NULL DEFAULT '',
			date TEXT NOT NULL DEFAULT '',
			content_type TEXT NOT NULL DEFAULT '',
			cached_at INTEGER NOT NULL,
			PRIMARY KEY (chat_id, uid)
		);
		CREATE INDEX IF NOT EXISTS idx_mail_messages_chat_uid
			ON mail_messages(chat_id, uid DESC);
	`); err != nil {
		db.Close()
		return nil, fmt.Errorf("create sqlite schema: %w", err)
	}

	s := &Storage{path: path, users: make(map[int64]*User), key: secretKey(secret), db: db}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&count); err != nil {
		db.Close()
		return nil, fmt.Errorf("count sqlite users: %w", err)
	}
	if count == 0 {
		if legacy, err := New(legacyPath, secret); err == nil {
			for _, user := range legacy.All() {
				if err := s.setSQLite(&user); err != nil {
					db.Close()
					return nil, fmt.Errorf("migrate user %d: %w", user.ChatID, err)
				}
			}
		}
	}
	return s, nil
}

func (s *Storage) Close() error {
	if s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *Storage) setSQLite(user *User) error {
	password, err := s.encrypt(user.Password)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`
		INSERT INTO users(chat_id,email,password,last_uid,group_name,group_uuid,updated_at)
		VALUES(?,?,?,?,?,?,?)
		ON CONFLICT(chat_id) DO UPDATE SET
			email=excluded.email, password=excluded.password, last_uid=excluded.last_uid,
			group_name=excluded.group_name, group_uuid=excluded.group_uuid,
			updated_at=excluded.updated_at
	`, user.ChatID, user.Email, password, user.LastUID, user.Group, user.GroupUUID, time.Now().Unix())
	return err
}

func (s *Storage) GetMailCache(chatID int64, limit int) ([]MailMessage, time.Time, error) {
	if s.db == nil {
		return nil, time.Time{}, nil
	}
	rows, err := s.db.Query(`
		SELECT uid, subject, from_addr, date, content_type, cached_at
		FROM mail_messages WHERE chat_id=? ORDER BY uid DESC LIMIT ?
	`, chatID, limit)
	if err != nil {
		return nil, time.Time{}, err
	}
	defer rows.Close()
	var messages []MailMessage
	var cachedAt int64
	for rows.Next() {
		var message MailMessage
		if err := rows.Scan(&message.UID, &message.Subject, &message.From, &message.Date, &message.ContentType, &cachedAt); err != nil {
			return nil, time.Time{}, err
		}
		messages = append(messages, message)
	}
	if err := rows.Err(); err != nil {
		return nil, time.Time{}, err
	}
	if len(messages) == 0 {
		return nil, time.Time{}, nil
	}
	return messages, time.Unix(cachedAt, 0), nil
}

func (s *Storage) SaveMailCache(chatID int64, messages []MailMessage) error {
	if s.db == nil || len(messages) == 0 {
		return nil
	}
	now := time.Now().Unix()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	stmt, err := tx.Prepare(`
		INSERT INTO mail_messages(chat_id,uid,subject,from_addr,date,content_type,cached_at)
		VALUES(?,?,?,?,?,?,?)
		ON CONFLICT(chat_id,uid) DO UPDATE SET
			subject=excluded.subject, from_addr=excluded.from_addr, date=excluded.date,
			content_type=excluded.content_type, cached_at=excluded.cached_at
	`)
	if err != nil {
		tx.Rollback()
		return err
	}
	defer stmt.Close()
	for _, message := range messages {
		if _, err := stmt.Exec(chatID, message.UID, message.Subject, message.From, message.Date, message.ContentType, now); err != nil {
			tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}
