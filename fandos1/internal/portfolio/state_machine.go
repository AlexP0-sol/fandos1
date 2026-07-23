// Package portfolio реализует persistent position state machine (раздел 10 промпта v2).
//
// Каждая позиция имеет state machine, переходы которого явно описаны и протестированы.
// Все переходы персистируются в БД и логируются в audit (через callback, реализуемый repository).
// Управление состоянием ТОЛЬКО в памяти недопустимо (раздел 10).
//
// Состояния (раздел 10):
//
//	DISCOVERED → QUALIFIED → AWAITING_USER_APPROVAL → PREPARING → OPENING
//	  → PARTIALLY_HEDGED → HEDGED → MONITORING → EXIT_REQUESTED → EXITING
//	  → RECONCILING → CLOSED
//	  → DEGRADED / FAILED
package portfolio

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/thecd/fundarbitrage/internal/decimal"
	"github.com/thecd/fundarbitrage/internal/domain"
)

// State — состояние позиции (раздел 10).
type State string

const (
	StateDiscovered       State = "DISCOVERED"
	StateQualified        State = "QUALIFIED"
	StateAwaitingApproval State = "AWAITING_USER_APPROVAL"
	StatePreparing        State = "PREPARING"
	StateOpening          State = "OPENING"
	StatePartiallyHedged  State = "PARTIALLY_HEDGED"
	StateHedged           State = "HEDGED"
	StateMonitoring       State = "MONITORING"
	StateExitRequested    State = "EXIT_REQUESTED"
	StateExiting          State = "EXITING"
	StateReconciling      State = "RECONCILING"
	StateClosed           State = "CLOSED"
	StateDegraded         State = "DEGRADED"
	StateFailed           State = "FAILED"
)

// IsTerminal — true для финальных состояний (позиция неактивна).
func (s State) IsTerminal() bool {
	return s == StateClosed || s == StateFailed
}

// IsOpenLike — true для состояний, где позиция удерживает экспозицию (дельта должна соблюдаться).
func (s State) IsOpenLike() bool {
	switch s {
	case StatePartiallyHedged, StateHedged, StateMonitoring, StateExitRequested, StateExiting, StateDegraded:
		return true
	}
	return false
}

// allowedTransitions — явная таблица разрешённых переходов (раздел 10).
// Ключ — текущее состояние, значение — множество допустимых следующих.
var allowedTransitions = map[State]map[State]bool{
	StateDiscovered: {
		StateQualified: true, StateFailed: true,
	},
	StateQualified: {
		StateAwaitingApproval: true, StatePreparing: true, StateFailed: true,
	},
	StateAwaitingApproval: {
		StatePreparing: true, StateDiscovered: true, StateFailed: true, // re-qualify или отмена
	},
	StatePreparing: {
		StateOpening: true, StateFailed: true, StateDiscovered: true, // preflight fail → back
	},
	StateOpening: {
		StatePartiallyHedged: true, StateHedged: true, StateDegraded: true, StateFailed: true,
	},
	StatePartiallyHedged: {
		StateHedged: true, StateDegraded: true, StateExitRequested: true, StateFailed: true,
	},
	StateHedged: {
		StateMonitoring: true, StateExitRequested: true,
	},
	StateMonitoring: {
		StateExitRequested: true, StateDegraded: true, StateReconciling: true,
	},
	StateExitRequested: {
		StateExiting: true, StateDegraded: true,
	},
	StateExiting: {
		StateReconciling: true, StateDegraded: true, StateFailed: true,
	},
	StateReconciling: {
		StateClosed: true, StateDegraded: true, StateFailed: true,
	},
	StateDegraded: {
		StateExitRequested: true, StateReconciling: true, StateFailed: true, StateClosed: true,
	},
	// Terminal: никаких переходов.
	StateClosed: {},
	StateFailed: {},
}

// CanTransition — true, если переход from→to разрешён таблицей.
func CanTransition(from, to State) bool {
	if targets, ok := allowedTransitions[from]; ok {
		return targets[to]
	}
	return false
}

// ErrInvalidTransition — попытка неразрешённого перехода.
var ErrInvalidTransition = errors.New("portfolio: invalid state transition")

// ErrPositionTerminal — позиция уже в финальном состоянии.
var ErrPositionTerminal = errors.New("portfolio: position is in terminal state")

// ErrPersistInProgress — персистенция перехода уже идёт; вызывающий должен повторить позже.
var ErrPersistInProgress = errors.New("portfolio: persist in progress")

// ErrPositionTerminalMutation — попытка изменить терминальную позицию.
var ErrPositionTerminalMutation = errors.New("portfolio: cannot mutate terminal position")

// ============================================================
// Position (доменный агрегат)
// ============================================================

// Position — парная позиция (long leg + short leg) с persistent state machine.
type Position struct {
	mu sync.Mutex

	ID            domain.PositionID
	Asset         domain.AssetSymbol
	LongExchange  domain.ExchangeID
	ShortExchange domain.ExchangeID

	State       State
	EntryReason string

	// Target / actual quantities.
	TargetBaseQty decimal.Decimal
	LongBaseQty   decimal.Decimal
	ShortBaseQty  decimal.Decimal // abs

	// PnL accumulators.
	RealisedPnL decimal.Decimal
	FundingPnL  decimal.Decimal
	FeesPaid    decimal.Decimal

	// Timestamps.
	CreatedAt  time.Time
	UpdatedAt  time.Time
	ExitReason string
	ExitedAt   *time.Time

	// History of transitions (in-memory audit; персистируется в БД через Persister callback).
	History []Transition

	// persisting — защита от TOCTOU: если true, персистенция уже в процессе,
	// новые переходы блокируются через ErrPersistInProgress.
	persisting bool
}

// Transition — запись одного перехода.
type Transition struct {
	From   State
	To     State
	At     time.Time
	Reason string
	Actor  string // "system:scanner" / "user" / "system:risk"
}

// Persister — callback для персистентной записи перехода в БД + audit log.
// Реализуется repository (Этап 2 migrations, таблица positions + audit_log).
// Если Persister возвращает ошибку, переход откатывается в памяти.
type Persister interface {
	OnTransition(pos *Position, t Transition) error
}

// NewPosition создаёт позицию в начальном состоянии DISCOVERED.
func NewPosition(id domain.PositionID, asset domain.AssetSymbol, longEx, shortEx domain.ExchangeID, now time.Time) *Position {
	return &Position{
		ID:            id,
		Asset:         asset,
		LongExchange:  longEx,
		ShortExchange: shortEx,
		State:         StateDiscovered,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
}

// State поля только для чтения через методы (потокобезопасно).

// CurrentState возвращает текущее состояние.
func (p *Position) CurrentState() State {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.State
}

// IsTerminal — true, если позиция закрыта/провалена.
func (p *Position) IsTerminal() bool {
	return p.CurrentState().IsTerminal()
}

// TransitionTo выполняет переход в новое состояние.
// Возвращает ErrInvalidTransition, если переход неразрешён; ErrPositionTerminal, если уже финально.
// ErrPersistInProgress, если параллельный вызов уже выполняет персистенцию (TOCTOU-защита).
// now передаётся для тестируемости. reason/actor — для audit.
// Если persister не nil и возвращает ошибку — переход откатывается, ошибка возвращается.
func (p *Position) TransitionTo(to State, now time.Time, reason, actor string, persister Persister) error {
	p.mu.Lock()
	from := p.State
	if from.IsTerminal() {
		p.mu.Unlock()
		return ErrPositionTerminal
	}
	// TOCTOU: если персистенция уже в процессе — отказываем.
	if p.persisting {
		p.mu.Unlock()
		return ErrPersistInProgress
	}
	if !CanTransition(from, to) {
		p.mu.Unlock()
		return fmt.Errorf("%w: %s → %s", ErrInvalidTransition, from, to)
	}
	// Фиксируем переход и блокируем параллельные вызовы на время персистенции.
	p.persisting = true
	p.State = to
	p.UpdatedAt = now
	t := Transition{From: from, To: to, At: now, Reason: reason, Actor: actor}
	p.History = append(p.History, t)
	p.mu.Unlock()

	// Persister вызывается ВНЕ блокировки, чтобы избежать deadlock при callback → read state.
	if persister != nil {
		if err := persister.OnTransition(p, t); err != nil {
			// Откат.
			p.mu.Lock()
			p.State = from
			p.UpdatedAt = now
			// Удаляем последнюю запись history.
			if len(p.History) > 0 {
				p.History = p.History[:len(p.History)-1]
			}
			p.persisting = false
			p.mu.Unlock()
			return fmt.Errorf("portfolio: persist transition failed: %w", err)
		}
	}

	p.mu.Lock()
	p.persisting = false
	p.mu.Unlock()
	return nil
}

// SetQuantities — обновляет фактические quantities после исполнения (например, после slice).
// Возвращает ErrPositionTerminalMutation при попытке изменить терминальную позицию.
func (p *Position) SetQuantities(long, short decimal.Decimal, now time.Time) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.State.IsTerminal() {
		return ErrPositionTerminalMutation
	}
	p.LongBaseQty = long
	p.ShortBaseQty = short
	p.UpdatedAt = now
	return nil
}

// DeltaBase — дельта по базовому активу (раздел 3.5):
//
//	DeltaBase = LongBaseQty - abs(ShortBaseQty)
func (p *Position) DeltaBase() decimal.Decimal {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.LongBaseQty.Sub(p.ShortBaseQty.Abs())
}

// AddRealisedPnL — добавляет реализованный PnL (после закрытия/частичного закрытия).
// Возвращает ErrPositionTerminalMutation при попытке изменить терминальную позицию.
func (p *Position) AddRealisedPnL(amount decimal.Decimal, now time.Time) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.State.IsTerminal() {
		return ErrPositionTerminalMutation
	}
	p.RealisedPnL = p.RealisedPnL.Add(amount)
	p.UpdatedAt = now
	return nil
}

// AddFundingPnL — добавляет подтверждённый funding PnL (раздел 3.2).
// Возвращает ErrPositionTerminalMutation при попытке изменить терминальную позицию.
func (p *Position) AddFundingPnL(amount decimal.Decimal, now time.Time) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.State.IsTerminal() {
		return ErrPositionTerminalMutation
	}
	p.FundingPnL = p.FundingPnL.Add(amount)
	p.UpdatedAt = now
	return nil
}

// Snapshot возвращает безопасную копию позиции для UI/логов (без mutex).
type Snapshot struct {
	ID            domain.PositionID
	Asset         domain.AssetSymbol
	LongExchange  domain.ExchangeID
	ShortExchange domain.ExchangeID
	State         State
	TargetBaseQty decimal.Decimal
	LongBaseQty   decimal.Decimal
	ShortBaseQty  decimal.Decimal
	RealisedPnL   decimal.Decimal
	FundingPnL    decimal.Decimal
	FeesPaid      decimal.Decimal
	DeltaBase     decimal.Decimal
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// Snapshot — потокобезопасный снимок позиции.
func (p *Position) Snapshot() Snapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	return Snapshot{
		ID: p.ID, Asset: p.Asset,
		LongExchange: p.LongExchange, ShortExchange: p.ShortExchange,
		State:         p.State,
		TargetBaseQty: p.TargetBaseQty,
		LongBaseQty:   p.LongBaseQty, ShortBaseQty: p.ShortBaseQty,
		RealisedPnL: p.RealisedPnL, FundingPnL: p.FundingPnL, FeesPaid: p.FeesPaid,
		DeltaBase: p.LongBaseQty.Sub(p.ShortBaseQty.Abs()),
		CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt,
	}
}

// HistoryCopy — копия истории переходов (для audit/UI).
func (p *Position) HistoryCopy() []Transition {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]Transition, len(p.History))
	copy(out, p.History)
	return out
}
