package storage

import (
	"path/filepath"
	"testing"
)

func TestStorageEncryptsAndReloadsPassword(t *testing.T) {
	path := filepath.Join(t.TempDir(), "users.json")
	store, err := New(path, "test-secret")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Set(&User{ChatID: 7, Email: "user@student.bmstu.ru", Password: "password"}); err != nil {
		t.Fatal(err)
	}
	reloaded, err := New(path, "test-secret")
	if err != nil {
		t.Fatal(err)
	}
	user, ok := reloaded.Get(7)
	if !ok || user.Password != "password" {
		t.Fatalf("unexpected reloaded user: %+v, %v", user, ok)
	}
}

func TestStorageRejectsChangedSecretForEncryptedPassword(t *testing.T) {
	path := filepath.Join(t.TempDir(), "users.json")
	store, err := New(path, "test-secret")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Set(&User{ChatID: 7, Password: "password"}); err != nil {
		t.Fatal(err)
	}
	if _, err := New(path, "different-secret"); err == nil {
		t.Fatal("expected changed secret to be rejected")
	}
}

func TestSQLiteMigratesUsersAndStoresMailCache(t *testing.T) {
	dir := t.TempDir()
	legacyPath := filepath.Join(dir, "users.json")
	legacy, err := New(legacyPath, "test-secret")
	if err != nil {
		t.Fatal(err)
	}
	if err := legacy.Set(&User{ChatID: 7, Email: "user@student.bmstu.ru", Password: "password", LastUID: 41}); err != nil {
		t.Fatal(err)
	}

	store, err := NewSQLite(filepath.Join(dir, "bot.db"), legacyPath, "test-secret")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	user, ok := store.Get(7)
	if !ok || user.Password != "password" || user.LastUID != 41 {
		t.Fatalf("unexpected migrated user: %+v, %v", user, ok)
	}
	if err := store.SaveMailCache(7, []MailMessage{{UID: 41, Subject: "cached"}}); err != nil {
		t.Fatal(err)
	}
	messages, _, err := store.GetMailCache(7, 6)
	if err != nil || len(messages) != 1 || messages[0].Subject != "cached" {
		t.Fatalf("unexpected cached mail: %+v, %v", messages, err)
	}
}
