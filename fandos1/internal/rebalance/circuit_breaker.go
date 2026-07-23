package rebalance

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// ============================================================
// BreakerStore — интерфейс персистентного хранилища circuit breaker.
// Реализуется internal/repository; в памяти — встроенный in-memory core.
// ============================================================

// BreakerStore — персистентное хранилище состояния circuit breaker (раздел 26.4).
// Позволяет сохранять/восстанавливать состояние между перезапусками.
type BreakerStore interface {
	// SaveBreakerState сохраняет текущее состояние breaker для scope.
	SaveBreakerState(ctx context.Context, state BreakerState) error
	// LoadBreakerState загружает состояние для scope; nil если не найдено.
	LoadBreakerState(ctx context.Context, scope string) (*BreakerState, error)
}

// ============================================================
// BreakerState — снимок состояния circuit breaker.
// ============================================================

// BreakerState — состояние breaker для одного scope (exchange или маршрута).
// Соответствует таблице withdrawal_circuit_breaker.
type BreakerState struct {
	// Scope — ключ: "exchange:<id>" | "route:<route_id>" | "global".
	Scope   string
	Tripped bool
	// TripReason — причина срабатывания.
	TripReason string
	// FailuresCount — количество последовательных неудач.
	FailuresCount int
	// TrippedAt — время срабатывания.
	TrippedAt *time.Time
	// ManualResetRequired — разблокировка только вручную (через Mini App, раздел 26.4).
	ManualResetRequired bool
	// UpdatedAt — время последнего обновления.
	UpdatedAt time.Time
}

// ============================================================
// CircuitBreaker — in-memory ядро (раздел 26.4).
// ============================================================

// CircuitBreaker — защитник вывода: счётчик последовательных ошибок по scope.
//
// Срабатывание:
//   - После FailureThreshold последовательных неудач по scope → tripped.
//   - Executor проверяет IsTripped перед КАЖДЫМ переводом.
//   - Сброс — только через Reset (вручную, раздел 26.4).
//
// Потокобезопасен.
type CircuitBreaker struct {
	mu               sync.Mutex
	threshold        int
	states           map[string]*scopeState
	persistenceStore BreakerStore // может быть nil (только in-memory)
	clock            func() time.Time
}

// scopeState — состояние breaker для одного scope.
type scopeState struct {
	failures   int
	tripped    bool
	tripReason string
	trippedAt  *time.Time
}

// NewCircuitBreaker создаёт CircuitBreaker.
//
// threshold — количество последовательных неудач до срабатывания (раздел 26.4).
// store — персистентное хранилище (может быть nil для pure in-memory режима).
// clk — инъекция времени; nil = time.Now.
func NewCircuitBreaker(threshold int, store BreakerStore, clk func() time.Time) *CircuitBreaker {
	if clk == nil {
		clk = time.Now
	}
	return &CircuitBreaker{
		threshold:        threshold,
		states:           make(map[string]*scopeState),
		persistenceStore: store,
		clock:            clk,
	}
}

// IsTripped возвращает (true, reason) если breaker для scope сработал.
// Консультируется только с in-memory состоянием (горячий путь).
func (cb *CircuitBreaker) IsTripped(scope string) (bool, string) {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	s, ok := cb.states[scope]
	if !ok {
		return false, ""
	}
	if s.tripped {
		return true, s.tripReason
	}
	return false, ""
}

// RecordFailure регистрирует неудачу для scope.
// При достижении порога — переключает breaker в tripped (раздел 26.4).
func (cb *CircuitBreaker) RecordFailure(scope string) {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	s := cb.ensureScope(scope)
	if s.tripped {
		// Уже сработал — ничего не меняем.
		return
	}
	s.failures++

	if s.failures >= cb.threshold {
		now := cb.clock()
		s.tripped = true
		s.tripReason = fmt.Sprintf(
			"достигнут порог %d последовательных ошибок для scope %q", cb.threshold, scope,
		)
		s.trippedAt = &now
	}
}

// RecordSuccess сбрасывает счётчик неудач для scope (успешный перевод).
// НЕ снимает trip — для снятия требуется явный вызов Reset (раздел 26.4).
func (cb *CircuitBreaker) RecordSuccess(scope string) {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	s := cb.ensureScope(scope)
	if !s.tripped {
		s.failures = 0
	}
}

// Reset снимает trip для scope (ручное вмешательство оператора, раздел 26.4).
// В боевом режиме вызывается только из Mini App после 2FA подтверждения.
func (cb *CircuitBreaker) Reset(scope string) {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	s := cb.ensureScope(scope)
	s.tripped = false
	s.failures = 0
	s.tripReason = ""
	s.trippedAt = nil
}

// State возвращает снимок состояния для scope (для UI/аудита).
func (cb *CircuitBreaker) State(scope string) BreakerState {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	s, ok := cb.states[scope]
	if !ok {
		return BreakerState{
			Scope:               scope,
			ManualResetRequired: true,
			UpdatedAt:           cb.clock(),
		}
	}
	return BreakerState{
		Scope:               scope,
		Tripped:             s.tripped,
		TripReason:          s.tripReason,
		FailuresCount:       s.failures,
		TrippedAt:           s.trippedAt,
		ManualResetRequired: true,
		UpdatedAt:           cb.clock(),
	}
}

// FailuresCount возвращает текущий счётчик последовательных ошибок для scope.
func (cb *CircuitBreaker) FailuresCount(scope string) int {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	s, ok := cb.states[scope]
	if !ok {
		return 0
	}
	return s.failures
}

// ensureScope возвращает существующее или новое состояние для scope.
// Вызывается под cb.mu.
func (cb *CircuitBreaker) ensureScope(scope string) *scopeState {
	s, ok := cb.states[scope]
	if !ok {
		s = &scopeState{}
		cb.states[scope] = s
	}
	return s
}
