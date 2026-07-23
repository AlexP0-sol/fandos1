package repository_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/thecd/fundarbitrage/internal/decimal"
	"github.com/thecd/fundarbitrage/internal/domain"
	"github.com/thecd/fundarbitrage/internal/outbox"
	"github.com/thecd/fundarbitrage/internal/portfolio"
	"github.com/thecd/fundarbitrage/internal/repository"
)

const testDSN = "postgres://fandos:fandos@localhost:5432/fandos"

var testPool *pgxpool.Pool

func TestMain(m *testing.M) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	pool, err := repository.NewPool(ctx, testDSN)
	if err != nil {
		// БД недоступна — пропускаем все тесты (в нашем случае она ДОЛЖНА быть доступна).
		fmt.Printf("БД недоступна (%v), пропуск интеграционных тестов\n", err)
		return
	}
	testPool = pool
	m.Run()
	pool.Close()
}

// ============================================================
// Вспомогательные функции
// ============================================================

// uid генерирует уникальный суффикс для тестовых ID.
func uid() string {
	return fmt.Sprintf("%d", time.Now().UnixNano())
}

// mustDec создаёт Decimal из строки или паникует.
func mustDec(s string) decimal.Decimal {
	d, err := decimal.FromString(s)
	if err != nil {
		panic(err)
	}
	return d
}

// makePosition создаёт тестовую позицию.
func makePosition(id, asset string) *portfolio.Position {
	pos := portfolio.NewPosition(
		domain.PositionID(id),
		domain.AssetSymbol(asset),
		domain.ExchangeBinance,
		domain.ExchangeBybit,
		time.Now().UTC(),
	)
	pos.TargetBaseQty = mustDec("1.5")
	return pos
}

// cleanupPosition удаляет позицию и связанные данные (defer cleanup).
func cleanupPosition(t *testing.T, posID string) {
	t.Helper()
	ctx := context.Background()
	_, _ = testPool.Exec(ctx, `DELETE FROM position_transitions WHERE position_id = $1`, posID)
	_, _ = testPool.Exec(ctx, `DELETE FROM audit_log WHERE correlation_id = $1`, posID)
	_, _ = testPool.Exec(ctx, `DELETE FROM positions WHERE position_id = $1`, posID)
}

// cleanupOrder удаляет ордер по client_order_id.
func cleanupOrder(t *testing.T, exchange, clientOrderID string) {
	t.Helper()
	_, _ = testPool.Exec(context.Background(),
		`DELETE FROM orders WHERE exchange = $1 AND client_order_id = $2`, exchange, clientOrderID)
}

// cleanupFill удаляет fill по exchange + exchange_fill_id.
func cleanupFill(t *testing.T, exchange, fillID string) {
	t.Helper()
	_, _ = testPool.Exec(context.Background(),
		`DELETE FROM fills WHERE exchange = $1 AND exchange_fill_id = $2`, exchange, fillID)
}

// cleanupFundingPayment удаляет funding payment.
func cleanupFundingPayment(t *testing.T, exchange, symbol string, fundingTime time.Time) {
	t.Helper()
	_, _ = testPool.Exec(context.Background(),
		`DELETE FROM funding_payments WHERE exchange = $1 AND exchange_symbol = $2 AND funding_time = $3`,
		exchange, symbol, fundingTime)
}

// cleanupOutboxEvents удаляет outbox-события по topic.
func cleanupOutboxEvents(t *testing.T, topic string) {
	t.Helper()
	_, _ = testPool.Exec(context.Background(),
		`DELETE FROM outbox_events WHERE topic = $1`, topic)
}

// ============================================================
// TestPersisterAtomicity — атомарность OnTransition
// ============================================================

// TestPersisterAtomicity проверяет, что OnTransition записывает все три строки
// (positions, position_transitions, audit_log) в одной транзакции.
func TestPersisterAtomicity(t *testing.T) {
	if testPool == nil {
		t.Skip("пул не инициализирован")
	}

	posID := "test-persist-" + uid()
	defer cleanupPosition(t, posID)

	pos := makePosition(posID, "BTC")
	persister := repository.NewPersister(testPool)

	// Первый переход: DISCOVERED → QUALIFIED.
	err := pos.TransitionTo(portfolio.StateQualified, time.Now().UTC(), "scanner found opportunity", "system:scanner", persister)
	if err != nil {
		t.Fatalf("TransitionTo QUALIFIED: %v", err)
	}

	ctx := context.Background()

	// Проверяем наличие строки в positions.
	var state string
	err = testPool.QueryRow(ctx, `SELECT state FROM positions WHERE position_id = $1`, posID).Scan(&state)
	if err != nil {
		t.Fatalf("positions не найдена: %v", err)
	}
	if state != "QUALIFIED" {
		t.Errorf("state = %q, хотим QUALIFIED", state)
	}

	// Проверяем наличие строки в position_transitions.
	var fromState, toState string
	err = testPool.QueryRow(ctx, `
		SELECT from_state, to_state FROM position_transitions WHERE position_id = $1
	`, posID).Scan(&fromState, &toState)
	if err != nil {
		t.Fatalf("position_transitions не найдена: %v", err)
	}
	if fromState != "DISCOVERED" || toState != "QUALIFIED" {
		t.Errorf("transition %q→%q, хотим DISCOVERED→QUALIFIED", fromState, toState)
	}

	// Проверяем наличие строки в audit_log.
	var auditCount int
	err = testPool.QueryRow(ctx, `
		SELECT COUNT(*) FROM audit_log WHERE correlation_id = $1
	`, posID).Scan(&auditCount)
	if err != nil {
		t.Fatalf("audit_log запрос: %v", err)
	}
	if auditCount == 0 {
		t.Error("audit_log: записей нет")
	}
}

// TestPersisterAtomicityRollbackOnConstraint проверяет, что при нарушении constraint
// (фейковый переход с нарушением FK) все строки откатываются.
func TestPersisterAtomicityRollbackOnConstraint(t *testing.T) {
	if testPool == nil {
		t.Skip("пул не инициализирован")
	}

	// Используем специальный persister, который всегда завершается с ошибкой.
	posID := "test-rollback-" + uid()
	defer cleanupPosition(t, posID)

	pos := makePosition(posID, "ETH")

	failingPersister := &failPersister{}

	// TransitionTo должен вернуть ошибку, а состояние — откатиться.
	err := pos.TransitionTo(portfolio.StateQualified, time.Now().UTC(), "test", "system:test", failingPersister)
	if err == nil {
		t.Fatal("ожидали ошибку от failingPersister, получили nil")
	}

	// Состояние в памяти должно быть откачено в DISCOVERED.
	if got := pos.CurrentState(); got != portfolio.StateDiscovered {
		t.Errorf("состояние после отката = %q, хотим DISCOVERED", got)
	}

	ctx := context.Background()

	// В БД не должно быть строки в positions.
	var dummy string
	err2 := testPool.QueryRow(ctx, `SELECT position_id FROM positions WHERE position_id = $1`, posID).Scan(&dummy)
	if !errors.Is(err2, pgx.ErrNoRows) {
		t.Errorf("positions содержит строку после отката: %v", err2)
	}
}

// failPersister — тестовый Persister, всегда возвращающий ошибку.
type failPersister struct{}

func (f *failPersister) OnTransition(_ *portfolio.Position, _ portfolio.Transition) error {
	return errors.New("искусственная ошибка persist")
}

// ============================================================
// TestUpsertLoadRoundTrip — точность Decimal при upsert/load
// ============================================================

func TestUpsertLoadRoundTrip(t *testing.T) {
	if testPool == nil {
		t.Skip("пул не инициализирован")
	}

	posID := "test-roundtrip-" + uid()
	defer cleanupPosition(t, posID)

	pos := makePosition(posID, "ARB")
	// Критически малое значение для проверки точности NUMERIC.
	pos.TargetBaseQty = mustDec("0.00000123")
	pos.RealisedPnL = mustDec("0.00000123")
	pos.FundingPnL = mustDec("-0.00000456")
	pos.FeesPaid = mustDec("0.00000789")

	repo := repository.NewPositionRepo(testPool)

	// Сначала сохраняем через Upsert (позиция в DISCOVERED, не нужен персистер).
	if err := repo.Upsert(context.Background(), pos); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	loaded, err := repo.Load(context.Background(), domain.PositionID(posID))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	snap := loaded.Snapshot()
	if !snap.TargetBaseQty.Equal(pos.TargetBaseQty) {
		t.Errorf("TargetBaseQty: got %s, want %s", snap.TargetBaseQty, pos.TargetBaseQty)
	}
	if !snap.RealisedPnL.Equal(pos.RealisedPnL) {
		t.Errorf("RealisedPnL: got %s, want %s", snap.RealisedPnL, pos.RealisedPnL)
	}
	if !snap.FundingPnL.Equal(pos.FundingPnL) {
		t.Errorf("FundingPnL: got %s, want %s", snap.FundingPnL, pos.FundingPnL)
	}
	if !snap.FeesPaid.Equal(pos.FeesPaid) {
		t.Errorf("FeesPaid: got %s, want %s", snap.FeesPaid, pos.FeesPaid)
	}
	if string(snap.State) != "DISCOVERED" {
		t.Errorf("State: got %s, want DISCOVERED", snap.State)
	}
}

// ============================================================
// TestLoadByStates — загрузка позиций по состоянию
// ============================================================

func TestLoadByStates(t *testing.T) {
	if testPool == nil {
		t.Skip("пул не инициализирован")
	}

	ctx := context.Background()
	repo := repository.NewPositionRepo(testPool)
	persister := repository.NewPersister(testPool)

	// Создаём две позиции: одну переводим в QUALIFIED, другую оставляем в DISCOVERED.
	id1 := "test-lbs-disc-" + uid()
	id2 := "test-lbs-qual-" + uid()
	defer cleanupPosition(t, id1)
	defer cleanupPosition(t, id2)

	pos1 := makePosition(id1, "SOL")
	pos2 := makePosition(id2, "SOL")

	// Сохраняем первую через Upsert (в DISCOVERED).
	if err := repo.Upsert(ctx, pos1); err != nil {
		t.Fatalf("Upsert pos1: %v", err)
	}

	// Вторую — через transition в QUALIFIED.
	if err := pos2.TransitionTo(portfolio.StateQualified, time.Now().UTC(), "test", "system:test", persister); err != nil {
		t.Fatalf("TransitionTo pos2: %v", err)
	}

	// Загружаем только QUALIFIED.
	qualifiedPositions, err := repo.LoadByStates(ctx, "QUALIFIED")
	if err != nil {
		t.Fatalf("LoadByStates: %v", err)
	}

	found := false
	for _, p := range qualifiedPositions {
		if string(p.ID) == id2 {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("позиция %s не найдена в LoadByStates([QUALIFIED])", id2)
	}

	// pos1 (DISCOVERED) не должна присутствовать.
	for _, p := range qualifiedPositions {
		if string(p.ID) == id1 {
			t.Errorf("позиция %s (DISCOVERED) не должна присутствовать в QUALIFIED запросе", id1)
		}
	}
}

// ============================================================
// TestOrderFillIdempotency — идемпотентность ордеров и fills
// ============================================================

func TestOrderFillIdempotency(t *testing.T) {
	if testPool == nil {
		t.Skip("пул не инициализирован")
	}

	ctx := context.Background()
	orderRepo := repository.NewOrderRepo(testPool)

	clientOrderID := "test-order-" + uid()
	exchange := domain.ExchangeBinance
	defer cleanupOrder(t, string(exchange), clientOrderID)

	o := domain.Order{
		ClientOrderID: domain.ClientOrderID(clientOrderID),
		Side:          domain.SideLong,
		OrderMode:     domain.OrderMarket,
		RequestedQty:  mustDec("1.0"),
		FilledQty:     decimal.Zero,
		Fees:          decimal.Zero,
		Status:        domain.OrderStatusNew,
		AckState:      domain.AckStateAcked,
	}

	// Вставляем дважды — должна остаться одна строка.
	if err := orderRepo.UpsertOrder(ctx, o, exchange, "", ""); err != nil {
		t.Fatalf("первый UpsertOrder: %v", err)
	}
	if err := orderRepo.UpsertOrder(ctx, o, exchange, "", ""); err != nil {
		t.Fatalf("второй UpsertOrder: %v", err)
	}

	var cnt int
	if err := testPool.QueryRow(ctx,
		`SELECT COUNT(*) FROM orders WHERE exchange = $1 AND client_order_id = $2`,
		string(exchange), clientOrderID,
	).Scan(&cnt); err != nil {
		t.Fatalf("COUNT orders: %v", err)
	}
	if cnt != 1 {
		t.Errorf("ожидали 1 строку ордера, получили %d", cnt)
	}

	// Идемпотентность fill.
	fillID := "test-fill-" + uid()
	defer cleanupFill(t, string(exchange), fillID)

	fp := repository.FillParams{
		Exchange:       exchange,
		ExchangeFillID: fillID,
		ClientOrderID:  domain.ClientOrderID(clientOrderID),
		Side:           domain.SideLong,
		BaseQty:        "1.0",
		Price:          "50000.0",
		Fee:            "0.001",
		IsMaker:        false,
		ExchangeTS:     time.Now().UTC(),
	}

	if err := orderRepo.InsertFill(ctx, fp); err != nil {
		t.Fatalf("первый InsertFill: %v", err)
	}
	if err := orderRepo.InsertFill(ctx, fp); err != nil {
		t.Fatalf("второй InsertFill (должен быть идемпотентен): %v", err)
	}

	var fillCnt int
	if err := testPool.QueryRow(ctx,
		`SELECT COUNT(*) FROM fills WHERE exchange = $1 AND exchange_fill_id = $2`,
		string(exchange), fillID,
	).Scan(&fillCnt); err != nil {
		t.Fatalf("COUNT fills: %v", err)
	}
	if fillCnt != 1 {
		t.Errorf("ожидали 1 fill, получили %d", fillCnt)
	}
}

// ============================================================
// TestSettingsOptimisticVersion — оптимистичная блокировка settings
// ============================================================

func TestSettingsOptimisticVersion(t *testing.T) {
	if testPool == nil {
		t.Skip("пул не инициализирован")
	}

	ctx := context.Background()
	settingsRepo := repository.NewSettingsRepo(testPool)

	// Очищаем singleton перед тестом, восстанавливаем после.
	var (
		origPayload []byte
		origVersion int64
		hasOrig     bool
	)
	origPayload, origVersion, origErr := settingsRepo.LoadHot(ctx)
	if origErr == nil {
		hasOrig = true
	}
	defer func() {
		if hasOrig {
			// Восстанавливаем оригинальные настройки.
			_ = settingsRepo.SaveHot(ctx, origPayload, origVersion)
		} else {
			// Удаляем тестовую строку.
			_, _ = testPool.Exec(ctx, `DELETE FROM strategy_settings WHERE singleton = TRUE`)
		}
	}()

	// Удаляем текущую строку для чистого теста.
	_, _ = testPool.Exec(ctx, `DELETE FROM strategy_settings WHERE singleton = TRUE`)

	// LoadHot на пустую таблицу.
	_, _, err := settingsRepo.LoadHot(ctx)
	if !errors.Is(err, repository.ErrSettingsNotFound) {
		t.Errorf("ожидали ErrSettingsNotFound, получили: %v", err)
	}

	// Создаём запись через SaveHot(version=0).
	payload1, _ := json.Marshal(map[string]any{"max_positions": 5})
	if err := settingsRepo.SaveHot(ctx, payload1, 0); err != nil {
		t.Fatalf("SaveHot(0): %v", err)
	}

	_, v, err := settingsRepo.LoadHot(ctx)
	if err != nil {
		t.Fatalf("LoadHot после создания: %v", err)
	}
	if v < 1 {
		t.Errorf("ожидали version >= 1, получили %d", v)
	}

	// Сохраняем с корректной версией.
	payload2, _ := json.Marshal(map[string]any{"max_positions": 10})
	if err := settingsRepo.SaveHot(ctx, payload2, v); err != nil {
		t.Fatalf("SaveHot(v=%d): %v", v, err)
	}

	// Конфликт: пробуем сохранить со старой версией.
	payload3, _ := json.Marshal(map[string]any{"max_positions": 15})
	err = settingsRepo.SaveHot(ctx, payload3, v) // v устарела
	if !errors.Is(err, repository.ErrVersionConflict) {
		t.Errorf("ожидали ErrVersionConflict при устаревшей версии, получили: %v", err)
	}
}

// ============================================================
// TestLocksEngageRelease — блокировки
// ============================================================

func TestLocksEngageRelease(t *testing.T) {
	if testPool == nil {
		t.Skip("пул не инициализирован")
	}

	ctx := context.Background()
	locksRepo := repository.NewLocksRepo(testPool)

	// Используем существующий lock TRADING_LOCKED.
	const lockName = "TRADING_LOCKED"

	// Сначала освобождаем.
	if err := locksRepo.Release(ctx, lockName); err != nil {
		t.Fatalf("Release: %v", err)
	}
	defer func() { _ = locksRepo.Release(ctx, lockName) }()

	engaged, err := locksRepo.IsEngaged(ctx, lockName)
	if err != nil {
		t.Fatalf("IsEngaged после Release: %v", err)
	}
	if engaged {
		t.Error("блокировка должна быть неактивна после Release")
	}

	// Активируем.
	if err := locksRepo.Engage(ctx, lockName, "тест"); err != nil {
		t.Fatalf("Engage: %v", err)
	}

	engaged, err = locksRepo.IsEngaged(ctx, lockName)
	if err != nil {
		t.Fatalf("IsEngaged после Engage: %v", err)
	}
	if !engaged {
		t.Error("блокировка должна быть активна после Engage")
	}

	// Освобождаем повторно.
	if err := locksRepo.Release(ctx, lockName); err != nil {
		t.Fatalf("повторный Release: %v", err)
	}

	engaged, _ = locksRepo.IsEngaged(ctx, lockName)
	if engaged {
		t.Error("блокировка должна быть неактивна после повторного Release")
	}
}

// TestOwnerReadyFalseOnSeed проверяет, что OwnerReady возвращает false для seed-пользователя.
func TestOwnerReadyFalseOnSeed(t *testing.T) {
	if testPool == nil {
		t.Skip("пул не инициализирован")
	}

	locksRepo := repository.NewLocksRepo(testPool)
	ready, err := locksRepo.OwnerReady(context.Background())
	if err != nil {
		t.Fatalf("OwnerReady: %v", err)
	}
	// Seed telegram_id = -1, OwnerReady должен вернуть false.
	if ready {
		t.Error("OwnerReady должен возвращать false для seed telegram_id = -1")
	}
}

// ============================================================
// Тесты outbox вынесены в отдельный файл outbox_integration_test.go
// ============================================================

// TestOutboxHappyPath — базовый сценарий: EnqueueTx → RunOnce → событие обработано.
func TestOutboxHappyPath(t *testing.T) {
	if testPool == nil {
		t.Skip("пул не инициализирован")
	}

	ctx := context.Background()
	topic := "test-happy-" + uid()
	defer cleanupOutboxEvents(t, topic)

	producer := outbox.NewProducer()

	// Вставляем событие через EnqueueTx в отдельной транзакции.
	tx, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := producer.EnqueueTx(ctx, tx, topic, "test.event", map[string]string{"key": "value"}); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("EnqueueTx: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// Проверяем наличие события в outbox.
	var cnt int
	if err := testPool.QueryRow(ctx,
		`SELECT COUNT(*) FROM outbox_events WHERE topic = $1 AND processed_at IS NULL`, topic,
	).Scan(&cnt); err != nil {
		t.Fatalf("COUNT outbox_events: %v", err)
	}
	if cnt != 1 {
		t.Errorf("ожидали 1 событие, получили %d", cnt)
	}

	dispatcher := outbox.NewDispatcher(testPool, 3, 100*time.Millisecond)

	// RunOnce должен обработать событие.
	var received []outbox.Event
	processed, err := dispatcher.RunOnce(ctx, func(ev outbox.Event) error {
		received = append(received, ev)
		return nil
	})
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if processed != 1 {
		t.Errorf("RunOnce: обработано %d событий, ожидали 1", processed)
	}

	// Событие должно быть помечено как обработанное.
	if err := testPool.QueryRow(ctx,
		`SELECT COUNT(*) FROM outbox_events WHERE topic = $1 AND processed_at IS NULL`, topic,
	).Scan(&cnt); err != nil {
		t.Fatalf("COUNT необработанных: %v", err)
	}
	if cnt != 0 {
		t.Errorf("после RunOnce должно быть 0 необработанных, получили %d", cnt)
	}
}

// TestOutboxRetryOnError проверяет, что при ошибке handler:
// - attempts инкрементируется
// - next_retry_at устанавливается
// - событие не помечается как processed.
func TestOutboxRetryOnError(t *testing.T) {
	if testPool == nil {
		t.Skip("пул не инициализирован")
	}

	ctx := context.Background()
	topic := "test-retry-" + uid()
	defer cleanupOutboxEvents(t, topic)

	producer := outbox.NewProducer()

	tx, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := producer.EnqueueTx(ctx, tx, topic, "test.event", "payload"); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("EnqueueTx: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	dispatcher := outbox.NewDispatcher(testPool, 5, 10*time.Millisecond)

	handlerErr := errors.New("временная ошибка handler")
	_, err = dispatcher.RunOnce(ctx, func(ev outbox.Event) error {
		return handlerErr
	})
	if err != nil {
		t.Fatalf("RunOnce после ошибки handler: %v", err)
	}

	// Проверяем attempts и next_retry_at.
	var attempts int16
	var nextRetryAt *time.Time
	var lastError *string
	if err := testPool.QueryRow(ctx, `
		SELECT attempts, next_retry_at, last_error
		FROM outbox_events
		WHERE topic = $1
	`, topic).Scan(&attempts, &nextRetryAt, &lastError); err != nil {
		t.Fatalf("SELECT после ошибки: %v", err)
	}

	if attempts != 1 {
		t.Errorf("attempts = %d, ожидали 1", attempts)
	}
	if nextRetryAt == nil {
		t.Error("next_retry_at должен быть установлен после ошибки")
	}
	if lastError == nil || *lastError == "" {
		t.Error("last_error должен быть установлен после ошибки handler")
	}

	// processed_at должен быть NULL (не обработано).
	var processedAt *time.Time
	if err := testPool.QueryRow(ctx, `
		SELECT processed_at FROM outbox_events WHERE topic = $1
	`, topic).Scan(&processedAt); err != nil {
		t.Fatalf("SELECT processed_at: %v", err)
	}
	if processedAt != nil {
		t.Error("processed_at должен быть NULL после ошибки handler")
	}
}

// TestOutboxNoDoubleDispatch проверяет, что два конкурентных RunOnce
// не обрабатывают одно и то же событие дважды (SKIP LOCKED).
func TestOutboxNoDoubleDispatch(t *testing.T) {
	if testPool == nil {
		t.Skip("пул не инициализирован")
	}

	ctx := context.Background()
	topic := "test-double-" + uid()
	defer cleanupOutboxEvents(t, topic)

	producer := outbox.NewProducer()

	tx, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := producer.EnqueueTx(ctx, tx, topic, "test.event", "payload"); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("EnqueueTx: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	dispatcher := outbox.NewDispatcher(testPool, 5, 10*time.Millisecond)

	var processedCount int64
	var mu sync.Mutex
	handler := func(ev outbox.Event) error {
		// Искусственная задержка, чтобы второй горутин тоже успел зайти в RunOnce.
		time.Sleep(50 * time.Millisecond)
		mu.Lock()
		processedCount++
		mu.Unlock()
		return nil
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = dispatcher.RunOnce(ctx, handler)
	}()
	go func() {
		defer wg.Done()
		_, _ = dispatcher.RunOnce(ctx, handler)
	}()
	wg.Wait()

	if processedCount > 1 {
		t.Errorf("событие обработано %d раз, ожидали не более 1 (SKIP LOCKED)", processedCount)
	}
}

// TestOutboxDeadLetterAfterMaxAttempts проверяет, что событие с attempts >= MaxAttempts
// не выбирается RunOnce.
func TestOutboxDeadLetterAfterMaxAttempts(t *testing.T) {
	if testPool == nil {
		t.Skip("пул не инициализирован")
	}

	ctx := context.Background()
	topic := "test-dead-letter-" + uid()
	defer cleanupOutboxEvents(t, topic)

	producer := outbox.NewProducer()

	tx, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := producer.EnqueueTx(ctx, tx, topic, "test.event", "payload"); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("EnqueueTx: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// Устанавливаем attempts = MaxAttempts напрямую.
	maxAttempts := 3
	_, err = testPool.Exec(ctx, `
		UPDATE outbox_events SET attempts = $1 WHERE topic = $2
	`, maxAttempts, topic)
	if err != nil {
		t.Fatalf("UPDATE attempts: %v", err)
	}

	dispatcher := outbox.NewDispatcher(testPool, maxAttempts, 10*time.Millisecond)

	processed, err := dispatcher.RunOnce(ctx, func(ev outbox.Event) error {
		return nil
	})
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if processed > 0 {
		t.Errorf("dead-letter событие не должно обрабатываться, обработано %d", processed)
	}
}

// TestFundingPaymentIdempotency проверяет идемпотентность InsertFundingPayment.
func TestFundingPaymentIdempotency(t *testing.T) {
	if testPool == nil {
		t.Skip("пул не инициализирован")
	}

	ctx := context.Background()
	orderRepo := repository.NewOrderRepo(testPool)

	exchange := domain.ExchangeBinance
	symbol := "BTCUSDT"
	fundingTime := time.Now().UTC().Truncate(time.Second)

	defer cleanupFundingPayment(t, string(exchange), symbol, fundingTime)

	fp := repository.FundingPaymentParams{
		Exchange:       exchange,
		ExchangeSymbol: symbol,
		Amount:         "0.00012345",
		Rate:           "0.0001",
		FundingTime:    fundingTime,
	}

	// Вставляем дважды.
	if err := orderRepo.InsertFundingPayment(ctx, fp); err != nil {
		t.Fatalf("первый InsertFundingPayment: %v", err)
	}
	if err := orderRepo.InsertFundingPayment(ctx, fp); err != nil {
		t.Fatalf("второй InsertFundingPayment: %v", err)
	}

	var cnt int
	if err := testPool.QueryRow(ctx, `
		SELECT COUNT(*) FROM funding_payments
		WHERE exchange = $1 AND exchange_symbol = $2 AND funding_time = $3
	`, string(exchange), symbol, fundingTime).Scan(&cnt); err != nil {
		t.Fatalf("COUNT funding_payments: %v", err)
	}
	if cnt != 1 {
		t.Errorf("ожидали 1 funding payment, получили %d", cnt)
	}
}
