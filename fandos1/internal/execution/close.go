// close.go — координированное закрытие парной позиции (раздел 11.3 промпта v2).
//
// ГЛАВНЫЙ ПРИНЦИП: НЕ использовать независимые TP/SL на двух биржах как основной механизм
// выхода. Они могут закрыть один leg раньше второго и создать направленную экспозицию.
//
// Алгоритм coordinated close:
//  1. Перевести позицию в EXIT_REQUESTED, заблокировать новые slices.
//  2. Отменить все entry/pending/несовместимые conditional orders.
//  3. Считать актуальные позиции обеих ног.
//  4. Вычислить общий base quantity, который можно закрыть синхронно.
//  5. Одновременно отправить reduce-only marketable IOC на обеих биржах.
//  6. long leg: limit = bestBid - CloseProtectionTicks × tickSize.
//     short leg: limit = bestAsk + CloseProtectionTicks × tickSize.
//  7. Дождаться executions (не только ack).
//  8. При частичном исполнении выровнять legs до минимального общего остатка.
//  9. Повторять закрытие оставшейся части с bounded числом requotes.
// 10. При критическом риске применять UltimateEmergencyClosePolicy.
// 11. После закрытия — REST+WS reconciliation, отмена остаточных ордеров.
package execution

import (
	"context"
	"errors"
	"sync"

	"github.com/thecd/fundarbitrage/internal/decimal"
	"github.com/thecd/fundarbitrage/internal/domain"
	"github.com/thecd/fundarbitrage/internal/exchange"
)

// CloseConfig — параметры закрытия (раздел 5.3, 11.3).
type CloseConfig struct {
	CloseProtectionTicks int            // сколько тиков уступать от best price
	MaxRequotes          int            // bounded число повторных попыток
	UltimatePolicy       string         // STRICT_PRICE_GUARD / ESCALATING / EMERGENCY_MARKET
	TickSize             decimal.Decimal
	OrderAckTimeout      // поля заполняются вызывающим
}

// OrderAckTimeout — анти-поле для избежания конфликтов; оставляем как часть конфига.
type OrderAckTimeout struct{}

// CloseRequest — параметры одной операции закрытия.
type CloseRequest struct {
	PositionID domain.PositionID
	// Long leg.
	LongSymbol    domain.ExchangeSymbol
	LongRemaining decimal.Decimal // сколько ещё закрыть (reduce)
	LongExecutor  *OrderExecutor
	// Short leg.
	ShortSymbol    domain.ExchangeSymbol
	ShortRemaining decimal.Decimal // сколько выкупить
	ShortExecutor  *OrderExecutor
	// Book providers для получения best prices.
	LongBookProvider  BookProvider
	ShortBookProvider BookProvider
}

// BookProvider — абстракция для получения best bid/ask на момент закрытия.
// В real impl это marketdata.Cache; в тестах — stub.
type BookProvider interface {
	BestBid(symbol domain.ExchangeSymbol) (decimal.Decimal, bool)
	BestAsk(symbol domain.ExchangeSymbol) (decimal.Decimal, bool)
}

// CloseResult — исход coordinated close.
type CloseResult struct {
	LongClosedQty  decimal.Decimal
	ShortClosedQty decimal.Decimal
	Degraded       bool   // true, если не удалось закрыть синхронно
	Reason         string // объяснение при Degraded
}

// ErrCloseIncomplete — закрыто не полностью, нужна эскалация.
var ErrCloseIncomplete = errors.New("execution: coordinated close incomplete")

// CoordinatedClose выполняет синхронное закрытие обеих ног (раздел 11.3).
// Возвращает ошибку, если закрыть не удалось; result.Degraded=true при частичном успехе.
func CoordinatedClose(ctx context.Context, req CloseRequest, cfg CloseConfig) (CloseResult, error) {
	result := CloseResult{}

	// Общий объём, который можно закрыть синхронно = min(longRemaining, shortRemaining).
	common := req.LongRemaining
	if req.ShortRemaining.LessThan(common) {
		common = req.ShortRemaining
	}
	if !common.IsPositive() {
		return result, nil // нечего закрывать
	}

	for attempt := 0; attempt <= cfg.MaxRequotes; attempt++ {
		if !common.IsPositive() {
			break
		}
		// Получаем актуальные best prices.
		longBid, ok := req.LongBookProvider.BestBid(req.LongSymbol)
		if !ok {
			result.Degraded = true
			result.Reason = "no long bid for close price"
			return result, ErrCloseIncomplete
		}
		shortAsk, ok := req.ShortBookProvider.BestAsk(req.ShortSymbol)
		if !ok {
			result.Degraded = true
			result.Reason = "no short ask for close price"
			return result, ErrCloseIncomplete
		}

		// limit prices с protection (раздел 11.3 п.6, п.7).
		protection := cfg.TickSize.MulInt(int64(cfg.CloseProtectionTicks))
		longLimit := longBid.Sub(protection)   // продаём long чуть дешевле bid
		shortLimit := shortAsk.Add(protection) // выкупаем short чуть дороже ask

		// Формируем clientOrderID для этой попытки закрытия.
		longID := Format(ClientOrderIDParts{
			PositionID: req.PositionID, LegSide: domain.SideLong,
			SliceIndex: attempt, Nonce: 0, Purpose: "EXIT",
		})
		shortID := Format(ClientOrderIDParts{
			PositionID: req.PositionID, LegSide: domain.SideShort,
			SliceIndex: attempt, Nonce: 0, Purpose: "EXIT",
		})

		// ОДНОВРЕМЕННАЯ отправка обеих ног (раздел 11.3 п.5).
		var longRes, shortRes PlaceOrderResult
		var longErr, shortErr error
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			longRes, longErr = req.LongExecutor.Place(ctx, domain.PlaceOrderRequest{
				ClientOrderID: longID,
				Symbol:        req.LongSymbol,
				Side:          domain.SideShort, // закрытие long = продажа
				OrderMode:     domain.OrderMarketableLimitIOC,
				BaseQty:       common,
				Price:         longLimit,
				ReduceOnly:    true,
				TimeInForce:   domain.TIFIOC,
			})
		}()
		go func() {
			defer wg.Done()
			shortRes, shortErr = req.ShortExecutor.Place(ctx, domain.PlaceOrderRequest{
				ClientOrderID: shortID,
				Symbol:        req.ShortSymbol,
				Side:          domain.SideLong, // закрытие short = выкуп
				OrderMode:     domain.OrderMarketableLimitIOC,
				BaseQty:       common,
				Price:         shortLimit,
				ReduceOnly:    true,
				TimeInForce:   domain.TIFIOC,
			})
		}()
		wg.Wait()

		// Ошибки обеих ног — критично.
		if longErr != nil && shortErr != nil {
			result.Degraded = true
			result.Reason = "both legs failed during close"
			return result, ErrCloseIncomplete
		}

		// Считаем фактически закрытое.
		longFilled := longRes.Order.FilledQty
		shortFilled := shortRes.Order.FilledQty
		if longErr != nil {
			longFilled = decimal.Zero
		}
		if shortErr != nil {
			shortFilled = decimal.Zero
		}

		// Выравниваем: общий закрытый объём = min(longFilled, shortFilled) (раздел 11.3 п.8).
		closedThisAttempt := longFilled
		if shortFilled.LessThan(closedThisAttempt) {
			closedThisAttempt = shortFilled
		}

		result.LongClosedQty = result.LongClosedQty.Add(closedThisAttempt)
		result.ShortClosedQty = result.ShortClosedQty.Add(closedThisAttempt)
		common = common.Sub(closedThisAttempt)

		// Если обе ноги заполнились полностью на эту попытку — done.
		if common.IsPositive() && attempt == cfg.MaxRequotes {
			// Исчерпали requotes, но что-то осталось.
			result.Degraded = true
			result.Reason = "max requotes exhausted with residual"
			return result, ErrCloseIncomplete
		}
	}

	return result, nil
}

// CloseOneLegEmergency — экстренное закрытие одной ноги market-ордером (раздел 11.3 п.10,
// UltimateEmergencyClosePolicy = EMERGENCY_MARKET_CLOSE).
// Используется ТОЛЬКО при критическом риске, когда координированное закрытие не успевает.
func CloseOneLegEmergency(ctx context.Context, exec *OrderExecutor, symbol domain.ExchangeSymbol,
	qty decimal.Decimal, side domain.Side, positionID domain.PositionID) error {
	id := Format(ClientOrderIDParts{
		PositionID: positionID, LegSide: side, SliceIndex: 0, Nonce: 0, Purpose: "EMERGENCY",
	})
	_, err := exec.Place(ctx, domain.PlaceOrderRequest{
		ClientOrderID: id,
		Symbol:        symbol,
		Side:          side,
		OrderMode:     domain.OrderMarket,
		BaseQty:       qty,
		Price:         decimal.Zero, // market
		ReduceOnly:    true,
		TimeInForce:   domain.TIFIOC,
	})
	if err != nil && !errors.Is(err, exchange.ErrOrderNotFound) {
		return err
	}
	return nil
}
