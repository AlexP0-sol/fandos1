// close.go — координированное закрытие парной позиции (раздел 11.3 промпта v2).
//
// ГЛАВНЫЙ ПРИНЦИП: НЕ использовать независимые TP/SL на двух биржах как основной механизм
// выхода. Они могут закрыть один leg раньше второго и создать направленную экспозицию.
//
// Алгоритм coordinated close:
//  1. Перевести позицию в EXIT_REQUESTED, заблокировать новые slices (вызывающий).
//  2. Отменить все entry/pending/несовместимые conditional orders (вызывающий).
//  3. Считать актуальные позиции обеих ног (вызывающий; сюда приходят remaining).
//  4. Общая цель закрытия = min(longRemaining, shortRemaining); излишек большей ноги —
//     зона ответственности repair (RepairReduceExcess), не coordinated close.
//  5. Одновременно отправить reduce-only marketable IOC на обеих биржах.
//  6. long leg: limit = bestBid − CloseProtectionTicks × tickSize;
//     short leg: limit = bestAsk + CloseProtectionTicks × tickSize.
//  7. Дождаться executions (не только ack).
//  8. Учёт per-leg: каждая нога ведёт СВОЙ остаток. Недобранная нога досылается
//     на следующей попытке своим объёмом — ноги сходятся к нулю, а не «по минимуму».
//  9. Повторять с bounded числом requotes.
//  10. Неопределённое состояние ноги (ack+query таймаут) → НЕМЕДЛЕННЫЙ выход с
//     ErrCloseAmbiguous: продолжать нельзя, нужна reconciliation (раздел 10.2, 28).
//  11. После закрытия — REST+WS reconciliation, отмена остаточных ордеров (вызывающий).
package execution

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/thecd/fundarbitrage/internal/decimal"
	"github.com/thecd/fundarbitrage/internal/domain"
	"github.com/thecd/fundarbitrage/internal/exchange"
)

// CloseConfig — параметры закрытия (раздел 5.3, 11.3).
type CloseConfig struct {
	CloseProtectionTicks int // сколько тиков уступать от best price
	MaxRequotes          int // bounded число повторных попыток
	TickSize             decimal.Decimal
}

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

// CloseResult — исход coordinated close. Счётчики отражают ФАКТИЧЕСКИ закрытое
// per-leg (не min) — искажение учёта здесь означало бы скрытую направленную экспозицию.
type CloseResult struct {
	LongClosedQty  decimal.Decimal
	ShortClosedQty decimal.Decimal
	// Residual* — незакрытый остаток цели закрытия по каждой ноге.
	// Ненулевой остаток → позиция в DEGRADED/RECONCILING, решает вызывающий.
	ResidualLongQty  decimal.Decimal
	ResidualShortQty decimal.Decimal
	Degraded         bool   // true, если закрыть цель полностью не удалось
	Reason           string // объяснение при Degraded
}

// ErrCloseIncomplete — закрыто не полностью, нужна эскалация (bounded requotes исчерпаны).
var ErrCloseIncomplete = errors.New("execution: coordinated close incomplete")

// ErrCloseAmbiguous — состояние ноги после попытки закрытия НЕИЗВЕСТНО
// (ack и query таймаутили). Продолжать нельзя: возможен незамеченный fill.
// Вызывающий обязан выполнить reconciliation (REST+WS) до любых новых ордеров.
var ErrCloseAmbiguous = errors.New("execution: leg state unknown after close attempt, reconciliation required")

// legAttemptResult — исход одной попытки на одной ноге.
type legAttemptResult struct {
	filled    decimal.Decimal
	ambiguous bool  // состояние неизвестно (timeout/network на place И query)
	err       error // прочие ошибки (reject, rate limit, ...)
}

// CoordinatedClose выполняет синхронное закрытие обеих ног (раздел 11.3).
//
// Цель закрытия = min(LongRemaining, ShortRemaining) на КАЖДОЙ ноге: coordinated close
// уменьшает обе ноги на одинаковую величину, сохраняя дельту; излишек большей ноги
// устраняет repair (RepairReduceExcess) отдельным reduce-only ордером.
//
// Каждая нога ведёт собственный остаток: если long исполнился на 10, а short на 4,
// следующая попытка отправит long 0 (или его остаток) и short 6 — ноги сходятся к нулю.
// Возвращает ErrCloseAmbiguous при неопределённом состоянии (нужна reconciliation),
// ErrCloseIncomplete при исчерпании requotes с остатком.
func CoordinatedClose(ctx context.Context, req CloseRequest, cfg CloseConfig) (CloseResult, error) {
	result := CloseResult{}

	// Цель: одинаковое уменьшение обеих ног.
	target := decimal.Min(req.LongRemaining, req.ShortRemaining)
	if !target.IsPositive() {
		return result, nil // нечего закрывать
	}
	longLeft, shortLeft := target, target

	protection := cfg.TickSize.MulInt(int64(cfg.CloseProtectionTicks))

	for attempt := 0; attempt <= cfg.MaxRequotes; attempt++ {
		if !longLeft.IsPositive() && !shortLeft.IsPositive() {
			break // обе ноги закрыты полностью
		}

		// Цены нужны только для ног с остатком.
		var longLimit, shortLimit decimal.Decimal
		if longLeft.IsPositive() {
			bid, ok := req.LongBookProvider.BestBid(req.LongSymbol)
			if !ok {
				return degraded(result, longLeft, shortLeft, "no long bid for close price"), ErrCloseIncomplete
			}
			longLimit = bid.Sub(protection) // продаём long чуть дешевле bid
		}
		if shortLeft.IsPositive() {
			ask, ok := req.ShortBookProvider.BestAsk(req.ShortSymbol)
			if !ok {
				return degraded(result, longLeft, shortLeft, "no short ask for close price"), ErrCloseIncomplete
			}
			shortLimit = ask.Add(protection) // выкупаем short чуть дороже ask
		}

		// ОДНОВРЕМЕННАЯ отправка обеих ног (раздел 11.3 п.5).
		var longRes, shortRes legAttemptResult
		var wg sync.WaitGroup
		if longLeft.IsPositive() {
			wg.Add(1)
			go func(qty, limit decimal.Decimal, att int) {
				defer wg.Done()
				longRes = placeCloseLeg(ctx, req.LongExecutor, domain.PlaceOrderRequest{
					ClientOrderID: Format(ClientOrderIDParts{
						PositionID: req.PositionID, LegSide: domain.SideLong,
						SliceIndex: att, Nonce: 0, Purpose: "EXIT",
					}),
					Symbol:      req.LongSymbol,
					Side:        domain.SideShort, // закрытие long = продажа
					OrderMode:   domain.OrderMarketableLimitIOC,
					BaseQty:     qty,
					Price:       limit,
					ReduceOnly:  true,
					TimeInForce: domain.TIFIOC,
				}, qty)
			}(longLeft, longLimit, attempt)
		}
		if shortLeft.IsPositive() {
			wg.Add(1)
			go func(qty, limit decimal.Decimal, att int) {
				defer wg.Done()
				shortRes = placeCloseLeg(ctx, req.ShortExecutor, domain.PlaceOrderRequest{
					ClientOrderID: Format(ClientOrderIDParts{
						PositionID: req.PositionID, LegSide: domain.SideShort,
						SliceIndex: att, Nonce: 0, Purpose: "EXIT",
					}),
					Symbol:      req.ShortSymbol,
					Side:        domain.SideLong, // закрытие short = выкуп
					OrderMode:   domain.OrderMarketableLimitIOC,
					BaseQty:     qty,
					Price:       limit,
					ReduceOnly:  true,
					TimeInForce: domain.TIFIOC,
				}, qty)
			}(shortLeft, shortLimit, attempt)
		}
		wg.Wait()

		// Неопределённое состояние ЛЮБОЙ ноги — немедленный стоп: возможен
		// незамеченный fill; счётчики ниже включают только ПОДТВЕРЖДЁННЫЕ fills.
		if longRes.ambiguous || shortRes.ambiguous {
			result.LongClosedQty = result.LongClosedQty.Add(longRes.filled)
			result.ShortClosedQty = result.ShortClosedQty.Add(shortRes.filled)
			leg := "long"
			if shortRes.ambiguous {
				leg = "short"
			}
			if longRes.ambiguous && shortRes.ambiguous {
				leg = "both"
			}
			return degraded(result,
				longLeft.Sub(longRes.filled), shortLeft.Sub(shortRes.filled),
				fmt.Sprintf("%s leg state unknown (ack+query timeout)", leg)), ErrCloseAmbiguous
		}

		// Учёт подтверждённых fills per-leg.
		longLeft = longLeft.Sub(longRes.filled)
		shortLeft = shortLeft.Sub(shortRes.filled)
		result.LongClosedQty = result.LongClosedQty.Add(longRes.filled)
		result.ShortClosedQty = result.ShortClosedQty.Add(shortRes.filled)

		// Обе активные ноги отказали жёсткой ошибкой — прогресса не будет.
		bothActive := longRes.err != nil && shortRes.err != nil
		if bothActive && longRes.filled.IsZero() && shortRes.filled.IsZero() {
			return degraded(result, longLeft, shortLeft, "both legs failed during close"), ErrCloseIncomplete
		}
	}

	if longLeft.IsPositive() || shortLeft.IsPositive() {
		return degraded(result, longLeft, shortLeft, "max requotes exhausted with residual"), ErrCloseIncomplete
	}
	return result, nil
}

// placeCloseLeg выполняет одну попытку закрытия одной ноги и классифицирует исход.
// cap — верхняя граница засчитываемого fill (защита от некорректного оверфилла).
func placeCloseLeg(ctx context.Context, exec *OrderExecutor, req domain.PlaceOrderRequest, capQty decimal.Decimal) legAttemptResult {
	res, err := exec.Place(ctx, req)
	if err == nil {
		filled := res.Order.FilledQty
		if filled.GreaterThan(capQty) {
			filled = capQty
		}
		return legAttemptResult{filled: filled}
	}
	// Ордер точно не создан — безопасно считать fill = 0 и пробовать снова.
	if errors.Is(err, exchange.ErrOrderNotFound) {
		return legAttemptResult{filled: decimal.Zero, err: err}
	}
	// Timeout/network после query — состояние НЕИЗВЕСТНО.
	if IsAmbiguousTimeout(err) {
		return legAttemptResult{filled: decimal.Zero, ambiguous: true, err: err}
	}
	// Прочее (reject, rate limit, unauthorized) — fill = 0, ошибка зафиксирована.
	return legAttemptResult{filled: decimal.Zero, err: err}
}

// degraded заполняет остатки и причину деградации.
func degraded(r CloseResult, longLeft, shortLeft decimal.Decimal, reason string) CloseResult {
	r.ResidualLongQty = decimal.Max(longLeft, decimal.Zero)
	r.ResidualShortQty = decimal.Max(shortLeft, decimal.Zero)
	r.Degraded = true
	r.Reason = reason
	return r
}

// CloseOneLegEmergency — экстренное закрытие одной ноги market-ордером (раздел 11.3 п.10,
// UltimateEmergencyClosePolicy = EMERGENCY_MARKET_CLOSE).
// Используется ТОЛЬКО при критическом риске, когда координированное закрытие не успевает.
func CloseOneLegEmergency(ctx context.Context, exec *OrderExecutor, symbol domain.ExchangeSymbol,
	qty decimal.Decimal, side domain.Side, positionID domain.PositionID) error {
	if !qty.IsPositive() {
		return nil
	}
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
	return err
}
