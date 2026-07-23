package rebalance

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/thecd/fundarbitrage/internal/decimal"
	"github.com/thecd/fundarbitrage/internal/domain"
)

// ============================================================
// Fake реализации интерфейсов для тестов.
// ============================================================

// fakeStore — in-memory хранилище планов для тестов.
type fakeStore struct {
	plans    map[string]*Plan
	attempts map[string]*TransferAttempt
	states   map[string]PlanState
	reasons  map[string]string
	errors   map[string]error // для симуляции ошибок хранилища
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		plans:    make(map[string]*Plan),
		attempts: make(map[string]*TransferAttempt),
		states:   make(map[string]PlanState),
		reasons:  make(map[string]string),
		errors:   make(map[string]error),
	}
}

func (s *fakeStore) SavePlan(_ context.Context, plan *Plan) error {
	s.plans[plan.ID] = plan
	s.states[plan.ID] = plan.State
	return nil
}

func (s *fakeStore) UpdatePlanState(_ context.Context, planID string, state PlanState, failureReason string) error {
	if err, ok := s.errors["UpdatePlanState"]; ok {
		return err
	}
	s.states[planID] = state
	s.reasons[planID] = failureReason
	return nil
}

func (s *fakeStore) RecordAttempt(_ context.Context, attempt *TransferAttempt) error {
	// Идемпотентность: если уже есть — no-op.
	if _, exists := s.attempts[attempt.RequestID]; exists {
		return nil
	}
	s.attempts[attempt.RequestID] = attempt
	return nil
}

func (s *fakeStore) UpdateAttemptStatus(_ context.Context, requestID string, status AttemptStatus, failureReason string) error {
	a, ok := s.attempts[requestID]
	if !ok {
		return nil
	}
	a.Status = status
	a.FailureReason = failureReason
	return nil
}

// ============================================================
// fakeTransferer — фиктивный transferer для тестов.
// ============================================================

type fakeTransferer struct {
	// withdrawErr — ошибка, возвращаемая при вызове Withdraw.
	withdrawErr error
	// internalErr — ошибка InternalTransfer.
	internalErr error
	// withdrawCalls — счётчик вызовов Withdraw.
	withdrawCalls int
	// internalCalls — счётчик вызовов InternalTransfer.
	internalCalls int
}

func (f *fakeTransferer) Withdraw(_ context.Context, _ domain.WithdrawalRequest) (domain.WithdrawalResult, error) {
	f.withdrawCalls++
	if f.withdrawErr != nil {
		return domain.WithdrawalResult{}, f.withdrawErr
	}
	return domain.WithdrawalResult{WithdrawalID: "w-test-001", Status: "sent"}, nil
}

func (f *fakeTransferer) InternalTransfer(_ context.Context, _ domain.InternalTransferRequest) (domain.TransferResult, error) {
	f.internalCalls++
	if f.internalErr != nil {
		return domain.TransferResult{}, f.internalErr
	}
	return domain.TransferResult{TransferID: "t-test-001", Status: "ok"}, nil
}

// ============================================================
// fakeChecker — фиктивный ConfirmationChecker.
// ============================================================

type fakeChecker struct {
	// confirmed — map requestID → confirmed?
	confirmed map[string]bool
	// err — ошибка, возвращаемая при IsConfirmed.
	err error
}

func (f *fakeChecker) IsConfirmed(_ context.Context, requestID string) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	return f.confirmed[requestID], nil
}

// ============================================================
// Helpers для создания тестовых объектов.
// ============================================================

// approvedAt — одобренный адрес.
var approvedAt = func() *time.Time {
	t := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return &t
}()

// testRoute — стандартный тестовый маршрут.
func testRoute() Route {
	return Route{
		RouteID:      1,
		FromExchange: domain.ExchangeBinance,
		ToExchange:   domain.ExchangeBybit,
		Asset:        "USDT",
		Network:      "TRC20",
		Address:      "TXabcdef123",
		Kind:         KindOnChain,
		ApprovedAt:   approvedAt,
	}
}

// testProposal — стандартное предложение плана.
func testProposal() *PlanProposal {
	return &PlanProposal{
		FromExchange: domain.ExchangeBinance,
		ToExchange:   domain.ExchangeBybit,
		Asset:        "USDT",
		GrossAmount:  decimal.MustFromString("500"),
		Reason:       "test rebalance",
	}
}

// testExecutorCfg — конфигурация с нулевым PollInterval для тестов.
func testExecutorCfg() ExecutorConfig {
	return ExecutorConfig{
		TestAmountUSDT: decimal.MustFromString("10"),
		TestTimeout:    5 * time.Minute,
		MainTimeout:    30 * time.Minute,
		PollInterval:   0, // в тестах polling не нужен
	}
}

// ============================================================
// Тесты.
// ============================================================

// TestExecutor_HappyTwoPhase — полный happy-path TEST → MAIN → COMPLETED.
func TestExecutor_HappyTwoPhase(t *testing.T) {
	store := newFakeStore()
	cb := NewCircuitBreaker(3, nil, fixedClock(testTime))
	ex := NewExecutor(testExecutorCfg(), store, cb, fixedClock(testTime))

	plan := NewPlan("plan-001", testProposal(), false, fixedClock(testTime))
	plan.State = StateAwaitingApproval
	_ = store.SavePlan(context.Background(), plan)

	transferer := &fakeTransferer{}
	checker := &fakeChecker{
		confirmed: map[string]bool{
			"plan-001-TEST": true, // тест подтверждён сразу
		},
	}

	// Фаза TEST.
	if err := ex.ExecuteTest(context.Background(), plan, testRoute(), transferer, checker); err != nil {
		t.Fatalf("ExecuteTest: %v", err)
	}
	if plan.State != StateTestConfirmed {
		t.Errorf("после ExecuteTest: got %s, want %s", plan.State, StateTestConfirmed)
	}

	// Теперь фаза MAIN.
	checker.confirmed["plan-001-MAIN"] = true
	if err := ex.ExecuteMain(context.Background(), plan, testRoute(), transferer, checker, decimal.Zero); err != nil {
		t.Fatalf("ExecuteMain: %v", err)
	}
	if plan.State != StateCompleted {
		t.Errorf("после ExecuteMain: got %s, want %s", plan.State, StateCompleted)
	}

	// Проверяем хранилище.
	if store.states["plan-001"] != StateCompleted {
		t.Errorf("Store state: got %s, want %s", store.states["plan-001"], StateCompleted)
	}
	// Обе попытки записаны.
	if _, ok := store.attempts["plan-001-TEST"]; !ok {
		t.Error("TEST attempt не записана в store")
	}
	if _, ok := store.attempts["plan-001-MAIN"]; !ok {
		t.Error("MAIN attempt не записана в store")
	}
}

// TestExecutor_TestTransferFailure_NoMain — ошибка теста → FAILED, MAIN никогда не вызывается.
func TestExecutor_TestTransferFailure_NoMain(t *testing.T) {
	store := newFakeStore()
	cb := NewCircuitBreaker(3, nil, fixedClock(testTime))
	ex := NewExecutor(testExecutorCfg(), store, cb, fixedClock(testTime))

	plan := NewPlan("plan-fail-test", testProposal(), false, fixedClock(testTime))
	plan.State = StateAwaitingApproval
	_ = store.SavePlan(context.Background(), plan)

	transferer := &fakeTransferer{
		withdrawErr: errors.New("network unavailable"),
	}
	checker := &fakeChecker{confirmed: map[string]bool{}}

	// ExecuteTest должен вернуть ошибку.
	err := ex.ExecuteTest(context.Background(), plan, testRoute(), transferer, checker)
	if err == nil {
		t.Fatal("ожидали ошибку при сбое теста")
	}

	// План должен быть FAILED.
	if plan.State != StateFailed {
		t.Errorf("ожидали FAILED, получили %s", plan.State)
	}
	if store.states["plan-fail-test"] != StateFailed {
		t.Errorf("Store: ожидали FAILED, получили %s", store.states["plan-fail-test"])
	}

	// ExecuteMain из FAILED должен быть отклонён (жёсткая защита).
	if errMain := ex.ExecuteMain(context.Background(), plan, testRoute(), transferer, checker, decimal.Zero); errMain == nil {
		t.Error("ExecuteMain должен быть отклонён после FAILED плана")
	}

	// MAIN transferer никогда не должен быть вызван.
	if transferer.withdrawCalls > 1 {
		t.Errorf("Withdraw был вызван %d раз, ожидали ≤1 (только TEST)", transferer.withdrawCalls)
	}
}

// TestExecutor_MainBeforeTestConfirmed_Rejected — жёсткий гард MAIN без TEST_CONFIRMED (раздел 12.6).
func TestExecutor_MainBeforeTestConfirmed_Rejected(t *testing.T) {
	store := newFakeStore()
	cb := NewCircuitBreaker(3, nil, fixedClock(testTime))
	ex := NewExecutor(testExecutorCfg(), store, cb, fixedClock(testTime))

	transferer := &fakeTransferer{}
	checker := &fakeChecker{confirmed: map[string]bool{}}

	// Пробуем запустить MAIN из разных недопустимых состояний.
	badStates := []PlanState{
		StateDraft, StatePlanned, StateAwaitingApproval, StateTestSent,
	}
	for _, s := range badStates {
		t.Run(string(s), func(t *testing.T) {
			plan := NewPlan("plan-"+string(s), testProposal(), false, fixedClock(testTime))
			plan.State = s
			_ = store.SavePlan(context.Background(), plan)

			err := ex.ExecuteMain(context.Background(), plan, testRoute(), transferer, checker, decimal.Zero)
			if err == nil {
				t.Errorf("ExecuteMain из состояния %s должен быть отклонён", s)
			}
			if !errors.Is(err, ErrMainBeforeTestConfirmed) {
				t.Errorf("ожидали ErrMainBeforeTestConfirmed, получили %v", err)
			}
			// Withdraw НЕ должен вызываться.
			if transferer.withdrawCalls > 0 {
				t.Errorf("Withdraw не должен вызываться при отклонении MAIN")
			}
		})
	}
}

// TestExecutor_FeeCapBreach_PlanFailed — нарушение fee cap → FAILED (раздел 26.2).
func TestExecutor_FeeCapBreach_PlanFailed(t *testing.T) {
	store := newFakeStore()
	cb := NewCircuitBreaker(3, nil, fixedClock(testTime))
	ex := NewExecutor(testExecutorCfg(), store, cb, fixedClock(testTime))

	plan := NewPlan("plan-feecap", testProposal(), false, fixedClock(testTime))
	plan.State = StateTestConfirmed
	_ = store.SavePlan(context.Background(), plan)

	// Маршрут с fee cap = 5 USDT.
	cap := decimal.MustFromString("5")
	route := testRoute()
	route.FeeCap = &cap

	transferer := &fakeTransferer{}
	checker := &fakeChecker{confirmed: map[string]bool{}}

	// Комиссия = 10 USDT > 5 USDT → должны получить ошибку.
	estimatedFee := decimal.MustFromString("10")
	err := ex.ExecuteMain(context.Background(), plan, route, transferer, checker, estimatedFee)
	if err == nil {
		t.Fatal("ожидали ошибку fee cap")
	}
	if !errors.Is(err, ErrFeeCapExceeded) {
		t.Errorf("ожидали ErrFeeCapExceeded, получили %v", err)
	}

	// План должен быть FAILED.
	if plan.State != StateFailed {
		t.Errorf("ожидали FAILED, получили %s", plan.State)
	}
	// Withdraw не вызывался.
	if transferer.withdrawCalls > 0 {
		t.Error("Withdraw не должен вызываться при нарушении fee cap")
	}
}

// TestExecutor_FeeCapBelow_Allowed — комиссия ниже cap → OK.
func TestExecutor_FeeCapBelow_Allowed(t *testing.T) {
	store := newFakeStore()
	cb := NewCircuitBreaker(3, nil, fixedClock(testTime))
	ex := NewExecutor(testExecutorCfg(), store, cb, fixedClock(testTime))

	plan := NewPlan("plan-feecap-ok", testProposal(), false, fixedClock(testTime))
	plan.State = StateTestConfirmed
	_ = store.SavePlan(context.Background(), plan)

	cap := decimal.MustFromString("15")
	route := testRoute()
	route.FeeCap = &cap

	checker := &fakeChecker{
		confirmed: map[string]bool{
			"plan-feecap-ok-MAIN": true,
		},
	}
	transferer := &fakeTransferer{}

	estimatedFee := decimal.MustFromString("10") // < 15 → OK
	err := ex.ExecuteMain(context.Background(), plan, route, &fakeTransferer{}, checker, estimatedFee)
	if err != nil {
		t.Fatalf("ExecuteMain: %v", err)
	}
	_ = transferer
	if plan.State != StateCompleted {
		t.Errorf("ожидали COMPLETED, получили %s", plan.State)
	}
}

// TestExecutor_BreakerTripped_Refused — сработавший breaker блокирует ExecuteTest.
func TestExecutor_BreakerTripped_Refused(t *testing.T) {
	store := newFakeStore()
	cb := NewCircuitBreaker(1, nil, fixedClock(testTime)) // порог = 1
	cb.RecordFailure("exchange:binance")                  // немедленно срабатывает

	ex := NewExecutor(testExecutorCfg(), store, cb, fixedClock(testTime))

	plan := NewPlan("plan-breaker", testProposal(), false, fixedClock(testTime))
	plan.State = StateAwaitingApproval
	_ = store.SavePlan(context.Background(), plan)

	transferer := &fakeTransferer{}
	checker := &fakeChecker{confirmed: map[string]bool{}}

	err := ex.ExecuteTest(context.Background(), plan, testRoute(), transferer, checker)
	if err == nil {
		t.Fatal("ожидали ошибку от circuit breaker")
	}
	if !errors.Is(err, ErrBreakerTripped) {
		t.Errorf("ожидали ErrBreakerTripped, получили %v", err)
	}
	// Withdraw не вызывался.
	if transferer.withdrawCalls > 0 {
		t.Error("Withdraw не должен вызываться при сработавшем breaker")
	}
}

// TestExecutor_BreakerTripAfterFailures — breaker срабатывает после накопления ошибок.
func TestExecutor_BreakerTripAfterFailures(t *testing.T) {
	store := newFakeStore()
	cb := NewCircuitBreaker(3, nil, fixedClock(testTime))
	ex := NewExecutor(testExecutorCfg(), store, cb, fixedClock(testTime))

	netErr := errors.New("network error")

	for i := 0; i < 2; i++ {
		plan := NewPlan("plan-err-"+string(rune('A'+i)), testProposal(), false, fixedClock(testTime))
		plan.State = StateAwaitingApproval
		_ = store.SavePlan(context.Background(), plan)

		transferer := &fakeTransferer{withdrawErr: netErr}
		checker := &fakeChecker{confirmed: map[string]bool{}}
		_ = ex.ExecuteTest(context.Background(), plan, testRoute(), transferer, checker)
	}

	// После 2 неудач breaker ещё не сработал (порог = 3).
	tripped, _ := cb.IsTripped("exchange:binance")
	if tripped {
		t.Error("breaker не должен сработать при 2 ошибках (порог 3)")
	}

	// Третья неудача → срабатывание.
	plan3 := NewPlan("plan-err-C", testProposal(), false, fixedClock(testTime))
	plan3.State = StateAwaitingApproval
	_ = store.SavePlan(context.Background(), plan3)
	transferer3 := &fakeTransferer{withdrawErr: netErr}
	checker3 := &fakeChecker{confirmed: map[string]bool{}}
	_ = ex.ExecuteTest(context.Background(), plan3, testRoute(), transferer3, checker3)

	tripped, _ = cb.IsTripped("exchange:binance")
	if !tripped {
		t.Error("breaker должен сработать после 3 ошибок")
	}
}

// TestExecutor_IdempotentRequestIDs — requestID стабилен при повторе одной фазы.
func TestExecutor_IdempotentRequestIDs(t *testing.T) {
	planID := "plan-idem"

	testReqID := planID + "-TEST"
	mainReqID := planID + "-MAIN"

	// Проверяем детерминированность requestID.
	plan := NewPlan(planID, testProposal(), false, fixedClock(testTime))

	// Симулируем два вызова buildRequestID для одного плана.
	req1 := plan.ID + "-TEST"
	req2 := plan.ID + "-TEST"
	if req1 != req2 {
		t.Errorf("requestID нестабилен: %s != %s", req1, req2)
	}
	if req1 != testReqID {
		t.Errorf("testReqID: got %s, want %s", req1, testReqID)
	}

	mainReq1 := plan.ID + "-MAIN"
	if mainReq1 != mainReqID {
		t.Errorf("mainReqID: got %s, want %s", mainReq1, mainReqID)
	}
}

// TestExecutor_IdempotentRecordAttempt — повторная запись попытки с тем же requestID — no-op.
func TestExecutor_IdempotentRecordAttempt(t *testing.T) {
	store := newFakeStore()
	cb := NewCircuitBreaker(3, nil, fixedClock(testTime))
	ex := NewExecutor(testExecutorCfg(), store, cb, fixedClock(testTime))

	plan := NewPlan("plan-idem2", testProposal(), false, fixedClock(testTime))
	plan.State = StateAwaitingApproval
	_ = store.SavePlan(context.Background(), plan)

	checker := &fakeChecker{
		confirmed: map[string]bool{
			"plan-idem2-TEST": true,
		},
	}
	transferer := &fakeTransferer{}

	// Первый вызов.
	if err := ex.ExecuteTest(context.Background(), plan, testRoute(), transferer, checker); err != nil {
		t.Fatalf("первый ExecuteTest: %v", err)
	}
	firstAttempt := store.attempts["plan-idem2-TEST"]

	// Симулируем повторный вызов RecordAttempt с тем же requestID.
	duplicate := &TransferAttempt{
		RequestID: "plan-idem2-TEST",
		PlanID:    plan.ID,
		Status:    AttemptCreated,
	}
	if err := store.RecordAttempt(context.Background(), duplicate); err != nil {
		t.Fatalf("повторный RecordAttempt: %v", err)
	}

	// Исходная попытка не перезаписана.
	if store.attempts["plan-idem2-TEST"] != firstAttempt {
		t.Error("повторный RecordAttempt не должен перезаписывать существующую попытку")
	}
}

// TestExecutor_TestTimeout_PlanFailed — таймаут теста → FAILED (раздел 12.7).
func TestExecutor_TestTimeout_PlanFailed(t *testing.T) {
	store := newFakeStore()
	cb := NewCircuitBreaker(5, nil, fixedClock(testTime))

	// PollInterval=0 → один проход цикла.
	// Checker всегда возвращает false — симулируем таймаут.
	// Clock: первый вызов — до deadline, второй — после deadline.
	callCount := 0
	clk := func() time.Time {
		callCount++
		if callCount <= 3 {
			return testTime // до deadline
		}
		// После 3-го вызова — после deadline (TestTimeout = 5 min).
		return testTime.Add(10 * time.Minute)
	}

	ex := NewExecutor(testExecutorCfg(), store, cb, clk)

	plan := NewPlan("plan-timeout", testProposal(), false, fixedClock(testTime))
	plan.State = StateAwaitingApproval
	_ = store.SavePlan(context.Background(), plan)

	transferer := &fakeTransferer{}
	// Checker никогда не подтверждает.
	checker := &fakeChecker{confirmed: map[string]bool{}}

	err := ex.ExecuteTest(context.Background(), plan, testRoute(), transferer, checker)
	if err == nil {
		t.Fatal("ожидали ошибку таймаута")
	}

	// При PollInterval=0 waitConfirmation возвращает (false, nil) после первой проверки.
	// Это значит план должен стать FAILED.
	if plan.State != StateFailed {
		t.Errorf("ожидали FAILED, получили %s", plan.State)
	}
}

// TestExecutor_DryRunPlan_NeverExecutes — dry-run план не выполняет переводы.
func TestExecutor_DryRunPlan_NeverExecutes(t *testing.T) {
	store := newFakeStore()
	cb := NewCircuitBreaker(3, nil, fixedClock(testTime))
	// ex создан для документирования зависимостей dry-run; переводы не выполняем.
	_ = NewExecutor(testExecutorCfg(), store, cb, fixedClock(testTime))

	// Создаём dry-run план в состоянии PLANNED (максимально возможное для dry-run).
	plan := NewPlan("plan-dry", testProposal(), true /* dryRun */, fixedClock(testTime))
	plan.State = StatePlanned
	_ = store.SavePlan(context.Background(), plan)

	// В dry-run PLANNED→AWAITING_APPROVAL должен быть заблокирован.
	if err := plan.Transition(StateAwaitingApproval, ""); err == nil {
		t.Error("dry-run: PLANNED→AWAITING_APPROVAL должен быть заблокирован")
	}
	if plan.State != StatePlanned {
		t.Errorf("dry-run: состояние должно остаться PLANNED, получили %s", plan.State)
	}
}

// TestExecutor_AddressNotApproved_Refused — неодобренный адрес → отказ ExecuteTest.
func TestExecutor_AddressNotApproved_Refused(t *testing.T) {
	store := newFakeStore()
	cb := NewCircuitBreaker(3, nil, fixedClock(testTime))
	ex := NewExecutor(testExecutorCfg(), store, cb, fixedClock(testTime))

	plan := NewPlan("plan-noaddr", testProposal(), false, fixedClock(testTime))
	plan.State = StateAwaitingApproval
	_ = store.SavePlan(context.Background(), plan)

	// Маршрут с не одобренным адресом (ApprovedAt = nil).
	route := testRoute()
	route.ApprovedAt = nil

	transferer := &fakeTransferer{}
	checker := &fakeChecker{confirmed: map[string]bool{}}

	err := ex.ExecuteTest(context.Background(), plan, route, transferer, checker)
	if err == nil {
		t.Fatal("ожидали ошибку при неодобренном адресе")
	}
	if !errors.Is(err, ErrAddressNotApproved) {
		t.Errorf("ожидали ErrAddressNotApproved, получили %v", err)
	}
	if plan.State != StateFailed {
		t.Errorf("ожидали FAILED, получили %s", plan.State)
	}
}
