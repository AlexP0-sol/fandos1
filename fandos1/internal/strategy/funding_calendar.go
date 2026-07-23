// Package strategy реализует торговую логику стратегии funding-арбитража:
// funding calendar (раздел 8.3), ExpectedNetPnL (3.4), candidate scoring (8.4).
//
// Это «мозг» стратегии: берёт рыночные данные и настройки, возвращает eligible кандидатов
// с полным расчётом ожидаемой чистой доходности и risk score. Сама стратегия НЕ размещает
// ордера — это делает execution-coordinator (Этап 7) по immutable execution plan.
package strategy

import (
	"sort"
	"time"

	"github.com/thecd/fundarbitrage/internal/decimal"
	"github.com/thecd/fundarbitrage/internal/domain"
)

// ============================================================
// FundingEvent (раздел 8.3)
// ============================================================

// FundingEvent — одно ожидаемое (или свершившееся) начисление funding на одной ноге.
// Calendar строит список таких событий на горизонте удержания.
type FundingEvent struct {
	Exchange    domain.ExchangeID
	Symbol      domain.ExchangeSymbol
	LegSide     domain.Side
	ScheduledAt time.Time

	// FundingRate — predicted rate на момент построения календаря (раздел 3.2).
	// После свершения события обновляется на realized (RateType=REALIZED).
	FundingRate decimal.Decimal
	RateType    domain.FundingRateType

	// EstimatedNotional — объём, на который начисляется funding (qty × fundingPrice, раздел 3.2).
	EstimatedNotional decimal.Decimal
	// EstimatedCashFlow — предсказанный платёж со знаком:
	// для long: -rate × notional (при rate>0 long платит); для short: +rate × notional.
	// Это ExpectedFundingCashFlow одного события (раздел 3.2).
	EstimatedCashFlow decimal.Decimal

	// Confidence — уверенность в predicted rate (раздел 3.2).
	// Тем выше, чем ближе событие и стабильнее ставка.
	Confidence domain.ConfidenceLevel
}

// CalendarInput — входные данные для построения funding calendar одной ноги.
type CalendarInput struct {
	Exchange        domain.ExchangeID
	Symbol          domain.ExchangeSymbol
	Side            domain.Side
	PredictedRate   decimal.Decimal // текущий predicted rate (на ближайшее событие)
	FundingInterval time.Duration   // интервал начисления (1h, 4h, 8h, ...)
	NextFundingTime time.Time       // ближайшее запланированное событие
	Horizon         time.Duration   // горизонт удержания (до скольких событий строить календарь)
	Confidence      domain.ConfidenceLevel
	Notional        decimal.Decimal // объём ноги для оценки cash flow
}

// BuildFundingCalendar строит список ожидаемых funding events в пределах горизонта
// удержания (раздел 8.3). Не extrapolирует ставку на будущие события: каждое событие
// получает ту же predicted rate, что и ближайшее, но Confidence деградирует с дистанцией
// (чем дальше событие, тем менее предсказуема ставка).
//
// События упорядочены по ScheduledAt. Возвращает пустой слайс, если NextFundingTime в прошлом
// или Horizon ≤ 0.
func BuildFundingCalendar(in CalendarInput, now time.Time) []FundingEvent {
	if in.Horizon <= 0 || in.FundingInterval <= 0 {
		return nil
	}
	if !in.NextFundingTime.After(now) {
		// Ближайшее событие уже прошло — данные устарели; календарь не строим.
		return nil
	}
	horizonEnd := now.Add(in.Horizon)

	var events []FundingEvent
	t := in.NextFundingTime
	stepIndex := 0
	for !t.After(horizonEnd) {
		ev := FundingEvent{
			Exchange:          in.Exchange,
			Symbol:            in.Symbol,
			LegSide:           in.Side,
			ScheduledAt:       t,
			FundingRate:       in.PredictedRate,
			RateType:          domain.FundingRatePredicted,
			EstimatedNotional: in.Notional,
			Confidence:        degradeConfidence(in.Confidence, stepIndex),
		}
		ev.EstimatedCashFlow = fundingCashFlow(in.Side, in.PredictedRate, in.Notional)
		events = append(events, ev)
		t = t.Add(in.FundingInterval)
		stepIndex++
	}
	return events
}

// fundingCashFlow — формула FundingCashFlow из раздела 3.2:
//
//	FundingCashFlow = -sideSign × fundingRate × fundingNotional
//
// sideSign(long)=+1, sideSign(short)=-1 (domain.Side.Sign).
// При fundingRate > 0 (long платит short): long получает отрицательный поток, short — положительный.
func fundingCashFlow(side domain.Side, rate, notional decimal.Decimal) decimal.Decimal {
	return decimal.FromInt(int64(-side.Sign())).Mul(rate).Mul(notional)
}

// degradeConfidence снижает уровень уверенности с удалением от ближайшего события
// (раздел 3.2: ConfidenceLevel зависит от дистанции до события).
// stepIndex=0 (ближайшее) → без изменений; 1 → на шаг ниже; и т.д., не ниже ConfidenceNone.
func degradeConfidence(base domain.ConfidenceLevel, stepIndex int) domain.ConfidenceLevel {
	c := base
	for i := 0; i < stepIndex; i++ {
		if c <= domain.ConfidenceNone {
			break
		}
		c--
	}
	return c
}

// SumExpectedFundingCashFlow — суммарный предсказанный funding cash flow списка событий (раздел 3.2).
// Используется как ExpectedFundingPnL в формуле ExpectedNetPnL (раздел 3.4).
func SumExpectedFundingCashFlow(events []FundingEvent) decimal.Decimal {
	var sum decimal.Decimal
	for _, ev := range events {
		sum = sum.Add(ev.EstimatedCashFlow)
	}
	return sum
}

// ============================================================
// Сравнение и классификация интервалов (раздел 8.2)
// ============================================================

// IntervalClass — класс пары по режиму funding (раздел 8.2).
type IntervalClass string

const (
	// ClassSameIntervalAligned — одинаковый интервал, время выровнено в пределах skew.
	ClassSameIntervalAligned IntervalClass = "SAME_INTERVAL_ALIGNED"
	// ClassSameIntervalUnaligned — одинаковый интервал, время разъехалось.
	ClassSameIntervalUnaligned IntervalClass = "SAME_INTERVAL_UNALIGNED"
	// ClassDifferentInterval — разные интервалы.
	ClassDifferentInterval IntervalClass = "DIFFERENT_INTERVAL"
)

// ClassifyInterval — классифицирует пару по funding interval (раздел 8.2).
// Классификация ОПИСАТЕЛЬНАЯ: решение «пропускать ли unaligned-пары»
// (настройка RequireAlignedFundingTimes) принимает вызывающий по полученному классу.
// longFundingInterval / shortFundingInterval — длительности интервалов.
// longNext / shortNext — ближайшие события; нулевое время = данные отсутствуют → unaligned.
// maxSkew — допустимый разъезд времени событий (MaxFundingTimeSkewMinutes).
func ClassifyInterval(longFundingInterval, shortFundingInterval time.Duration,
	longNext, shortNext time.Time, maxSkew time.Duration) IntervalClass {
	if longFundingInterval != shortFundingInterval {
		return ClassDifferentInterval
	}
	if longNext.IsZero() || shortNext.IsZero() {
		// Нет данных о времени события — консервативно считаем unaligned.
		return ClassSameIntervalUnaligned
	}
	skew := longNext.Sub(shortNext)
	if skew < 0 {
		skew = -skew
	}
	if skew <= maxSkew {
		return ClassSameIntervalAligned
	}
	return ClassSameIntervalUnaligned
}

// SortByScheduledAt — утилита для UI/логов.
func SortByScheduledAt(events []FundingEvent) {
	sort.Slice(events, func(i, j int) bool {
		return events[i].ScheduledAt.Before(events[j].ScheduledAt)
	})
}
