package rebalance

import (
	"testing"
	"time"
)

// TestCircuitBreaker_NotTrippedByDefault проверяет начальное состояние.
func TestCircuitBreaker_NotTrippedByDefault(t *testing.T) {
	cb := NewCircuitBreaker(3, nil, fixedClock(testTime))
	tripped, reason := cb.IsTripped("exchange:binance")
	if tripped {
		t.Errorf("ожидали не сработавший breaker, получили tripped reason=%q", reason)
	}
}

// TestCircuitBreaker_TripsAfterThreshold проверяет срабатывание при достижении порога.
func TestCircuitBreaker_TripsAfterThreshold(t *testing.T) {
	threshold := 3
	cb := NewCircuitBreaker(threshold, nil, fixedClock(testTime))
	scope := "exchange:bybit"

	// Ниже порога — не срабатывает.
	for i := 0; i < threshold-1; i++ {
		cb.RecordFailure(scope)
		tripped, _ := cb.IsTripped(scope)
		if tripped {
			t.Errorf("breaker сработал раньше порога на итерации %d", i+1)
		}
	}

	// Ровно порог — срабатывает.
	cb.RecordFailure(scope)
	tripped, reason := cb.IsTripped(scope)
	if !tripped {
		t.Error("breaker должен был сработать при достижении порога")
	}
	if reason == "" {
		t.Error("reason должен быть заполнен при срабатывании")
	}
}

// TestCircuitBreaker_SuccessResetsCounter проверяет сброс счётчика после успеха.
func TestCircuitBreaker_SuccessResetsCounter(t *testing.T) {
	cb := NewCircuitBreaker(3, nil, fixedClock(testTime))
	scope := "exchange:okx"

	cb.RecordFailure(scope)
	cb.RecordFailure(scope)
	if cb.FailuresCount(scope) != 2 {
		t.Errorf("ожидали 2 ошибки, получили %d", cb.FailuresCount(scope))
	}

	cb.RecordSuccess(scope)
	if cb.FailuresCount(scope) != 0 {
		t.Errorf("ожидали 0 ошибок после успеха, получили %d", cb.FailuresCount(scope))
	}

	// Не должен сработать теперь при следующей одиночной ошибке.
	cb.RecordFailure(scope)
	tripped, _ := cb.IsTripped(scope)
	if tripped {
		t.Error("breaker не должен сработать после сброса счётчика")
	}
}

// TestCircuitBreaker_ResetManual проверяет ручной сброс.
func TestCircuitBreaker_ResetManual(t *testing.T) {
	cb := NewCircuitBreaker(2, nil, fixedClock(testTime))
	scope := "exchange:gate"

	cb.RecordFailure(scope)
	cb.RecordFailure(scope)

	tripped, _ := cb.IsTripped(scope)
	if !tripped {
		t.Fatal("breaker должен был сработать")
	}

	// Ручной сброс (оператор через Mini App, раздел 26.4).
	cb.Reset(scope)
	tripped, _ = cb.IsTripped(scope)
	if tripped {
		t.Error("после Reset breaker должен быть не сработан")
	}
	if cb.FailuresCount(scope) != 0 {
		t.Errorf("после Reset счётчик должен быть 0, получили %d", cb.FailuresCount(scope))
	}
}

// TestCircuitBreaker_SuccessDoesNotResetTripped проверяет, что успех не снимает trip.
func TestCircuitBreaker_SuccessDoesNotResetTripped(t *testing.T) {
	cb := NewCircuitBreaker(2, nil, fixedClock(testTime))
	scope := "exchange:mexc"

	cb.RecordFailure(scope)
	cb.RecordFailure(scope)

	// Breaker сработал.
	tripped, _ := cb.IsTripped(scope)
	if !tripped {
		t.Fatal("breaker должен сработать")
	}

	// Успех НЕ снимает trip (раздел 26.4 — только Manual Reset).
	cb.RecordSuccess(scope)
	tripped, _ = cb.IsTripped(scope)
	if !tripped {
		t.Error("успех не должен снимать trip circuit breaker")
	}
}

// TestCircuitBreaker_NoMoreFailuresAfterTrip проверяет, что после срабатывания
// счётчик не растёт (breaker уже сработан).
func TestCircuitBreaker_NoMoreFailuresAfterTrip(t *testing.T) {
	cb := NewCircuitBreaker(2, nil, fixedClock(testTime))
	scope := "exchange:kucoin"

	cb.RecordFailure(scope)
	cb.RecordFailure(scope)
	// Breaker сработал.

	countBefore := cb.FailuresCount(scope)
	cb.RecordFailure(scope)
	countAfter := cb.FailuresCount(scope)

	// Счётчик не меняется после срабатывания.
	if countAfter != countBefore {
		t.Errorf("счётчик изменился после срабатывания: %d → %d", countBefore, countAfter)
	}
}

// TestCircuitBreaker_IndependentScopes проверяет независимость разных scope.
func TestCircuitBreaker_IndependentScopes(t *testing.T) {
	cb := NewCircuitBreaker(2, nil, fixedClock(testTime))
	scopeA := "exchange:binance"
	scopeB := "exchange:bybit"

	cb.RecordFailure(scopeA)
	cb.RecordFailure(scopeA)

	trippedA, _ := cb.IsTripped(scopeA)
	trippedB, _ := cb.IsTripped(scopeB)

	if !trippedA {
		t.Error("scopeA должен сработать")
	}
	if trippedB {
		t.Error("scopeB должен быть независим от scopeA")
	}
}

// TestCircuitBreaker_State проверяет содержимое снимка состояния.
func TestCircuitBreaker_State(t *testing.T) {
	now := time.Date(2026, 7, 23, 10, 0, 0, 0, time.UTC)
	cb := NewCircuitBreaker(2, nil, fixedClock(now))
	scope := "route:42"

	cb.RecordFailure(scope)
	cb.RecordFailure(scope)

	s := cb.State(scope)
	if !s.Tripped {
		t.Error("State.Tripped должен быть true")
	}
	if s.FailuresCount != 2 {
		t.Errorf("State.FailuresCount: got %d, want 2", s.FailuresCount)
	}
	if s.TripReason == "" {
		t.Error("State.TripReason должен быть заполнен")
	}
	if !s.ManualResetRequired {
		t.Error("State.ManualResetRequired должен быть true")
	}
	if s.TrippedAt == nil {
		t.Error("State.TrippedAt должен быть заполнен")
	}
}
