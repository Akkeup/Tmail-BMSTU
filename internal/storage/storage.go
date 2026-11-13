package storage

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type User struct {
	ChatID    int64  `json:"chat_id"`
	Email     string `json:"email"`
	Password  string `json:"password"`
	LastUID   uint32 `json:"last_uid"`
	Group     string `json:"group,omitempty"`
	GroupUUID string `json:"group_uuid,omitempty"`
}

type Storage struct {
	mu    sync.RWMutex
	path  string
	users map[int64]*User
	key   []byte
	db    *sql.DB
}

func New(path, secret string) (*Storage, error) {
	s := &Storage{
		path:  path,
		users: make(map[int64]*User),
		key:   secretKey(secret),
	}
	if err := s.load(); err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	// Re-save immediately to encrypt any plaintext passwords from older versions
	if len(s.users) > 0 {
		if err := s.save(); err != nil {
			return nil, fmt.Errorf("migrate passwords: %w", err)
		}
	}
	return s, nil
}

func secretKey(secret string) []byte {
	h := sha256.Sum256([]byte(secret))
	return h[:]
}

func (s *Storage) encrypt(plaintext string) (string, error) {
	block, err := aes.NewCipher(s.key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	sealed := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return base64.StdEncoding.EncodeToString(sealed), nil
}

func (s *Storage) decrypt(encoded string) (string, error) {
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(s.key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(data) < gcm.NonceSize() {
		return "", fmt.Errorf("ciphertext too short")
	}
	nonce, ct := data[:gcm.NonceSize()], data[gcm.NonceSize():]
	plain, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

// on-disk format keeps passwords encrypted
type diskUser struct {
	ChatID    int64  `json:"chat_id"`
	Email     string `json:"email"`
	Password  string `json:"password"` // AES-256-GCM + base64
	LastUID   uint32 `json:"last_uid"`
	Group     string `json:"group,omitempty"`
	GroupUUID string `json:"group_uuid,omitempty"`
}

func (s *Storage) load() error {
	data, err := os.ReadFile(s.path)
	if err != nil {
		return err
	}
	var raw map[int64]*diskUser
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	for id, u := range raw {
		pwd, err := s.decrypt(u.Password)
		if err != nil {
			if looksLikeEncryptedPassword(u.Password) {
				return fmt.Errorf("не удалось расшифровать пароль пользователя %d: проверь SECRET_KEY", id)
			}
			// backward compat: file written before encryption was added
			pwd = u.Password
		}
		s.users[id] = &User{
			ChatID:    u.ChatID,
			Email:     u.Email,
			Password:  pwd,
			LastUID:   u.LastUID,
			Group:     u.Group,
			GroupUUID: u.GroupUUID,
		}
	}
	return nil
}

func looksLikeEncryptedPassword(value string) bool {
	data, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		return false
	}
	// AES-GCM stores a 12-byte nonce and at least a 16-byte authentication tag.
	return len(data) >= 28
}

func (s *Storage) save() error {
	disk := make(map[int64]*diskUser, len(s.users))
	for id, u := range s.users {
		enc, err := s.encrypt(u.Password)
		if err != nil {
			return fmt.Errorf("encrypt: %w", err)
		}
		disk[id] = &diskUser{
			ChatID:    u.ChatID,
			Email:     u.Email,
			Password:  enc,
			LastUID:   u.LastUID,
			Group:     u.Group,
			GroupUUID: u.GroupUUID,
		}
	}
	out, err := json.MarshalIndent(disk, "", "  ")
	if err != nil {
		return err
	}
	// Write atomically: temp file in same directory, then rename
	tmp := filepath.Join(filepath.Dir(s.path), ".users.json.tmp")
	if err := os.WriteFile(tmp, out, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func (s *Storage) Set(user *User) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db != nil {
		return s.setSQLite(user)
	}
	s.users[user.ChatID] = user
	return s.save()
}

func (s *Storage) Get(chatID int64) (User, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.db != nil {
		var user User
		var encrypted string
		err := s.db.QueryRow(`SELECT chat_id,email,password,last_uid,group_name,group_uuid FROM users WHERE chat_id=?`, chatID).
			Scan(&user.ChatID, &user.Email, &encrypted, &user.LastUID, &user.Group, &user.GroupUUID)
		if err != nil {
			return User{}, false
		}
		password, err := s.decrypt(encrypted)
		if err != nil {
			return User{}, false
		}
		user.Password = password
		return user, true
	}
	u, ok := s.users[chatID]
	if !ok {
		return User{}, false
	}
	return *u, true
}

func (s *Storage) Delete(chatID int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db != nil {
		_, err := s.db.Exec(`DELETE FROM users WHERE chat_id=?`, chatID)
		if err == nil {
			_, err = s.db.Exec(`DELETE FROM mail_messages WHERE chat_id=?`, chatID)
		}
		return err
	}
	delete(s.users, chatID)
	return s.save()
}

func (s *Storage) All() []User {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.db != nil {
		rows, err := s.db.Query(`SELECT chat_id,email,password,last_uid,group_name,group_uuid FROM users`)
		if err != nil {
			return nil
		}
		defer rows.Close()
		users := make([]User, 0)
		for rows.Next() {
			var user User
			var encrypted string
			if rows.Scan(&user.ChatID, &user.Email, &encrypted, &user.LastUID, &user.Group, &user.GroupUUID) != nil {
				continue
			}
			user.Password, _ = s.decrypt(encrypted)
			users = append(users, user)
		}
		return users
	}
	users := make([]User, 0, len(s.users))
	for _, u := range s.users {
		users = append(users, *u)
	}
	return users
}

func (s *Storage) UpdateLastUID(chatID int64, uid uint32) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db != nil {
		_, err := s.db.Exec(`UPDATE users SET last_uid=?, updated_at=? WHERE chat_id=?`, uid, time.Now().Unix(), chatID)
		return err
	}
	if u, ok := s.users[chatID]; ok {
		u.LastUID = uid
		return s.save()
	}
	return nil
}

func (s *Storage) UpdateGroup(chatID int64, group, groupUUID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db != nil {
		_, err := s.db.Exec(`UPDATE users SET group_name=?, group_uuid=?, updated_at=? WHERE chat_id=?`, group, groupUUID, time.Now().Unix(), chatID)
		return err
	}
	if u, ok := s.users[chatID]; ok {
		u.Group = group
		u.GroupUUID = groupUUID
		return s.save()
	}
	return nil
}
