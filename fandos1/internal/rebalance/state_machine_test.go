package rebalance

import (
	"testing"
	"time"
)

// fixedClock возвращает детерминированные часы для тестов.
func fixedClock(t time.Time) func() time.Time {
	return func() time.Time { return t }
}

var testTime = time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)

// makePlan — вспомогательная функция создания плана для тестов.
func makePlan(id string, dryRun bool) *Plan {
	return NewPlan(id, &PlanProposal{Asset: "USDT"}, dryRun, fixedClock(testTime))
}

// TestStateMachine_AllowedTransitions проверяет все разрешённые переходы.
func TestStateMachine_AllowedTransitions(t *testing.T) {
	// Таблица: (начальное состояние, переход, ожидаемый результат).
	tests := []struct {
		name      string
		fromState PlanState
		toState   PlanState
		wantErr   bool
	}{
		// DRAFT →
		{"DRAFT→PLANNED", StateDraft, StatePlanned, false},
		{"DRAFT→FAILED", StateDraft, StateFailed, false},
		{"DRAFT→CANCELLED", StateDraft, StateCancelled, false},
		// PLANNED →
		{"PLANNED→AWAITING_APPROVAL", StatePlanned, StateAwaitingApproval, false},
		{"PLANNED→FAILED", StatePlanned, StateFailed, false},
		{"PLANNED→CANCELLED", StatePlanned, StateCancelled, false},
		// AWAITING_APPROVAL →
		{"AWAITING_APPROVAL→TEST_SENT", StateAwaitingApproval, StateTestSent, false},
		{"AWAITING_APPROVAL→FAILED", StateAwaitingApproval, StateFailed, false},
		{"AWAITING_APPROVAL→CANCELLED", StateAwaitingApproval, StateCancelled, false},
		// TEST_SENT →
		{"TEST_SENT→TEST_CONFIRMED", StateTestSent, StateTestConfirmed, false},
		{"TEST_SENT→FAILED", StateTestSent, StateFailed, false},
		{"TEST_SENT→CANCELLED", StateTestSent, StateCancelled, false},
		// TEST_CONFIRMED →
		{"TEST_CONFIRMED→MAIN_SENT", StateTestConfirmed, StateMainSent, false},
		{"TEST_CONFIRMED→FAILED", StateTestConfirmed, StateFailed, false},
		{"TEST_CONFIRMED→CANCELLED", StateTestConfirmed, StateCancelled, false},
		// MAIN_SENT →
		{"MAIN_SENT→MAIN_CONFIRMED", StateMainSent, StateMainConfirmed, false},
		{"MAIN_SENT→FAILED", StateMainSent, StateFailed, false},
		{"MAIN_SENT→CANCELLED", StateMainSent, StateCancelled, false},
		// MAIN_CONFIRMED →
		{"MAIN_CONFIRMED→COMPLETED", StateMainConfirmed, StateCompleted, false},
		{"MAIN_CONFIRMED→FAILED", StateMainConfirmed, StateFailed, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan := makePlan("test-plan", false)
			// Принудительно устанавливаем начальное состояние.
			plan.State = tt.fromState

			err := plan.Transition(tt.toState, "test")
			if tt.wantErr && err == nil {
				t.Errorf("ожидали ошибку, получили nil")
			}
			if !tt.wantErr && err != nil {
				t.Errorf("неожиданная ошибка: %v", err)
			}
			if !tt.wantErr && plan.State != tt.toState {
				t.Errorf("State: got %s, want %s", plan.State, tt.toState)
			}
		})
	}
}

// TestStateMachine_ForbiddenTransitions проверяет запрещённые переходы.
func TestStateMachine_ForbiddenTransitions(t *testing.T) {
	forbidden := []struct {
		name      string
		fromState PlanState
		toState   PlanState
	}{
		// Запрещённые «короткие замыкания».
		{"DRAFT→TEST_SENT", StateDraft, StateTestSent},
		{"DRAFT→COMPLETED", StateDraft, StateCompleted},
		{"PLANNED→TEST_SENT", StatePlanned, StateTestSent},
		{"PLANNED→MAIN_SENT", StatePlanned, StateMainSent},
		{"AWAITING_APPROVAL→MAIN_SENT", StateAwaitingApproval, StateMainSent},
		{"AWAITING_APPROVAL→COMPLETED", StateAwaitingApproval, StateCompleted},
		// Ключевой запрет: MAIN без TEST_CONFIRMED.
		{"TEST_SENT→MAIN_SENT", StateTestSent, StateMainSent},
		{"AWAITING_APPROVAL→MAIN_SENT (MAIN before TEST)", StateAwaitingApproval, StateMainSent},
		// Из финальных состояний — нельзя.
		{"COMPLETED→PLANNED", StateCompleted, StatePlanned},
		{"FAILED→PLANNED", StateFailed, StatePlanned},
		{"CANCELLED→DRAFT", StateCancelled, StateDraft},
		// Обратные переходы.
		{"TEST_CONFIRMED→TEST_SENT", StateTestConfirmed, StateTestSent},
		{"MAIN_CONFIRMED→MAIN_SENT", StateMainConfirmed, StateMainSent},
	}

	for _, tt := range forbidden {
		t.Run(tt.name, func(t *testing.T) {
			plan := makePlan("test-plan", false)
			plan.State = tt.fromState

			err := plan.Transition(tt.toState, "")
			if err == nil {
				t.Errorf("ожидали ошибку для запрещённого перехода %s → %s", tt.fromState, tt.toState)
			}
		})
	}
}

// TestStateMachine_MainBeforeTestConfirmed — явная проверка ключевого запрета (раздел 12.6).
func TestStateMachine_MainBeforeTestConfirmed(t *testing.T) {
	states := []PlanState{
		StateDraft, StatePlanned, StateAwaitingApproval, StateTestSent,
	}
	for _, s := range states {
		t.Run(string(s)+"→MAIN_SENT", func(t *testing.T) {
			plan := makePlan("plan-test", false)
			plan.State = s
			err := plan.Transition(StateMainSent, "")
			if err == nil {
				t.Errorf("MAIN_SENT должен быть запрещён из состояния %s", s)
			}
		})
	}
}

// TestStateMachine_DryRunStopsAtPlanned проверяет dry-run барьер (этап 10).
func TestStateMachine_DryRunStopsAtPlanned(t *testing.T) {
	plan := makePlan("dry-plan", true /* dryRun */)

	// Переход DRAFT→PLANNED должен работать.
	if err := plan.Transition(StatePlanned, ""); err != nil {
		t.Fatalf("DRAFT→PLANNED: %v", err)
	}

	// Любой переход за PLANNED (кроме FAILED/CANCELLED) должен быть заблокирован.
	blockedNext := []PlanState{StateAwaitingApproval, StateTestSent, StateMainSent}
	for _, next := range blockedNext {
		plan2 := makePlan("dry-plan-2", true)
		plan2.State = StatePlanned
		err := plan2.Transition(next, "")
		if err == nil {
			t.Errorf("dry-run: переход PLANNED→%s должен быть запрещён", next)
		}
	}

	// FAILED и CANCELLED из PLANNED разрешены даже в dry-run.
	for _, terminal := range []PlanState{StateFailed, StateCancelled} {
		plan3 := makePlan("dry-plan-3", true)
		plan3.State = StatePlanned
		if err := plan3.Transition(terminal, "dry-run cancel"); err != nil {
			t.Errorf("dry-run: переход PLANNED→%s должен быть разрешён: %v", terminal, err)
		}
	}
}

// TestStateMachine_TerminalStateNilTransition — из финального состояния нельзя никуда.
func TestStateMachine_TerminalStateNilTransition(t *testing.T) {
	for _, terminal := range []PlanState{StateCompleted, StateFailed, StateCancelled} {
		t.Run(string(terminal), func(t *testing.T) {
			plan := makePlan("plan", false)
			plan.State = terminal
			err := plan.Transition(StateDraft, "")
			if err == nil {
				t.Errorf("ожидали ошибку из финального состояния %s", terminal)
			}
		})
	}
}

// TestStateMachine_FailureReasonPersisted — FailureReason сохраняется при FAILED.
func TestStateMachine_FailureReasonPersisted(t *testing.T) {
	plan := makePlan("plan-fail", false)
	plan.State = StateTestSent
	reason := "тестовый перевод не зачислен"
	if err := plan.Transition(StateFailed, reason); err != nil {
		t.Fatalf("Transition: %v", err)
	}
	if plan.FailureReason != reason {
		t.Errorf("FailureReason: got %q, want %q", plan.FailureReason, reason)
	}
}

// TestStateMachine_CompletedTimestamp — CompletedAt заполняется при COMPLETED.
func TestStateMachine_CompletedTimestamp(t *testing.T) {
	plan := makePlan("plan-complete", false)
	plan.State = StateMainConfirmed
	if err := plan.Transition(StateCompleted, ""); err != nil {
		t.Fatalf("Transition: %v", err)
	}
	if plan.CompletedAt == nil {
		t.Error("CompletedAt должен быть заполнен")
	}
}

// TestStateMachine_UpdatedAt — UpdatedAt меняется после каждого перехода.
func TestStateMachine_UpdatedAt(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	t1 := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)

	calls := 0
	clk := func() time.Time {
		calls++
		if calls <= 1 {
			return t0
		}
		return t1
	}
	plan := NewPlan("plan-time", &PlanProposal{Asset: "USDT"}, false, clk)
	// CreatedAt = t0
	if !plan.CreatedAt.Equal(t0) {
		t.Errorf("CreatedAt: got %v, want %v", plan.CreatedAt, t0)
	}

	_ = plan.Transition(StatePlanned, "")
	// UpdatedAt должен быть t1 (второй вызов clock).
	if !plan.UpdatedAt.Equal(t1) {
		t.Errorf("UpdatedAt: got %v, want %v", plan.UpdatedAt, t1)
	}
}

// TestStateMachine_HappyPath — полный путь через все состояния.
func TestStateMachine_HappyPath(t *testing.T) {
	path := []PlanState{
		StatePlanned,
		StateAwaitingApproval,
		StateTestSent,
		StateTestConfirmed,
		StateMainSent,
		StateMainConfirmed,
		StateCompleted,
	}
	plan := makePlan("happy-plan", false)
	for _, next := range path {
		if err := plan.Transition(next, ""); err != nil {
			t.Fatalf("happy path: %s → %s: %v", plan.State, next, err)
		}
	}
	if plan.State != StateCompleted {
		t.Errorf("итоговое состояние: got %s, want %s", plan.State, StateCompleted)
	}
}

// TestStateMachine_TransitionTableComplete — убеждаемся, что в таблице есть все состояния.
func TestStateMachine_TransitionTableComplete(t *testing.T) {
	allStates := []PlanState{
		StateDraft, StatePlanned, StateAwaitingApproval, StateTestSent, StateTestConfirmed,
		StateMainSent, StateMainConfirmed, StateCompleted, StateFailed, StateCancelled,
	}
	for _, s := range allStates {
		if _, ok := allowedPlanTransitions[s]; !ok {
			t.Errorf("состояние %s отсутствует в таблице переходов", s)
		}
	}
}
