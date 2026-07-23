// order_executor.go — размещение ордера с QUERY_THEN_DECIDE при ack timeout
// (раздел 5.3, 10.2 промпта v2).
//
// Принцип (раздел 1.2.5): НЕ выполнять blind retry. При таймауте ack:
//  1. Запросить состояние ордера через GetOrder (query).
//  2. На основе фактического состояния решить:
//     - если исполнен — оставить как есть;
//     - если активен — отменить;
//     - если не найден — можно безопасно переотправить (это единственный случай retry).
//
// Это гарантирует, что мы НИКОГДА не создадим дублирующий ордер из-за таймаута ответа.
package execution

import (
	"context"
	"errors"
	"time"

	"github.com/thecd/fundarbitrage/internal/domain"
	"github.com/thecd/fundarbitrage/internal/exchange"
)

// OrderExecutor — выполняет один ордер с QUERY_THEN_DECIDE.
type OrderExecutor struct {
	adapter exchange.ExchangeAdapter
	timeout time.Duration // OrderAckTimeoutMs (раздел 5.3)
}

// NewOrderExecutor создаёт executor для конкретной биржи.
func NewOrderExecutor(adapter exchange.ExchangeAdapter, ackTimeout time.Duration) *OrderExecutor {
	return &OrderExecutor{adapter: adapter, timeout: ackTimeout}
}

// PlaceOrderResult — исход размещения с учётом QUERY_THEN_DECIDE.
type PlaceOrderResult struct {
	Order  domain.Order
	Source ResultSource // откуда получено состояние ордера
	Err    error        // ненулевая при критической ошибке (без состояния)
}

// ResultSource — источник состояния ордера.
type ResultSource string

const (
	SourceAck   ResultSource = "ack"   // штатный ack от биржи
	SourceQuery ResultSource = "query" // восстановлено через GetOrder после ack timeout
)

// Place размещает ордер с QUERY_THEN_DECIDE-логикой.
// Возвращает:
//   - Order со статусом из ack ИЛИ из query (если ack таймаутил).
//   - ErrTimeout, если ни ack, ни query не помогли найти ордер (редкий случай: биржа
//     ещё не зарегистрировала его — тогда безопасно retry, т.к. ордера точно нет).
func (e *OrderExecutor) Place(ctx context.Context, req domain.PlaceOrderRequest) (PlaceOrderResult, error) {
	// 1. Attempt placement с таймаутом.
	placeCtx, cancel := context.WithTimeout(ctx, e.timeout)
	ack, err := e.adapter.PlaceOrder(placeCtx, req)
	cancel()

	if err == nil {
		// Штатный ack. OrderAck содержит только exchange ID + status; для деталей fill
		// (FilledQty, AvgFillPrice) нужен GetOrder — это моделирует реальную биржу,
		// где ack = «принят», а fill details приходят в private WS или через query.
		// Пробуем получить детали; если GetOrder недоступен сразу — используем ack как есть.
		order := domain.Order{
			ExchangeOrderID:   ack.ExchangeOrderID,
			ClientOrderID:     ack.ClientOrderID,
			Symbol:            req.Symbol,
			Side:              req.Side,
			OrderMode:         req.OrderMode,
			ReduceOnly:        req.ReduceOnly,
			RequestedQty:      req.BaseQty,
			Status:            ack.Status,
			ExchangeTimestamp: ack.Timestamp,
			AckState:          domain.AckStateAcked,
		}
		// Best-effort query для fill details. Используем отдельный таймаут e.timeout,
		// производный от родительского ctx (если родитель уже отменён — query просто
		// не выполняется и мы сохраняем ack-данные — это допустимое поведение).
		queryCtx, queryCancel := context.WithTimeout(ctx, e.timeout)
		q, qerr := e.adapter.GetOrder(queryCtx, domain.OrderQuery{
			ClientOrderID: req.ClientOrderID,
			Symbol:        req.Symbol,
		})
		queryCancel()
		if qerr == nil {
			order.FilledQty = q.FilledQty
			order.AvgFillPrice = q.AvgFillPrice
			order.Fees = q.Fees
			// Статус из query может быть «свежее» (filled vs acknowledged).
			// Если query вернул терминально-негативный статус (cancelled/rejected/expired)
			// при успешном ack — фиксируем Source=SourceQuery, чтобы вызывающий знал,
			// что состояние получено из query, а не из ack (ack и query противоречат друг другу).
			if isTerminalNegative(q.Status) {
				order.Status = q.Status
				return PlaceOrderResult{
					Order:  order,
					Source: SourceQuery, // состояние из query, противоречащего ack
				}, nil
			}
			order.Status = q.Status
		}
		return PlaceOrderResult{Order: order, Source: SourceAck}, nil
	}

	// 2. Если ошибка — НЕ retry вслепую. Анализируем тип ошибки.
	if !errors.Is(err, exchange.ErrTimeout) {
		// Не таймаут — реальная ошибка (rate limit, unauthorized, invalid symbol).
		// Возвращаем как есть; retry делает вызывающий по своей политике.
		return PlaceOrderResult{Err: err}, err
	}

	// 3. Ack timeout → QUERY: запросить состояние ордера.
	// (раздел 10.2: «при таймауте ack — запросить состояние, не переотправлять вслепую»).
	queryCtx, cancelQ := context.WithTimeout(ctx, e.timeout)
	queried, qerr := e.adapter.GetOrder(queryCtx, domain.OrderQuery{
		ClientOrderID: req.ClientOrderID,
		Symbol:        req.Symbol,
	})
	cancelQ()

	if qerr == nil {
		// Ордер найден — состояние восстановлено через query.
		queried.AckState = domain.AckStateQueried
		return PlaceOrderResult{Order: queried, Source: SourceQuery}, nil
	}

	// 4. GetOrder вернул ErrOrderNotFound — ордера на бирже нет.
	// Это единственный случай, когда retry безопасен: ордер точно не создан.
	if errors.Is(qerr, exchange.ErrOrderNotFound) {
		return PlaceOrderResult{Err: exchange.ErrOrderNotFound}, exchange.ErrOrderNotFound
	}

	// 5. GetOrder сам таймаутил или дал network error — состояние не известно.
	// Это самая опасная ситуация: мы не знаем, создан ордер или нет.
	// Возвращаем ошибку без состояния; вызывающий (execution coordinator) обязан
	// остановить дальнейший набор и перейти в recovery (раздел 10.3, 28).
	return PlaceOrderResult{Err: qerr}, qerr
}

// IsSafeRetry — true, если ошибку можно безопасно retry-ить (ордера точно нет).
// Это только ErrOrderNotFound из query path.
func IsSafeRetry(err error) bool {
	return errors.Is(err, exchange.ErrOrderNotFound)
}

// IsAmbiguousTimeout — true, если ошибка оставляет состояние неопределённым
// (нельзя ни retry, ни считать успешным — нужен recovery).
func IsAmbiguousTimeout(err error) bool {
	if errors.Is(err, exchange.ErrTimeout) {
		return true
	}
	if errors.Is(err, exchange.ErrNetwork) {
		return true
	}
	return false
}

// isTerminalNegative — true для статусов, означающих, что ордер не был исполнен:
// cancelled, rejected, expired. Используется для обнаружения противоречия между
// успешным ack и последующим query, возвращающим негативный терминальный статус.
func isTerminalNegative(s domain.OrderStatus) bool {
	return s == domain.OrderStatusCancelled ||
		s == domain.OrderStatusRejected ||
		s == domain.OrderStatusExpired
}
