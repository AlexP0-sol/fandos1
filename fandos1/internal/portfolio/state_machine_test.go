package portfolio

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/thecd/fundarbitrage/internal/decimal"
	"github.com/thecd/fundarbitrage/internal/domain"
)

var testNow = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

// recordingPersister — сохраняет все переходы в слайс для проверки в тестах.
type recordingPersister struct {
	mu     sync.Mutex
	events []Transition
	failOn int // индекс перехода, на котором вернуть ошибку (-1 = никогда)
}

func (r *recordingPersister) OnTransition(_ *Position, t Transition) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.events) == r.failOn {
		return errors.New("forced persist failure")
	}
	r.events = append(r.events, t)
	return nil
}

func newPos() *Position {
	return NewPosition("pos-1", "BTC", domain.ExchangeBinance, domain.ExchangeBybit, testNow)
}

// TestHappyPathTransition — полный путь DISCOVERED → ... → CLOSED.
func TestHappyPathTransition(t *testing.T) {
	p := newPos()
	persister := &recordingPersister{failOn: -1}

	steps := []struct {
		to     State
		reason string
	}{
		{StateQualified, "passed filters"},
		{StateAwaitingApproval, "semi-auto"},
		{StatePreparing, "preflight ok"},
		{StateOpening, "sending orders"},
		{StatePartiallyHedged, "slice 1 partial"},
		{StateHedged, "fully hedged"},
		{StateMonitoring, "now monitoring"},
		{StateExitRequested, "TP hit"},
		{StateExiting, "coordinated close"},
		{StateReconciling, "verify zero positions"},
		{StateClosed, "done"},
	}
	for i, s := range steps {
		if err := p.TransitionTo(s.to, testNow.Add(time.Duration(i)*time.Minute), s.reason, "system", persister); err != nil {
			t.Fatalf("step %d → %s: %v", i, s.to, err)
		}
	}
	if p.CurrentState() != StateClosed {
		t.Errorf("final state = %s, want CLOSED", p.CurrentState())
	}
	if len(persister.events) != len(steps) {
		t.Errorf("persisted events = %d, want %d", len(persister.events), len(steps))
	}
}

// TestInvalidTransition — неразрешённый переход отклоняется.
func TestInvalidTransition(t *testing.T) {
	p := newPos()
	// DISCOVERED → MONITORING напрямую запрещено.
	err := p.TransitionTo(StateMonitoring, testNow, "skip", "system", nil)
	if !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("expected ErrInvalidTransition, got %v", err)
	}
	// Состояние не изменилось.
	if p.CurrentState() != StateDiscovered {
		t.Error("state changed despite invalid transition")
	}
}

// TestTerminalNoTransitions — из CLOSED нельзя перейти.
func TestTerminalNoTransitions(t *testing.T) {
	p := newPos()
	_ = p.TransitionTo(StateQualified, testNow, "", "", nil)
	_ = p.TransitionTo(StateFailed, testNow, "abort", "system", nil)
	if !p.IsTerminal() {
		t.Fatal("expected terminal after FAILED")
	}
	// Попытка перехода из FAILED.
	err := p.TransitionTo(StateMonitoring, testNow, "revive", "system", nil)
	if !errors.Is(err, ErrPositionTerminal) {
		t.Errorf("expected ErrPositionTerminal, got %v", err)
	}
}

// TestPersisterFailureRollsBack — ошибка персистенции откатывает состояние.
func TestPersisterFailureRollsBack(t *testing.T) {
	p := newPos()
	persister := &recordingPersister{failOn: 0} // первый переход падает
	err := p.TransitionTo(StateQualified, testNow, "", "", persister)
	if err == nil {
		t.Fatal("expected persist error")
	}
	// Состояние откатилось в DISCOVERED.
	if p.CurrentState() != StateDiscovered {
		t.Errorf("state = %s, want rollback to DISCOVERED", p.CurrentState())
	}
	// History пуста (последняя запись удалена).
	if len(p.HistoryCopy()) != 0 {
		t.Errorf("history len = %d, want 0 after rollback", len(p.HistoryCopy()))
	}
}

// TestDegradedRecovery — из DEGRADED можно вернуться к закрытию.
func TestDegradedRecovery(t *testing.T) {
	p := newPos()
	_ = p.TransitionTo(StateQualified, testNow, "", "", nil)
	_ = p.TransitionTo(StatePreparing, testNow, "", "", nil)
	_ = p.TransitionTo(StateOpening, testNow, "", "", nil)
	_ = p.TransitionTo(StateDegraded, testNow, "60/50 mismatch", "system", nil)
	if p.CurrentState() != StateDegraded {
		t.Fatal("expected DEGRADED")
	}
	// DEGRADED → EXIT_REQUESTED → EXITING → RECONCILING → CLOSED.
	steps := []State{StateExitRequested, StateExiting, StateReconciling, StateClosed}
	for _, s := range steps {
		if err := p.TransitionTo(s, testNow, "", "system", nil); err != nil {
			t.Fatalf("DEGRADED recovery → %s: %v", s, err)
		}
	}
}

// TestCanTransitionMatrix — проверка нескольких ключевых пар.
func TestCanTransitionMatrix(t *testing.T) {
	tests := []struct {
		from, to State
		want     bool
	}{
		{StateDiscovered, StateQualified, true},
		{StateDiscovered, StateClosed, false},
		{StateMonitoring, StateExitRequested, true},
		{StateMonitoring, StateOpening, false},
		{StateHedged, StateMonitoring, true},
		{StateClosed, StateMonitoring, false},
		{StateFailed, StateDiscovered, false},
	}
	for _, tt := range tests {
		got := CanTransition(tt.from, tt.to)
		if got != tt.want {
			t.Errorf("CanTransition(%s→%s) = %v, want %v", tt.from, tt.to, got, tt.want)
		}
	}
}

// TestSetQuantitiesAndDelta — обновление quantities и расчёт дельты (раздел 3.5).
func TestSetQuantitiesAndDelta(t *testing.T) {
	p := newPos()
	p.SetQuantities(decimal.MustFromString("50"), decimal.MustFromString("50"), testNow)
	d := p.DeltaBase()
	if !d.IsZero() {
		t.Errorf("delta = %s, want 0 (hedged)", d.String())
	}

	p.SetQuantities(decimal.MustFromString("60"), decimal.MustFromString("50"), testNow)
	d = p.DeltaBase()
	want := decimal.MustFromString("10")
	if !d.Equal(want) {
		t.Errorf("delta = %s, want %s (long heavy)", d.String(), want.String())
	}
}

// TestDeltaWithNegativeShort — abs(short) корректно считается.
func TestDeltaWithNegativeShort(t *testing.T) {
	p := newPos()
	// Если short qty хранится как отрицательное (некоторые биржи так делают).
	p.SetQuantities(decimal.MustFromString("50"), decimal.MustFromString("-50"), testNow)
	d := p.DeltaBase()
	if !d.IsZero() {
		t.Errorf("delta with negative short = %s, want 0", d.String())
	}
}

// TestPnLAccumulation
func TestPnLAccumulation(t *testing.T) {
	p := newPos()
	p.AddRealisedPnL(decimal.MustFromString("10"), testNow)
	p.AddRealisedPnL(decimal.MustFromString("5"), testNow)
	p.AddFundingPnL(decimal.MustFromString("3"), testNow)
	snap := p.Snapshot()
	if !snap.RealisedPnL.Equal(decimal.MustFromString("15")) {
		t.Errorf("realised = %s, want 15", snap.RealisedPnL.String())
	}
	if !snap.FundingPnL.Equal(decimal.MustFromString("3")) {
		t.Errorf("funding = %s, want 3", snap.FundingPnL.String())
	}
}

// TestSnapshotIsCopy — mutation снимка не влияет на позицию.
func TestSnapshotIsCopy(t *testing.T) {
	p := newPos()
	p.SetQuantities(decimal.MustFromString("50"), decimal.MustFromString("50"), testNow)
	s := p.Snapshot()
	s.LongBaseQty = decimal.FromInt(999)
	if p.DeltaBase().IsZero() {
		// Оригинал не изменился.
	} else {
		t.Error("snapshot mutation affected position")
	}
}

// TestHistoryCopyIsCopy
func TestHistoryCopyIsCopy(t *testing.T) {
	p := newPos()
	_ = p.TransitionTo(StateQualified, testNow, "q", "system", nil)
	h := p.HistoryCopy()
	h[0].Reason = "tampered"
	again := p.HistoryCopy()
	if again[0].Reason == "tampered" {
		t.Error("history copy mutation affected internal state")
	}
}

// TestConcurrentTransitions — гонка переходов не должна ломать позицию.
func TestConcurrentTransitions(t *testing.T) {
	p := newPos()
	var wg sync.WaitGroup
	// Все гонятся за TransitionTo(StateQualified) — должен пройти только один, остальные ErrInvalidTransition.
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = p.TransitionTo(StateQualified, testNow, "race", "system", nil)
		}()
	}
	wg.Wait()
	// Состояние — DISCOVERED (если никто не прошёл) или QUALIFIED (ровно один).
	st := p.CurrentState()
	if st != StateDiscovered && st != StateQualified {
		t.Errorf("concurrent race left state in %s", st)
	}
}
