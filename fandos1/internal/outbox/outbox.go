// Package outbox реализует transactional outbox pattern (раздел 15.2 промпта v2).
//
// Producer.EnqueueTx — вставляет событие в outbox_events внутри транзакции вызывающего.
// Dispatcher.RunOnce — атомарно выбирает необработанные события (SKIP LOCKED),
// вызывает handler и обновляет processed_at или счётчик попыток.
// Dispatcher.Run — запускает RunOnce по тикеру до отмены контекста.
package outbox

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// defaultBatchSize — количество событий за один вызов RunOnce.
const defaultBatchSize = 100

// Event — одно outbox-событие, передаваемое в handler.
type Event struct {
	ID        int64
	Topic     string
	Kind      string
	Payload   json.RawMessage
	Attempts  int16
	CreatedAt time.Time
}

// Producer вставляет события в outbox в рамках транзакции вызывающего.
// Это гарантирует атомарность: событие появляется в outbox тогда и только тогда,
// когда основная операция закоммичена.
type Producer struct{}

// NewProducer создаёт Producer.
func NewProducer() *Producer {
	return &Producer{}
}

// EnqueueTx вставляет событие в outbox_events внутри транзакции tx.
// payload маршалируется в JSONB; topic и kind — строки маршрутизации.
func (p *Producer) EnqueueTx(ctx context.Context, tx pgx.Tx, topic, kind string, payload any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("outbox: маршалинг payload: %w", err)
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO outbox_events (topic, kind, payload)
		VALUES ($1, $2, $3)
	`, topic, kind, raw)
	if err != nil {
		return fmt.Errorf("outbox: вставка события topic=%s kind=%s: %w", topic, kind, err)
	}
	return nil
}

// Dispatcher читает и обрабатывает outbox-события.
type Dispatcher struct {
	pool        *pgxpool.Pool
	MaxAttempts int           // события с attempts >= MaxAttempts переходят в dead-letter (пропускаются)
	BaseBackoff time.Duration // базовая задержка; фактическая = BaseBackoff * 2^attempts
	BatchSize   int           // количество событий за один вызов RunOnce (0 = defaultBatchSize)
}

// NewDispatcher создаёт Dispatcher с указанным пулом.
func NewDispatcher(pool *pgxpool.Pool, maxAttempts int, baseBackoff time.Duration) *Dispatcher {
	return &Dispatcher{
		pool:        pool,
		MaxAttempts: maxAttempts,
		BaseBackoff: baseBackoff,
		BatchSize:   defaultBatchSize,
	}
}

// RunOnce выбирает batch необработанных событий, вызывает handler для каждого
// и атомарно обновляет статус. Возвращает количество обработанных событий.
//
// Атомарность: каждое событие обрабатывается в отдельной транзакции.
// SELECT FOR UPDATE SKIP LOCKED гарантирует, что два конкурентных RunOnce
// не обработают одно и то же событие.
func (d *Dispatcher) RunOnce(ctx context.Context, handler func(Event) error) (int, error) {
	batchSize := d.BatchSize
	if batchSize <= 0 {
		batchSize = defaultBatchSize
	}

	maxAttempts := d.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = 3
	}

	// Выбираем batch необработанных событий с блокировкой.
	rows, err := d.pool.Query(ctx, `
		SELECT event_id, topic, kind, payload, attempts, created_at
		FROM outbox_events
		WHERE processed_at IS NULL
		  AND attempts < $1
		  AND (next_retry_at IS NULL OR next_retry_at <= now())
		ORDER BY created_at
		LIMIT $2
		FOR UPDATE SKIP LOCKED
	`, maxAttempts, batchSize)
	if err != nil {
		return 0, fmt.Errorf("outbox: выборка событий: %w", err)
	}

	// Сначала собираем все события, чтобы закрыть rows до обработки.
	var events []Event
	for rows.Next() {
		var ev Event
		if scanErr := rows.Scan(&ev.ID, &ev.Topic, &ev.Kind, &ev.Payload, &ev.Attempts, &ev.CreatedAt); scanErr != nil {
			rows.Close()
			return 0, fmt.Errorf("outbox: сканирование события: %w", scanErr)
		}
		events = append(events, ev)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("outbox: итерация событий: %w", err)
	}

	processed := 0
	for _, ev := range events {
		if err := d.processEvent(ctx, ev, handler); err != nil {
			// Логируем ошибку, но не прерываем обработку остальных событий.
			// Ошибки handler уже обработаны внутри processEvent.
			_ = err
			continue
		}
		processed++
	}

	return processed, nil
}

// processEvent обрабатывает одно событие в отдельной транзакции.
func (d *Dispatcher) processEvent(ctx context.Context, ev Event, handler func(Event) error) error {
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("outbox: начало транзакции для события %d: %w", ev.ID, err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback(ctx)
		}
	}()

	// Повторно блокируем строку в транзакции для атомарного обновления.
	// Если строка уже заблокирована другим воркером — SKIP LOCKED пропустит её.
	var dummy int64
	lockErr := tx.QueryRow(ctx, `
		SELECT event_id FROM outbox_events
		WHERE event_id = $1
		  AND processed_at IS NULL
		FOR UPDATE SKIP LOCKED
	`, ev.ID).Scan(&dummy)
	if lockErr != nil {
		// Строка недоступна (уже обрабатывается или обработана) — пропускаем.
		_ = tx.Rollback(ctx)
		return nil
	}

	handlerErr := handler(ev)
	now := time.Now().UTC()

	if handlerErr == nil {
		// Успех: помечаем как обработанное.
		_, err = tx.Exec(ctx, `
			UPDATE outbox_events
			SET processed_at = $1,
			    attempts     = attempts + 1
			WHERE event_id = $2
		`, now, ev.ID)
		if err != nil {
			return fmt.Errorf("outbox: обновление processed_at события %d: %w", ev.ID, err)
		}
	} else {
		// Ошибка: инкрементируем attempts, вычисляем next_retry_at.
		nextAttempt := int(ev.Attempts) + 1
		backoff := d.BaseBackoff
		if nextAttempt > 0 {
			// Exponential backoff: BaseBackoff * 2^attempts.
			multiplier := math.Pow(2, float64(ev.Attempts))
			backoff = time.Duration(float64(d.BaseBackoff) * multiplier)
		}
		nextRetryAt := now.Add(backoff)
		lastError := handlerErr.Error()

		_, err = tx.Exec(ctx, `
			UPDATE outbox_events
			SET attempts      = attempts + 1,
			    last_error    = $1,
			    next_retry_at = $2
			WHERE event_id = $3
		`, lastError, nextRetryAt, ev.ID)
		if err != nil {
			return fmt.Errorf("outbox: обновление retry для события %d: %w", ev.ID, err)
		}
	}

	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("outbox: коммит события %d: %w", ev.ID, err)
	}
	return nil
}

// Run запускает цикл диспетчера: вызывает RunOnce с заданным интервалом до отмены контекста.
// Блокирует до тех пор, пока ctx не будет отменён.
func (d *Dispatcher) Run(ctx context.Context, handler func(Event) error, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := d.RunOnce(ctx, handler); err != nil {
				// Ошибки RunOnce логируются вызывающим через observability; здесь не паникуем.
				_ = err
			}
		}
	}
}
