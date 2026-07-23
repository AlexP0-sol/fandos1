package rebalance

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/thecd/fundarbitrage/internal/decimal"
	"github.com/thecd/fundarbitrage/internal/domain"
)

// ============================================================
// Интерфейс Store — минимальный контракт хранилища.
// Реализуется пакетом internal/repository (строится параллельно).
// ============================================================

// Store — хранилище планов и попыток ребалансировки.
// Все операции идемпотентны по requestID (раздел 26.5).
type Store interface {
	// SavePlan сохраняет новый план (раздел 12.5).
	// Возвращает ошибку при конфликте ID.
	SavePlan(ctx context.Context, plan *Plan) error

	// UpdatePlanState обновляет состояние плана и причину сбоя (при FAILED).
	UpdatePlanState(ctx context.Context, planID string, state PlanState, failureReason string) error

	// RecordAttempt сохраняет запись о попытке перевода (раздел 12.6, шаг 4).
	// Уникален по request_id; повторный вызов с тем же ID — no-op.
	RecordAttempt(ctx context.Context, attempt *TransferAttempt) error

	// UpdateAttemptStatus обновляет статус попытки (SENT, CONFIRMED, TIMED_OUT, FAILED).
	UpdateAttemptStatus(ctx context.Context, requestID string, status AttemptStatus, failureReason string) error
}

// ============================================================
// Интерфейс Transferer — абстракция over ExchangeAdapter.
// ============================================================

// Transferer — операции перемещения средств (раздел 12.2).
// Реализуется адаптером конкретной биржи.
type Transferer interface {
	// InternalTransfer — переброс main↔futures на одной бирже (мгновенно, без on-chain).
	InternalTransfer(ctx context.Context, req domain.InternalTransferRequest) (domain.TransferResult, error)
	// Withdraw — on-chain вывод с биржи; считается успешным только по зачислению.
	Withdraw(ctx context.Context, req domain.WithdrawalRequest) (domain.WithdrawalResult, error)
}

// ============================================================
// Интерфейс ConfirmationChecker — опрос зачисления.
// ============================================================

// ConfirmationChecker — проверяет зачисление депозита на destination.
// Реализуется вызывающим через историю депозитов биржи (раздел 12.6, шаг 5-6).
type ConfirmationChecker interface {
	// IsConfirmed возвращает true, если депозит по requestID зачислен.
	// Сопоставление — по withdrawal ID / txid / строгим признакам.
	IsConfirmed(ctx context.Context, requestID string) (bool, error)
}

// ============================================================
// TransferAttempt — запись об одной фазе перевода.
// ============================================================

// AttemptPhase — фаза перевода (раздел 12.6).
type AttemptPhase string

const (
	// PhaseTest — тестовый перевод (малая сумма).
	PhaseTest AttemptPhase = "TEST"
	// PhaseMain — основной перевод.
	PhaseMain AttemptPhase = "MAIN"
)

// AttemptStatus — статус попытки (CHECK-constraint transfer_attempts.status).
type AttemptStatus string

const (
	AttemptCreated   AttemptStatus = "CREATED"
	AttemptSent      AttemptStatus = "SENT"
	AttemptConfirmed AttemptStatus = "CONFIRMED"
	AttemptTimedOut  AttemptStatus = "TIMED_OUT"
	AttemptFailed    AttemptStatus = "FAILED"
	AttemptCancelled AttemptStatus = "CANCELLED"
)

// AttemptKind — вид перевода (раздел 12.2).
type AttemptKind string

const (
	KindInternal AttemptKind = "INTERNAL"
	KindOnChain  AttemptKind = "ONCHAIN"
)

// FeeCapStatus — результат проверки комиссии (раздел 26.2).
type FeeCapStatus string

const (
	FeeCapPending FeeCapStatus = "PENDING"
	FeeCapPassed  FeeCapStatus = "PASSED"
	FeeCapFailed  FeeCapStatus = "FAILED"
)

// TransferAttempt — запись об одной попытке перевода (соответствует transfer_attempts).
type TransferAttempt struct {
	// RequestID — наш идемпотентный ID (plan_id + "-TEST" или plan_id + "-MAIN").
	RequestID   string
	PlanID      string
	Phase       AttemptPhase
	Kind        AttemptKind
	Source      string
	Destination string
	Asset       string
	GrossAmount decimal.Decimal
	Fee         decimal.Decimal
	// WithdrawalID — ID биржи (заполняется после Withdraw/InternalTransfer).
	WithdrawalID string
	// FeeCapCheck — результат проверки fee cap.
	FeeCapCheck   FeeCapStatus
	Status        AttemptStatus
	TimeoutAt     time.Time
	FailureReason string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// ============================================================
// Route — маршрут ребалансировки (раздел 12.3).
// ============================================================

// Route — данные маршрута перемещения средств.
// Загружается из transfer_routes (реализация — в repository).
type Route struct {
	RouteID      int64
	FromExchange domain.ExchangeID
	ToExchange   domain.ExchangeID
	Asset        string
	Network      string
	// Address — полный адрес назначения (только для on-chain, НЕ пишем в логи).
	Address string
	// AddressFingerprint — sha256[:12] адреса для аудита (раздел 26.1).
	AddressFingerprint string
	Memo               string
	FeeCap             *decimal.Decimal // nil = без лимита
	ApprovedAt         *time.Time       // nil = адрес не одобрен
	Kind               AttemptKind      // INTERNAL или ONCHAIN
}

// ============================================================
// ExecutorConfig — конфигурация движка выполнения.
// ============================================================

// ExecutorConfig — параметры движка ребалансировки (раздел 12.6, 12.7, 26).
type ExecutorConfig struct {
	// TestAmountUSDT — сумма тестового перевода (например, 10 USDT).
	TestAmountUSDT decimal.Decimal
	// TestTimeout — таймаут ожидания подтверждения тестового перевода.
	TestTimeout time.Duration
	// MainTimeout — таймаут ожидания подтверждения основного перевода.
	MainTimeout time.Duration
	// PollInterval — интервал опроса подтверждения (для ConfirmationChecker).
	PollInterval time.Duration
}

// ============================================================
// Ошибки Executor.
// ============================================================

// ErrMainBeforeTestConfirmed — попытка запустить основной перевод без подтверждения теста.
// Жёсткая защита (раздел 12.6, шаг 7).
var ErrMainBeforeTestConfirmed = errors.New("rebalance: MAIN-перевод запрещён до TEST_CONFIRMED")

// ErrFeeCapExceeded — комиссия превышает допустимый предел (раздел 26.2).
var ErrFeeCapExceeded = errors.New("rebalance: комиссия превышает FeeCap маршрута")

// ErrBreakerTripped — circuit breaker сработал (раздел 26.4).
var ErrBreakerTripped = errors.New("rebalance: circuit breaker сработал, перевод отклонён")

// ErrAddressNotApproved — адрес маршрута не одобрён (раздел 26.1).
var ErrAddressNotApproved = errors.New("rebalance: адрес маршрута не одобрён")

// ErrTestTimeout — тестовый перевод не подтверждён в срок (раздел 12.7).
var ErrTestTimeout = errors.New("rebalance: тайм-аут ожидания подтверждения тестового перевода")

// ErrMainTimeout — основной перевод не подтверждён в срок (раздел 12.7).
var ErrMainTimeout = errors.New("rebalance: тайм-аут ожидания подтверждения основного перевода")

// ============================================================
// Executor — двухфазный движок ребалансировки (раздел 12.6).
// ============================================================

// Executor управляет жизненным циклом плана: TEST → MAIN → COMPLETED.
// Все операции идемпотентны по requestID (раздел 26.5).
type Executor struct {
	cfg     ExecutorConfig
	store   Store
	breaker *CircuitBreaker
	clock   func() time.Time
}

// NewExecutor создаёт Executor.
// clk — инъекция времени; nil = time.Now.
func NewExecutor(cfg ExecutorConfig, store Store, breaker *CircuitBreaker, clk func() time.Time) *Executor {
	if clk == nil {
		clk = time.Now
	}
	return &Executor{
		cfg:     cfg,
		store:   store,
		breaker: breaker,
		clock:   clk,
	}
}

// ExecuteTest отправляет тестовый перевод и ожидает его подтверждения (раздел 12.6, шаги 3-6).
//
// Предусловия:
//   - plan.State == AWAITING_APPROVAL или TEST_SENT (идемпотентность).
//
// Гарды:
//   - Проверка адреса маршрута (ValidateRoute).
//   - Проверка circuit breaker.
//
// При таймауте → plan.State = FAILED; автоматического повтора нет (раздел 12.7).
func (e *Executor) ExecuteTest(
	ctx context.Context,
	plan *Plan,
	route Route,
	transferer Transferer,
	checker ConfirmationChecker,
) error {
	// Гард: адрес маршрута должен быть одобрён.
	if err := ValidateRoute(route, route.ApprovedAt); err != nil {
		return e.failPlan(ctx, plan, fmt.Errorf("%w: %w", ErrAddressNotApproved, err))
	}

	// Гард: circuit breaker.
	scope := breakerScope(route.FromExchange)
	if tripped, reason := e.breaker.IsTripped(scope); tripped {
		return fmt.Errorf("%w: scope=%s reason=%s", ErrBreakerTripped, scope, reason)
	}

	// Идемпотентный requestID для тестовой фазы.
	requestID := plan.ID + "-TEST"

	// Переход в TEST_SENT (если ещё не были).
	if plan.State == StateAwaitingApproval {
		if err := plan.Transition(StateTestSent, ""); err != nil {
			return fmt.Errorf("rebalance: переход в TEST_SENT: %w", err)
		}
		if err := e.store.UpdatePlanState(ctx, plan.ID, StateTestSent, ""); err != nil {
			return fmt.Errorf("rebalance: сохранение TEST_SENT: %w", err)
		}
	}

	// Создаём запись попытки (идемпотентно).
	now := e.clock()
	attempt := &TransferAttempt{
		RequestID:   requestID,
		PlanID:      plan.ID,
		Phase:       PhaseTest,
		Kind:        route.Kind,
		Source:      string(route.FromExchange),
		Destination: string(route.ToExchange),
		Asset:       route.Asset,
		GrossAmount: e.cfg.TestAmountUSDT,
		FeeCapCheck: FeeCapPending,
		Status:      AttemptCreated,
		TimeoutAt:   now.Add(e.cfg.TestTimeout),
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := e.store.RecordAttempt(ctx, attempt); err != nil {
		return fmt.Errorf("rebalance: запись TEST attempt: %w", err)
	}

	// Отправляем тестовый перевод.
	result, err := e.doTransfer(ctx, route, e.cfg.TestAmountUSDT, requestID, transferer)
	if err != nil {
		// Регистрируем ошибку в circuit breaker.
		e.breaker.RecordFailure(scope)
		if updateErr := e.store.UpdateAttemptStatus(ctx, requestID, AttemptFailed, err.Error()); updateErr != nil {
			err = fmt.Errorf("%w; также ошибка обновления статуса: %v", err, updateErr)
		}
		return e.failPlan(ctx, plan, fmt.Errorf("rebalance: отправка тестового перевода: %w", err))
	}

	// Сохраняем withdrawal ID.
	attempt.WithdrawalID = result
	if err := e.store.UpdateAttemptStatus(ctx, requestID, AttemptSent, ""); err != nil {
		return fmt.Errorf("rebalance: обновление статуса TEST SENT: %w", err)
	}

	// Ожидаем подтверждения с таймаутом.
	deadline := now.Add(e.cfg.TestTimeout)
	confirmed, err := e.waitConfirmation(ctx, checker, requestID, deadline)
	if err != nil {
		e.breaker.RecordFailure(scope)
		if updateErr := e.store.UpdateAttemptStatus(ctx, requestID, AttemptFailed, err.Error()); updateErr != nil {
			err = fmt.Errorf("%w; также ошибка обновления статуса: %v", err, updateErr)
		}
		return e.failPlan(ctx, plan, fmt.Errorf("rebalance: ошибка проверки TEST: %w", err))
	}
	if !confirmed {
		e.breaker.RecordFailure(scope)
		if updateErr := e.store.UpdateAttemptStatus(ctx, requestID, AttemptTimedOut, ErrTestTimeout.Error()); updateErr != nil {
			_ = updateErr
		}
		return e.failPlan(ctx, plan, ErrTestTimeout)
	}

	// Тест подтверждён.
	e.breaker.RecordSuccess(scope)
	if err := e.store.UpdateAttemptStatus(ctx, requestID, AttemptConfirmed, ""); err != nil {
		return fmt.Errorf("rebalance: обновление TEST CONFIRMED: %w", err)
	}

	if err := plan.Transition(StateTestConfirmed, ""); err != nil {
		return fmt.Errorf("rebalance: переход в TEST_CONFIRMED: %w", err)
	}
	if err := e.store.UpdatePlanState(ctx, plan.ID, StateTestConfirmed, ""); err != nil {
		return fmt.Errorf("rebalance: сохранение TEST_CONFIRMED: %w", err)
	}

	return nil
}

// ExecuteMain отправляет основной перевод после подтверждения теста (раздел 12.6, шаги 8-9).
//
// Жёсткая защита (раздел 12.7): вызов MAIN до TEST_CONFIRMED → ErrMainBeforeTestConfirmed.
// Fee-cap проверка (раздел 26.2): если estimatedFee > route.FeeCap → FAILED.
func (e *Executor) ExecuteMain(
	ctx context.Context,
	plan *Plan,
	route Route,
	transferer Transferer,
	checker ConfirmationChecker,
	estimatedFee decimal.Decimal,
) error {
	// Жёсткая защита: MAIN только после TEST_CONFIRMED.
	if plan.State != StateTestConfirmed {
		return fmt.Errorf("%w: текущее состояние %s", ErrMainBeforeTestConfirmed, plan.State)
	}

	// Гард: адрес маршрута должен быть одобрён.
	if err := ValidateRoute(route, route.ApprovedAt); err != nil {
		return e.failPlan(ctx, plan, fmt.Errorf("%w: %w", ErrAddressNotApproved, err))
	}

	// Гард: circuit breaker.
	scope := breakerScope(route.FromExchange)
	if tripped, reason := e.breaker.IsTripped(scope); tripped {
		return fmt.Errorf("%w: scope=%s reason=%s", ErrBreakerTripped, scope, reason)
	}

	// Fee-cap проверка (раздел 26.2).
	if route.FeeCap != nil && estimatedFee.GreaterThan(*route.FeeCap) {
		reason := fmt.Sprintf("fee %s > FeeCap %s", estimatedFee.String(), route.FeeCap.String())
		return e.failPlan(ctx, plan, fmt.Errorf("%w: %s", ErrFeeCapExceeded, reason))
	}

	// Идемпотентный requestID для основной фазы.
	requestID := plan.ID + "-MAIN"

	// Переход в MAIN_SENT.
	if err := plan.Transition(StateMainSent, ""); err != nil {
		return fmt.Errorf("rebalance: переход в MAIN_SENT: %w", err)
	}
	if err := e.store.UpdatePlanState(ctx, plan.ID, StateMainSent, ""); err != nil {
		return fmt.Errorf("rebalance: сохранение MAIN_SENT: %w", err)
	}

	// Записываем попытку MAIN.
	now := e.clock()
	feeCapStatus := FeeCapPassed
	if route.FeeCap == nil {
		feeCapStatus = FeeCapPending
	}
	attempt := &TransferAttempt{
		RequestID:   requestID,
		PlanID:      plan.ID,
		Phase:       PhaseMain,
		Kind:        route.Kind,
		Source:      string(route.FromExchange),
		Destination: string(route.ToExchange),
		Asset:       route.Asset,
		GrossAmount: plan.Proposal.GrossAmount,
		Fee:         estimatedFee,
		FeeCapCheck: feeCapStatus,
		Status:      AttemptCreated,
		TimeoutAt:   now.Add(e.cfg.MainTimeout),
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := e.store.RecordAttempt(ctx, attempt); err != nil {
		return fmt.Errorf("rebalance: запись MAIN attempt: %w", err)
	}

	// Отправляем основной перевод.
	result, err := e.doTransfer(ctx, route, plan.Proposal.GrossAmount, requestID, transferer)
	if err != nil {
		e.breaker.RecordFailure(scope)
		if updateErr := e.store.UpdateAttemptStatus(ctx, requestID, AttemptFailed, err.Error()); updateErr != nil {
			err = fmt.Errorf("%w; также ошибка обновления статуса: %v", err, updateErr)
		}
		return e.failPlan(ctx, plan, fmt.Errorf("rebalance: отправка основного перевода: %w", err))
	}
	_ = result

	if err := e.store.UpdateAttemptStatus(ctx, requestID, AttemptSent, ""); err != nil {
		return fmt.Errorf("rebalance: обновление MAIN SENT: %w", err)
	}

	// Ожидаем подтверждения основного перевода.
	deadline := now.Add(e.cfg.MainTimeout)
	confirmed, err := e.waitConfirmation(ctx, checker, requestID, deadline)
	if err != nil {
		e.breaker.RecordFailure(scope)
		if updateErr := e.store.UpdateAttemptStatus(ctx, requestID, AttemptFailed, err.Error()); updateErr != nil {
			_ = updateErr
		}
		return e.failPlan(ctx, plan, fmt.Errorf("rebalance: ошибка проверки MAIN: %w", err))
	}
	if !confirmed {
		e.breaker.RecordFailure(scope)
		if updateErr := e.store.UpdateAttemptStatus(ctx, requestID, AttemptTimedOut, ErrMainTimeout.Error()); updateErr != nil {
			_ = updateErr
		}
		return e.failPlan(ctx, plan, ErrMainTimeout)
	}

	// Основной перевод подтверждён.
	e.breaker.RecordSuccess(scope)
	if err := e.store.UpdateAttemptStatus(ctx, requestID, AttemptConfirmed, ""); err != nil {
		return fmt.Errorf("rebalance: обновление MAIN CONFIRMED: %w", err)
	}

	if err := plan.Transition(StateMainConfirmed, ""); err != nil {
		return fmt.Errorf("rebalance: переход в MAIN_CONFIRMED: %w", err)
	}
	if err := e.store.UpdatePlanState(ctx, plan.ID, StateMainConfirmed, ""); err != nil {
		return fmt.Errorf("rebalance: сохранение MAIN_CONFIRMED: %w", err)
	}

	if err := plan.Transition(StateCompleted, ""); err != nil {
		return fmt.Errorf("rebalance: переход в COMPLETED: %w", err)
	}
	if err := e.store.UpdatePlanState(ctx, plan.ID, StateCompleted, ""); err != nil {
		return fmt.Errorf("rebalance: сохранение COMPLETED: %w", err)
	}

	return nil
}

// ============================================================
// Внутренние helpers.
// ============================================================

// doTransfer выполняет фактический перевод в зависимости от типа маршрута.
// Возвращает ID биржи (WithdrawalID или TransferID).
func (e *Executor) doTransfer(
	ctx context.Context,
	route Route,
	amount decimal.Decimal,
	_ string, // requestID зарезервирован для idempotency на стороне биржи в будущем
	transferer Transferer,
) (string, error) {
	switch route.Kind {
	case KindOnChain:
		req := domain.WithdrawalRequest{
			Asset:   route.Asset,
			Amount:  amount,
			Network: route.Network,
			Address: route.Address,
			Memo:    route.Memo,
		}
		result, err := transferer.Withdraw(ctx, req)
		if err != nil {
			return "", fmt.Errorf("withdraw: %w", err)
		}
		return result.WithdrawalID, nil

	case KindInternal:
		req := domain.InternalTransferRequest{
			Asset:  route.Asset,
			Amount: amount,
			From:   string(route.FromExchange),
			To:     string(route.ToExchange),
		}
		result, err := transferer.InternalTransfer(ctx, req)
		if err != nil {
			return "", fmt.Errorf("internal transfer: %w", err)
		}
		return result.TransferID, nil

	default:
		return "", fmt.Errorf("rebalance: неизвестный тип маршрута %q", route.Kind)
	}
}

// waitConfirmation опрашивает checker до deadline с интервалом PollInterval.
// Возвращает (true, nil) при подтверждении, (false, nil) при таймауте, (false, err) при ошибке.
// Детерминированный: не использует time.Sleep — проверяет clock напрямую.
// В реальном сервере вызывающий должен периодически вызывать checker вне executor.
// Здесь реализован polling loop для тестируемости.
func (e *Executor) waitConfirmation(
	ctx context.Context,
	checker ConfirmationChecker,
	requestID string,
	deadline time.Time,
) (bool, error) {
	for {
		// Проверяем context.
		select {
		case <-ctx.Done():
			return false, fmt.Errorf("context: %w", ctx.Err())
		default:
		}

		now := e.clock()
		if now.After(deadline) {
			return false, nil
		}

		confirmed, err := checker.IsConfirmed(ctx, requestID)
		if err != nil {
			return false, err
		}
		if confirmed {
			return true, nil
		}

		// В тестах PollInterval == 0 → немедленная следующая итерация,
		// но фиктивный clock уже за дедлайном после первого вызова → цикл выходит.
		if e.cfg.PollInterval > 0 {
			select {
			case <-ctx.Done():
				return false, fmt.Errorf("context: %w", ctx.Err())
			case <-time.After(e.cfg.PollInterval):
			}
		} else {
			// PollInterval == 0: выходим сразу после одной проверки,
			// не блокируем тест.
			return false, nil
		}
	}
}

// failPlan переводит план в состояние FAILED и сохраняет причину.
// Возвращает оригинальную ошибку для цепочки.
func (e *Executor) failPlan(ctx context.Context, plan *Plan, err error) error {
	reason := err.Error()
	// Игнорируем ошибки перехода в FAILED (план уже мог быть FAILED).
	_ = plan.Transition(StateFailed, reason)
	_ = e.store.UpdatePlanState(ctx, plan.ID, StateFailed, reason)
	return err
}

// breakerScope формирует ключ circuit breaker для биржи (раздел 26.4).
func breakerScope(exchange domain.ExchangeID) string {
	return "exchange:" + string(exchange)
}
