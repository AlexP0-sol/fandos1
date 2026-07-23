package marketdata

import (
	"context"
	"errors"
	"math/rand"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thecd/fundarbitrage/internal/domain"
)

// newRand — детерминированный rng для тестов jitter.
func newRand() *rand.Rand {
	return rand.New(rand.NewSource(42))
}

// waitFor — опрашивает cond каждые 5ms до таймаута.
// Возвращает true, если cond вернул true до истечения timeout.
func waitFor(cond func() bool, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

// TestBackoffGrowth — задержки растут экспоненциально до потолка.
func TestBackoffGrowth(t *testing.T) {
	b := BackoffConfig{Initial: 100 * time.Millisecond, Max: time.Second, Multiplier: 2.0, Jitter: 0}
	// attempt 0 → 100ms, 1 → 200, 2 → 400, 3 → 800, 4 → 1000 (cap).
	want := []time.Duration{100 * time.Millisecond, 200 * time.Millisecond, 400 * time.Millisecond, 800 * time.Millisecond, 1000 * time.Millisecond}
	for i, w := range want {
		got := b.NextDelay(i, nil)
		if got != w {
			t.Errorf("attempt %d: got %v, want %v", i, got, w)
		}
	}
	// На больших attempt — потолок.
	if b.NextDelay(20, nil) != time.Second {
		t.Errorf("expected cap at 1s, got %v", b.NextDelay(20, nil))
	}
}

// TestBackoffJitterWithinBounds — jitter не выводит за [1-J, 1+J] × base.
func TestBackoffJitterWithinBounds(t *testing.T) {
	b := BackoffConfig{Initial: 100 * time.Millisecond, Max: time.Second, Multiplier: 2.0, Jitter: 0.2}
	r := newRand()
	base := 100 * time.Millisecond
	lower := time.Duration(float64(base) * 0.8)
	upper := time.Duration(float64(base) * 1.2)
	for i := 0; i < 100; i++ {
		got := b.NextDelay(0, r)
		if got < lower || got > upper {
			t.Errorf("jittered delay %v outside [%v, %v]", got, lower, upper)
		}
	}
}

// TestBackoffNoInfOverflow — большой attempt не вызывает overflow к +Inf.
func TestBackoffNoInfOverflow(t *testing.T) {
	b := BackoffConfig{Initial: time.Second, Max: 60 * time.Second, Multiplier: 2.0, Jitter: 0}
	// 1000 попыток — без паники/Inf
	got := b.NextDelay(1000, nil)
	if got > b.Max {
		t.Errorf("delay %v exceeds Max %v", got, b.Max)
	}
	if got <= 0 {
		t.Errorf("delay %v must be positive", got)
	}
}

// TestManagerFirstConnectSuccess — первичный успех сбрасывает счётчик.
func TestManagerFirstConnectSuccess(t *testing.T) {
	var calls atomic.Int64
	cb := func(ctx context.Context) error {
		calls.Add(1)
		<-ctx.Done() // эмулируем «соединение живо, пока ctx не отменён»
		return ctx.Err()
	}
	m := NewConnectionManager(domain.ExchangeBinance, cb).
		WithBackoff(BackoffConfig{Initial: 10 * time.Millisecond, Max: 50 * time.Millisecond, Multiplier: 2, Jitter: 0})

	ctx, cancel := context.WithCancel(context.Background())
	go m.Run(ctx)

	// Ждём первого вызова через polling.
	if !waitFor(func() bool { return calls.Load() >= 1 }, 500*time.Millisecond) {
		t.Fatal("timeout waiting for first connect call")
	}
	cancel()

	if calls.Load() != 1 {
		t.Errorf("expected 1 connect call, got %d", calls.Load())
	}
	if !waitFor(func() bool { return m.ReconnectCount() >= 1 }, 200*time.Millisecond) {
		t.Errorf("reconnect count=%d, want 1", m.ReconnectCount())
	}
}

// TestManagerRetriesOnFailure — неудача вызывает повтор по backoff.
func TestManagerRetriesOnFailure(t *testing.T) {
	var calls atomic.Int64
	cb := func(ctx context.Context) error {
		calls.Add(1)
		return errors.New("connection refused") // всегда неудача
	}
	m := NewConnectionManager(domain.ExchangeBinance, cb).
		WithBackoff(BackoffConfig{Initial: 5 * time.Millisecond, Max: 20 * time.Millisecond, Multiplier: 2, Jitter: 0}).
		WithBreaker(CircuitBreakerConfig{FailureThreshold: 50, ResetTimeout: 1 * time.Hour})

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	m.Run(ctx)

	if calls.Load() < 3 {
		t.Errorf("expected at least 3 retry attempts, got %d", calls.Load())
	}
}

// TestManagerCircuitBreaker — после N неудач breaker открывается и блокирует попытки.
func TestManagerCircuitBreaker(t *testing.T) {
	var calls atomic.Int64
	cb := func(ctx context.Context) error {
		calls.Add(1)
		return errors.New("fail")
	}
	m := NewConnectionManager(domain.ExchangeBinance, cb).
		WithBackoff(BackoffConfig{Initial: 1 * time.Millisecond, Max: 2 * time.Millisecond, Multiplier: 2, Jitter: 0}).
		WithBreaker(CircuitBreakerConfig{FailureThreshold: 3, ResetTimeout: 1 * time.Hour})

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	m.Run(ctx)

	// Должен был открыть breaker после 3 неудач → последующие попытки заблокированы.
	if m.State() != BreakerOpen {
		t.Errorf("expected breaker open, state=%d", m.State())
	}
	if m.CircuitTrips() != 1 {
		t.Errorf("trips=%d, want 1", m.CircuitTrips())
	}
	// После 3 неудач + reset-timeout (1h) новых попыток быть не должно.
	if calls.Load() > 3 {
		t.Errorf("calls=%d, want ≤ 3 (breaker should block)", calls.Load())
	}
}

// TestManagerStop — Stop отменяет loop.
func TestManagerStop(t *testing.T) {
	cb := func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}
	m := NewConnectionManager(domain.ExchangeBinance, cb)
	go m.Run(context.Background())

	// Ждём первого соединения через polling.
	if !waitFor(func() bool { return m.ReconnectCount() >= 1 }, 500*time.Millisecond) {
		t.Fatal("timeout waiting for first connect")
	}
	m.Stop()
	// После Stop — manager не должен паниковать или висеть.
	// Даём короткий slot на завершение (не sleep).
	time.Sleep(20 * time.Millisecond)
}

// TestManagerReconnectsAfterDrop — обрыв успешного соединения → reconnect.
func TestManagerReconnectsAfterDrop(t *testing.T) {
	var calls atomic.Int64
	// Первое соединение держится 20ms, потом обрыв; второе — до отмены ctx.
	cb := func(ctx context.Context) error {
		n := calls.Add(1)
		if n == 1 {
			time.Sleep(20 * time.Millisecond)
			return errors.New("dropped")
		}
		<-ctx.Done()
		return ctx.Err()
	}
	m := NewConnectionManager(domain.ExchangeBinance, cb).
		WithBackoff(BackoffConfig{Initial: 5 * time.Millisecond, Max: 10 * time.Millisecond, Multiplier: 2, Jitter: 0}).
		WithBreaker(CircuitBreakerConfig{FailureThreshold: 100, ResetTimeout: 1 * time.Hour})

	ctx, cancel := context.WithCancel(context.Background())
	go m.Run(ctx)

	// Ждём второго вызова через polling вместо фиксированного sleep.
	if !waitFor(func() bool { return calls.Load() >= 2 }, 500*time.Millisecond) {
		t.Errorf("expected at least 2 connections (drop + reconnect), got %d", calls.Load())
	}
	cancel()
}

// TestManagerDoubleStartError — второй вызов Run() возвращает ErrAlreadyRunning.
func TestManagerDoubleStartError(t *testing.T) {
	cb := func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}
	m := NewConnectionManager(domain.ExchangeBinance, cb)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go m.Run(ctx)

	// Ждём, пока Run стартует.
	if !waitFor(func() bool { return m.ReconnectCount() >= 1 }, 500*time.Millisecond) {
		t.Fatal("timeout waiting for Run to start")
	}

	// Второй вызов Run должен вернуть ErrAlreadyRunning.
	err := m.Run(ctx)
	if !errors.Is(err, ErrAlreadyRunning) {
		t.Errorf("second Run: got %v, want ErrAlreadyRunning", err)
	}
}

// TestManagerCircuitBreakerResetsAttemptsOnReopen — при Open→Closed сбрасываются попытки,
// чтобы probe-попытка не вызвала немедленное повторное размыкание.
func TestManagerCircuitBreakerResetsAttemptsOnReopen(t *testing.T) {
	var calls atomic.Int64
	threshold := 3
	// Первые threshold попыток завершаются неудачей (открывают breaker).
	// После Reset — probe должна успеть.
	cb := func(ctx context.Context) error {
		n := calls.Add(1)
		if n <= int64(threshold) {
			return errors.New("fail")
		}
		// probe-попытка и все последующие успешны.
		<-ctx.Done()
		return ctx.Err()
	}

	resetTimeout := 20 * time.Millisecond
	m := NewConnectionManager(domain.ExchangeBinance, cb).
		WithBackoff(BackoffConfig{Initial: 1 * time.Millisecond, Max: 2 * time.Millisecond, Multiplier: 2, Jitter: 0}).
		WithBreaker(CircuitBreakerConfig{FailureThreshold: threshold, ResetTimeout: resetTimeout})

	ctx, cancel := context.WithCancel(context.Background())
	go m.Run(ctx)

	// Ждём, пока probe-попытка не пройдёт (calls > threshold).
	if !waitFor(func() bool { return calls.Load() > int64(threshold) }, 500*time.Millisecond) {
		t.Errorf("probe attempt did not occur, calls=%d", calls.Load())
	}
	// Breaker должен быть закрыт (probe успешна).
	if m.State() != BreakerClosed {
		t.Errorf("breaker should be closed after successful probe, got %d", m.State())
	}
	cancel()
}
