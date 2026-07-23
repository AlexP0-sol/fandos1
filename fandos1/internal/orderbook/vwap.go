// Package orderbook реализует расчёт VWAP по стакану для целевого объёма (раздел 3.3 промпта v2).
//
// VWAP — средневзвешенная цена исполнения, когда целевой объём «прходит» через уровни стакана.
// В отличие от BBO (best bid/ask), VWAP отражает реальную стоимость входа/выхода с учётом глубины,
// что критично для оценки EntryBasis и slippage (раздел 3.3).
package orderbook

import (
	"github.com/thecd/fundarbitrage/internal/decimal"
	"github.com/thecd/fundarbitrage/internal/domain"
)

// VWAPResult — результат прохода объёма через стакан.
type VWAPResult struct {
	// VWAP — средневзвешенная цена исполненной части. Zero, если ничего не исполнено.
	VWAP decimal.Decimal
	// FilledQty — фактически исполненный объём (может быть меньше запрошенного при тонком стакане).
	FilledQty decimal.Decimal
	// FilledValue — суммарная стоимость исполнения = Σ(qty_i × price_i). Удобно для PnL.
	FilledValue decimal.Decimal
	// FullyFilled — true, если весь запрошенный объём исполнен.
	FullyFilled bool
}

// AbsBestPrice возвращает абсолютное отклонение VWAP от лучшей цены стакана.
// Полезно для оценки slippage: чем больше отклонение, тем хуже цена входа.
func (r VWAPResult) SlippageBps(bestPrice decimal.Decimal) decimal.Decimal {
	if r.VWAP.IsZero() || bestPrice.IsZero() {
		return decimal.Zero
	}
	// |VWAP - best| / best × 10000
	diff := r.VWAP.Sub(bestPrice).Abs()
	return diff.Div(bestPrice).MulInt(10000)
}

// VWAPBuy — стоимость покупки заданного объёма через asks (возрастающие цены).
// Сторона long: берём asks от лучшего (минимального) вверх.
//
// Логика: для каждого уровня берём min(доступный объём уровня, остаток запроса),
// накапливаем Σ(qty × price) и Σqty. Если вся глубина исчерпана до заполнения объёма —
// FilledQty < requested, FullyFilled=false (раздел 8.1: insufficient depth → кандидат отбрасывается).
func VWAPBuy(asks []domain.PriceLevel, requestedQty decimal.Decimal) VWAPResult {
	if !requestedQty.IsPositive() || len(asks) == 0 {
		return VWAPResult{}
	}
	return walk(asks, requestedQty)
}

// VWAPSell — стоимость продажи заданного объёма через bids (убывающие цены).
// Сторона short: берём bids от лучшего (максимального) вниз.
func VWAPSell(bids []domain.PriceLevel, requestedQty decimal.Decimal) VWAPResult {
	if requestedQty.IsZero() || requestedQty.IsNegative() || len(bids) == 0 {
		return VWAPResult{}
	}
	return walk(bids, requestedQty)
}

// walk — общий алгоритм прохода по уровням в заданном порядке.
// Уровни уже отсортированы вызывающим (asks по возрастанию, bids по убыванию).
func walk(levels []domain.PriceLevel, requestedQty decimal.Decimal) VWAPResult {
	var (
		filledValue = decimal.Zero
		filledQty   = decimal.Zero
		remaining   = requestedQty
	)
	for _, lvl := range levels {
		if remaining.IsZero() {
			break
		}
		// Сколько можно взять с этого уровня.
		take := lvl.Qty
		if take.GreaterThan(remaining) {
			take = remaining
		}
		filledValue = filledValue.Add(lvl.Price.Mul(take))
		filledQty = filledQty.Add(take)
		remaining = remaining.Sub(take)
	}
	if filledQty.IsZero() {
		return VWAPResult{}
	}
	return VWAPResult{
		VWAP:        filledValue.Div(filledQty),
		FilledQty:   filledQty,
		FilledValue: filledValue,
		FullyFilled: remaining.IsZero(),
	}
}

// EntryBasis — исполнимый спред входа между short bid и long ask (раздел 3.3).
// Для целевого объёма считает VWAP-спред, не BBO.
//
//	EntryBasisVWAP = ShortEntryVWAP / LongEntryVWAP - 1
//
// Положительное значение — short стоит дороже long ( favourable edge для стратегии:
// продаём дорого, покупаем дёшево). Отрицательное — adverse (теряем на входе).
//
// Возвращает (basis, longVWAP, shortVWAP, fullyFilledBoth).
func EntryBasis(longAsks, shortBids []domain.PriceLevel, qty decimal.Decimal) (basis, longVWAP, shortVWAP decimal.Decimal, ok bool) {
	lr := VWAPBuy(longAsks, qty)
	sr := VWAPSell(shortBids, qty)
	if !lr.FullyFilled || !sr.FullyFilled {
		return decimal.Zero, decimal.Zero, decimal.Zero, false
	}
	if lr.VWAP.IsZero() {
		return decimal.Zero, decimal.Zero, decimal.Zero, false
	}
	// shortVWAP/longVWAP - 1
	b := sr.VWAP.Div(lr.VWAP).Sub(decimal.One)
	return b, lr.VWAP, sr.VWAP, true
}

// ExitBasis — исполнимый спред выхода между long bid и short ask (раздел 3.3).
// На выходе: продаём long (через bids биржи long-leg), выкупаем short (через asks биржи short-leg).
//
//	ExitBasisVWAP = LongExitVWAP / ShortExitVWAP - 1
//
// Положительное — выгодно закрыться (продаём long дорого, выкупаем short дёшево).
func ExitBasis(longBids, shortAsks []domain.PriceLevel, qty decimal.Decimal) (basis, longExitVWAP, shortExitVWAP decimal.Decimal, ok bool) {
	lr := VWAPSell(longBids, qty)
	sr := VWAPBuy(shortAsks, qty)
	if !lr.FullyFilled || !sr.FullyFilled {
		return decimal.Zero, decimal.Zero, decimal.Zero, false
	}
	if sr.VWAP.IsZero() {
		return decimal.Zero, decimal.Zero, decimal.Zero, false
	}
	b := lr.VWAP.Div(sr.VWAP).Sub(decimal.One)
	return b, lr.VWAP, sr.VWAP, true
}

// CheckDepthSufficient — упрощённая проверка, что суммарная глубина levels покрывает qty.
// Используется в первичных фильтрах сканера (раздел 8.1) без расчёта VWAP.
func CheckDepthSufficient(levels []domain.PriceLevel, qty decimal.Decimal) bool {
	sum := decimal.Zero
	for _, lvl := range levels {
		sum = sum.Add(lvl.Qty)
		if sum.GreaterThanOrEqual(qty) {
			return true
		}
	}
	return false
}
