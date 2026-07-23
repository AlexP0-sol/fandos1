package telegram

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestMemorySessionStore_CreateValidate — счастливый путь: создание и валидация сессии.
func TestMemorySessionStore_CreateValidate(t *testing.T) {
	store := NewMemorySessionStore()
	ctx := context.Background()

	token, expiresAt, err := store.Create(ctx, 12345, time.Hour)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if token == "" {
		t.Fatal("Create: пустой токен")
	}
	if expiresAt.Before(time.Now()) {
		t.Fatalf("expiresAt в прошлом: %v", expiresAt)
	}

	userID, err := store.Validate(ctx, token)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if userID != 12345 {
		t.Fatalf("userID = %d, хотим 12345", userID)
	}
}

// TestMemorySessionStore_InvalidToken — несуществующий токен → ErrSessionNotFound.
func TestMemorySessionStore_InvalidToken(t *testing.T) {
	store := NewMemorySessionStore()
	ctx := context.Background()

	_, err := store.Validate(ctx, "несуществующий-токен")
	if !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("ожидали ErrSessionNotFound, получили %v", err)
	}
}

// TestMemorySessionStore_Expiry — просроченная сессия → ErrSessionNotFound.
func TestMemorySessionStore_Expiry(t *testing.T) {
	store := NewMemorySessionStore()
	ctx := context.Background()

	// Манипулируем временем: создаём сессию с TTL=1s, перематываем часы вперёд.
	now := time.Now()
	store.nowFunc = func() time.Time { return now }

	token, _, err := store.Create(ctx, 99, time.Second)
	if err != nil {
		t.Fatal(err)
	}

	// Перематываем время на 2 секунды вперёд.
	store.nowFunc = func() time.Time { return now.Add(2 * time.Second) }

	_, err = store.Validate(ctx, token)
	if !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("ожидали ErrSessionNotFound для просроченного токена, получили %v", err)
	}
}

// TestMemorySessionStore_Revoke — отозванная сессия → ErrSessionRevoked.
func TestMemorySessionStore_Revoke(t *testing.T) {
	store := NewMemorySessionStore()
	ctx := context.Background()

	token, _, err := store.Create(ctx, 42, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	if err := store.Revoke(ctx, token); err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	_, err = store.Validate(ctx, token)
	if !errors.Is(err, ErrSessionRevoked) {
		t.Fatalf("ожидали ErrSessionRevoked, получили %v", err)
	}
}

// TestMemorySessionStore_RevokeNonExistent — отзыв несуществующего токена не возвращает ошибку.
func TestMemorySessionStore_RevokeNonExistent(t *testing.T) {
	store := NewMemorySessionStore()
	if err := store.Revoke(context.Background(), "нет-такого"); err != nil {
		t.Fatalf("Revoke несуществующего: %v", err)
	}
}

// TestMemorySessionStore_TokenUniqueness — каждый Create генерирует уникальный токен.
func TestMemorySessionStore_TokenUniqueness(t *testing.T) {
	store := NewMemorySessionStore()
	ctx := context.Background()
	seen := make(map[string]bool)
	for i := 0; i < 100; i++ {
		token, _, err := store.Create(ctx, int64(i), time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		if seen[token] {
			t.Fatalf("дублирующийся токен на итерации %d", i)
		}
		seen[token] = true
	}
}

// TestSessionManager_Delegate — SessionManager делегирует в store.
func TestSessionManager_Delegate(t *testing.T) {
	store := NewMemorySessionStore()
	mgr := NewSessionManager(store)
	ctx := context.Background()

	token, _, err := mgr.Create(ctx, 777, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	uid, err := mgr.Validate(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	if uid != 777 {
		t.Fatalf("uid = %d, хотим 777", uid)
	}
	if err := mgr.Revoke(ctx, token); err != nil {
		t.Fatal(err)
	}
	_, err = mgr.Validate(ctx, token)
	if !errors.Is(err, ErrSessionRevoked) {
		t.Fatalf("после Revoke ожидали ErrSessionRevoked, получили %v", err)
	}
}
