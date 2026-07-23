package rebalance

import (
	"errors"
	"fmt"
	"time"
)

// ============================================================
// PlanState — состояние плана ребалансировки (раздел 12.5, 0003_transfers.sql).
// ============================================================

// PlanState — состояние плана ребалансировки.
// Значения должны точно совпадать с CHECK-ограничением таблицы transfer_plans.
type PlanState string

const (
	// StateDraft — план создан, ещё не прошёл проверки.
	StateDraft PlanState = "DRAFT"
	// StatePlanned — план рассчитан; в dry-run режиме — конечное состояние (этап 10).
	StatePlanned PlanState = "PLANNED"
	// StateAwaitingApproval — ожидается подтверждение оператора.
	StateAwaitingApproval PlanState = "AWAITING_APPROVAL"
	// StateTestSent — тестовый перевод отправлен.
	StateTestSent PlanState = "TEST_SENT"
	// StateTestConfirmed — тестовый перевод зачислен на destination (раздел 12.6, шаг 5-6).
	StateTestConfirmed PlanState = "TEST_CONFIRMED"
	// StateMainSent — основной перевод отправлен.
	StateMainSent PlanState = "MAIN_SENT"
	// StateMainConfirmed — основной перевод зачислен.
	StateMainConfirmed PlanState = "MAIN_CONFIRMED"
	// StateCompleted — ребалансировка завершена.
	StateCompleted PlanState = "COMPLETED"
	// StateFailed — план завершился ошибкой; повтор только по решению оператора.
	StateFailed PlanState = "FAILED"
	// StateCancelled — план отменён.
	StateCancelled PlanState = "CANCELLED"
)

// IsTerminal — true для финальных состояний (дальнейших переходов нет).
func (s PlanState) IsTerminal() bool {
	return s == StateCompleted || s == StateFailed || s == StateCancelled
}

// IsActive — true для активных (не финальных) состояний.
func (s PlanState) IsActive() bool {
	return !s.IsTerminal()
}

// ============================================================
// Таблица разрешённых переходов (раздел 12.5).
// ============================================================

// allowedPlanTransitions — явная таблица переходов.
// Ключ — текущее состояние, значение — множество допустимых следующих.
//
// Таблица:
//
//	DRAFT            → PLANNED, FAILED, CANCELLED
//	PLANNED          → AWAITING_APPROVAL, FAILED, CANCELLED
//	AWAITING_APPROVAL→ TEST_SENT, FAILED, CANCELLED
//	TEST_SENT        → TEST_CONFIRMED, FAILED, CANCELLED
//	TEST_CONFIRMED   → MAIN_SENT, FAILED, CANCELLED
//	MAIN_SENT        → MAIN_CONFIRMED, FAILED, CANCELLED
//	MAIN_CONFIRMED   → COMPLETED, FAILED
//	COMPLETED        → (нет)
//	FAILED           → (нет)
//	CANCELLED        → (нет)
var allowedPlanTransitions = map[PlanState]map[PlanState]bool{
	StateDraft: {
		StatePlanned:   true,
		StateFailed:    true,
		StateCancelled: true,
	},
	StatePlanned: {
		StateAwaitingApproval: true,
		StateFailed:           true,
		StateCancelled:        true,
	},
	StateAwaitingApproval: {
		StateTestSent:  true,
		StateFailed:    true,
		StateCancelled: true,
	},
	StateTestSent: {
		StateTestConfirmed: true,
		StateFailed:        true,
		StateCancelled:     true,
	},
	StateTestConfirmed: {
		StateMainSent:  true,
		StateFailed:    true,
		StateCancelled: true,
	},
	StateMainSent: {
		StateMainConfirmed: true,
		StateFailed:        true,
		StateCancelled:     true,
	},
	StateMainConfirmed: {
		StateCompleted: true,
		StateFailed:    true,
	},
	// Финальные: никаких переходов.
	StateCompleted: {},
	StateFailed:    {},
	StateCancelled: {},
}

// ============================================================
// Ошибки машины состояний.
// ============================================================

// ErrInvalidTransition — попытка недопустимого перехода состояний.
var ErrInvalidTransition = errors.New("rebalance: недопустимый переход состояния")

// ErrTerminalState — попытка перехода из финального состояния.
var ErrTerminalState = errors.New("rebalance: план находится в финальном состоянии")

// ErrDryRunStopped — в dry-run режиме переход за PLANNED запрещён.
var ErrDryRunStopped = errors.New("rebalance: dry-run режим: план остановлен на PLANNED")

// ============================================================
// Plan — экземпляр плана ребалансировки с машиной состояний.
// ============================================================

// Plan — план ребалансировки с встроенной машиной состояний.
// Не потокобезопасен — синхронизация на стороне вызывающего.
type Plan struct {
	// ID — уникальный идентификатор плана.
	ID string
	// State — текущее состояние.
	State PlanState
	// DryRun — если true, переходы останавливаются на StatePlanned (этап 10).
	DryRun bool
	// Proposal — предложение, от которого построен план.
	Proposal *PlanProposal
	// FailureReason — причина перехода в FAILED (заполняется при сбое).
	FailureReason string
	// CreatedAt — время создания плана.
	CreatedAt time.Time
	// UpdatedAt — время последнего обновления.
	UpdatedAt time.Time
	// CompletedAt — время завершения (не nil для COMPLETED).
	CompletedAt *time.Time

	// clock — инъекция времени для тестируемости.
	clock func() time.Time
}

// NewPlan создаёт новый план в состоянии DRAFT.
// clk — источник времени; nil означает использование time.Now.
func NewPlan(id string, proposal *PlanProposal, dryRun bool, clk func() time.Time) *Plan {
	if clk == nil {
		clk = time.Now
	}
	now := clk()
	return &Plan{
		ID:        id,
		State:     StateDraft,
		DryRun:    dryRun,
		Proposal:  proposal,
		CreatedAt: now,
		UpdatedAt: now,
		clock:     clk,
	}
}

// Transition выполняет переход в новое состояние.
//
// Гарды:
//   - Финальные состояния не допускают переходов (ErrTerminalState).
//   - Переход должен присутствовать в таблице (ErrInvalidTransition).
//   - В dry-run режиме переход за PLANNED запрещён (ErrDryRunStopped).
//
// reason заполняется только при переходе в FAILED; в остальных случаях игнорируется.
func (p *Plan) Transition(next PlanState, reason string) error {
	// Гард: финальное состояние.
	if p.State.IsTerminal() {
		return fmt.Errorf("%w: %s → %s", ErrTerminalState, p.State, next)
	}

	// Гард: dry-run — разрешаем переходы только до PLANNED включительно.
	if p.DryRun && p.State == StatePlanned && next != StateFailed && next != StateCancelled {
		return fmt.Errorf("%w: попытка перейти в %s", ErrDryRunStopped, next)
	}

	// Гард: проверка таблицы переходов.
	allowed, ok := allowedPlanTransitions[p.State]
	if !ok || !allowed[next] {
		return fmt.Errorf("%w: %s → %s", ErrInvalidTransition, p.State, next)
	}

	// Применяем переход.
	now := p.clock()
	p.State = next
	p.UpdatedAt = now

	if next == StateFailed {
		p.FailureReason = reason
	}
	if next == StateCompleted {
		p.CompletedAt = &now
	}

	return nil
}

// MustTransition выполняет переход и паникует при ошибке.
// Использовать только в тестах для краткости.
func (p *Plan) MustTransition(next PlanState, reason string) {
	if err := p.Transition(next, reason); err != nil {
		panic(err)
	}
}
