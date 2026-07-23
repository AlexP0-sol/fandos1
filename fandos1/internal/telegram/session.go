package telegram

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"
)

// ============================================================
// SessionStore — интерфейс хранилища сессий (раздел 13.4)
// ============================================================

// ErrSessionNotFound — sentinel: сессия не найдена или истекла.
var ErrSessionNotFound = errors.New("session: not found or expired")

// ErrSessionRevoked — sentinel: сессия отозвана.
var ErrSessionRevoked = errors.New("session: revoked")

// SessionStore — интерфейс хранилища сессий.
// Репозиторий-backed реализация подключается позже через эту же сигнатуру.
type SessionStore interface {
	// Create создаёт новую сессию для пользователя с указанным TTL.
	// Возвращает случайный токен (hex) и время истечения.
	Create(ctx context.Context, userID int64, ttl time.Duration) (token string, expiresAt time.Time, err error)

	// Validate проверяет токен и возвращает userID.
	// Возвращает ErrSessionNotFound если токен не найден или истёк.
	Validate(ctx context.Context, token string) (userID int64, err error)

	// Revoke отзывает сессию.
	// Если токен не существует — не возвращает ошибку.
	Revoke(ctx context.Context, token string) error
}

// ============================================================
// SessionManager — менеджер сессий
// ============================================================

// SessionManager оборачивает SessionStore и предоставляет бизнес-логику сессий.
type SessionManager struct {
	store SessionStore
}

// NewSessionManager создаёт SessionManager.
func NewSessionManager(store SessionStore) *SessionManager {
	return &SessionManager{store: store}
}

// Create делегирует в SessionStore.
func (m *SessionManager) Create(ctx context.Context, userID int64, ttl time.Duration) (string, time.Time, error) {
	return m.store.Create(ctx, userID, ttl)
}

// Validate делегирует в SessionStore.
func (m *SessionManager) Validate(ctx context.Context, token string) (int64, error) {
	return m.store.Validate(ctx, token)
}

// Revoke делегирует в SessionStore.
func (m *SessionManager) Revoke(ctx context.Context, token string) error {
	return m.store.Revoke(ctx, token)
}

// ============================================================
// MemorySessionStore — in-memory реализация SessionStore
// ============================================================

// memoryEntry — запись в памяти.
type memoryEntry struct {
	userID    int64
	expiresAt time.Time
	revoked   bool
}

// MemorySessionStore — потокобезопасная in-memory реализация.
// Используется в тестах и при отсутствии базы данных.
type MemorySessionStore struct {
	mu       sync.RWMutex
	sessions map[string]*memoryEntry
	nowFunc  func() time.Time // переопределяется в тестах
}

// NewMemorySessionStore создаёт MemorySessionStore.
func NewMemorySessionStore() *MemorySessionStore {
	return &MemorySessionStore{
		sessions: make(map[string]*memoryEntry),
		nowFunc:  time.Now,
	}
}

// Create создаёт новую сессию с криптографически случайным токеном (32 байта hex).
func (s *MemorySessionStore) Create(_ context.Context, userID int64, ttl time.Duration) (string, time.Time, error) {
	token, err := generateToken()
	if err != nil {
		return "", time.Time{}, fmt.Errorf("session: генерация токена: %w", err)
	}

	now := s.nowFunc()
	expiresAt := now.Add(ttl)

	s.mu.Lock()
	s.sessions[token] = &memoryEntry{
		userID:    userID,
		expiresAt: expiresAt,
	}
	s.mu.Unlock()

	return token, expiresAt, nil
}

// Validate проверяет токен и возвращает userID.
func (s *MemorySessionStore) Validate(_ context.Context, token string) (int64, error) {
	s.mu.RLock()
	entry, ok := s.sessions[token]
	s.mu.RUnlock()

	if !ok {
		return 0, ErrSessionNotFound
	}
	if entry.revoked {
		return 0, ErrSessionRevoked
	}
	if s.nowFunc().After(entry.expiresAt) {
		return 0, ErrSessionNotFound
	}
	return entry.userID, nil
}

// Revoke отзывает сессию.
func (s *MemorySessionStore) Revoke(_ context.Context, token string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if entry, ok := s.sessions[token]; ok {
		entry.revoked = true
	}
	return nil
}

// generateToken генерирует 32-байтовый криптографически случайный токен в hex.
func generateToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
