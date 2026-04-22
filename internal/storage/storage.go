package storage

import (
	"encoding/json"
	"os"
	"sync"
)

type User struct {
	ChatID   int64  `json:"chat_id"`
	Email    string `json:"email"`
	Password string `json:"password"`
	LastUID  uint32 `json:"last_uid"`
}

type Storage struct {
	mu    sync.RWMutex
	path  string
	users map[int64]*User
}

func New(path string) (*Storage, error) {
	s := &Storage{
		path:  path,
		users: make(map[int64]*User),
	}
	if err := s.load(); err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	return s, nil
}

func (s *Storage) load() error {
	data, err := os.ReadFile(s.path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, &s.users)
}

func (s *Storage) save() error {
	data, err := json.MarshalIndent(s.users, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, data, 0600)
}

func (s *Storage) Set(user *User) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.users[user.ChatID] = user
	return s.save()
}

func (s *Storage) Get(chatID int64) (User, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	u, ok := s.users[chatID]
	if !ok {
		return User{}, false
	}
	return *u, true
}

func (s *Storage) Delete(chatID int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.users, chatID)
	return s.save()
}

func (s *Storage) All() []User {
	s.mu.RLock()
	defer s.mu.RUnlock()
	users := make([]User, 0, len(s.users))
	for _, u := range s.users {
		users = append(users, *u)
	}
	return users
}

func (s *Storage) UpdateLastUID(chatID int64, uid uint32) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if u, ok := s.users[chatID]; ok {
		u.LastUID = uid
		return s.save()
	}
	return nil
}
