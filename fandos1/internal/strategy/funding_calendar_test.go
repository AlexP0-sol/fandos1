package strategy

import (
	"testing"
	"time"

	"github.com/thecd/fundarbitrage/internal/decimal"
	"github.com/thecd/fundarbitrage/internal/domain"
)

func mustDur(s string) time.Duration {
	d, err := time.ParseDuration(s)
	if err != nil {
		panic(err)
	}
	return d
}

// TestFundingCashFlowSign — конвенция раздела 3.2:
// rate > 0 → long платит (negative CF), short получает (positive CF).
func TestFundingCashFlowSign(t *testing.T) {
	rate := decimal.MustFromString("0.0001") // 0.01%
	notional := decimal.MustFromString("10000")

	// Long при положительной ставке платит → negative.
	longCF := fundingCashFlow(domain.SideLong, rate, notional)
	if !longCF.IsNegative() {
		t.Errorf("long CF at rate>0 = %s, want negative", longCF.String())
	}
	// -1 × 0.0001 × 10000 = -1
	if !longCF.Equal(decimal.MustFromString("-1")) {
		t.Errorf("long CF = %s, want -1", longCF.String())
	}

	// Short при положительной ставке получает → positive.
	shortCF := fundingCashFlow(domain.SideShort, rate, notional)
	if !shortCF.IsPositive() {
		t.Errorf("short CF at rate>0 = %s, want positive", shortCF.String())
	}
	// +1 × 0.0001 × 10000 = 1
	if !shortCF.Equal(decimal.FromInt(1)) {
		t.Errorf("short CF = %s, want 1", shortCF.String())
	}
}

// TestFundingCashFlowNegativeRate — rate < 0 → long получает, short платит.
func TestFundingCashFlowNegativeRate(t *testing.T) {
	rate := decimal.MustFromString("-0.0002")
	notional := decimal.MustFromString("10000")

	longCF := fundingCashFlow(domain.SideLong, rate, notional)
	if !longCF.IsPositive() {
		t.Errorf("long CF at rate<0 = %s, want positive", longCF.String())
	}
	shortCF := fundingCashFlow(domain.SideShort, rate, notional)
	if !shortCF.IsNegative() {
		t.Errorf("short CF at rate<0 = %s, want negative", shortCF.String())
	}
}

// TestFundingCashFlowZeroRate — нулевая ставка → нулевой поток.
func TestFundingCashFlowZeroRate(t *testing.T) {
	cf := fundingCashFlow(domain.SideLong, decimal.Zero, decimal.MustFromString("1000"))
	if !cf.IsZero() {
		t.Errorf("zero rate CF = %s, want 0", cf.String())
	}
}

// TestBuildCalendarSameInterval — 8h-интервал, горизонт 24h → 3 события.
func TestBuildCalendarSameInterval(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	next := now.Add(2 * time.Hour) // ближайшее через 2h

	in := CalendarInput{
		Exchange:        domain.ExchangeBinance,
		Symbol:          "BTCUSDT",
		Side:            domain.SideShort,
		PredictedRate:   decimal.MustFromString("0.0001"),
		FundingInterval: 8 * time.Hour,
		NextFundingTime: next,
		Horizon:         24 * time.Hour,
		Confidence:      domain.ConfidenceHigh,
		Notional:        decimal.MustFromString("10000"),
	}
	events := BuildFundingCalendar(in, now)
	// Горизонт 24h, первое в +2h, затем +10h, +18h → 3 события (+26h уже вне горизонта).
	if len(events) != 3 {
		t.Fatalf("got %d events, want 3", len(events))
	}
	// Все события с предсказанной ставкой.
	for _, ev := range events {
		if ev.RateType != domain.FundingRatePredicted {
			t.Errorf("event type = %s, want predicted", ev.RateType)
		}
		if !ev.EstimatedCashFlow.IsPositive() {
			t.Errorf("short at positive rate should have positive CF, got %s", ev.EstimatedCashFlow.String())
		}
	}
	// Confidence деградирует по шагам.
	if events[0].Confidence != domain.ConfidenceHigh {
		t.Errorf("event 0 confidence = %s, want high", events[0].Confidence)
	}
	if events[1].Confidence >= events[0].Confidence {
		t.Error("confidence should degrade over steps")
	}
}

// TestBuildCalendarStaleNextFunding — NextFundingTime в прошлом → nil.
func TestBuildCalendarStaleNextFunding(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	in := CalendarInput{
		FundingInterval: 8 * time.Hour,
		NextFundingTime: now.Add(-1 * time.Hour), // в прошлом
		Horizon:         24 * time.Hour,
		Confidence:      domain.ConfidenceHigh,
		Notional:        decimal.FromInt(1000),
	}
	if events := BuildFundingCalendar(in, now); events != nil {
		t.Errorf("stale next funding should give nil, got %d events", len(events))
	}
}

// TestBuildCalendarZeroHorizon — нулевой горизонт → nil.
func TestBuildCalendarZeroHorizon(t *testing.T) {
	now := time.Now()
	in := CalendarInput{
		FundingInterval: 8 * time.Hour,
		NextFundingTime: now.Add(1 * time.Hour),
		Horizon:         0,
		Notional:        decimal.FromInt(1000),
	}
	if events := BuildFundingCalendar(in, now); events != nil {
		t.Errorf("zero horizon should give nil, got %d events", len(events))
	}
}

// TestDegradeConfidence — монотонное убывание до ConfidenceNone.
func TestDegradeConfidence(t *testing.T) {
	if degradeConfidence(domain.ConfidenceHigh, 0) != domain.ConfidenceHigh {
		t.Error("step 0 should keep base")
	}
	if degradeConfidence(domain.ConfidenceHigh, 1) != domain.ConfidenceMedium {
		t.Error("step 1 should drop high → medium")
	}
	if degradeConfidence(domain.ConfidenceHigh, 2) != domain.ConfidenceLow {
		t.Error("step 2 should drop high → low")
	}
	if degradeConfidence(domain.ConfidenceHigh, 3) != domain.ConfidenceNone {
		t.Error("step 3 should drop high → none")
	}
	// Floor на ConfidenceNone.
	if degradeConfidence(domain.ConfidenceHigh, 10) != domain.ConfidenceNone {
		t.Error("floor should be ConfidenceNone")
	}
}

// TestSumExpectedFundingCashFlow — сумма событий парной позиции.
// Long платит (negative), short получает (positive). При равных notional ставки компенсируются,
// но если ставки разные — остаётся net edge.
func TestSumExpectedFundingCashFlow(t *testing.T) {
	events := []FundingEvent{
		{EstimatedCashFlow: decimal.MustFromString("-1")},   // long платит 1
		{EstimatedCashFlow: decimal.MustFromString("3.5")},  // short получает 3.5
	}
	sum := SumExpectedFundingCashFlow(events)
	if !sum.Equal(decimal.MustFromString("2.5")) {
		t.Errorf("sum = %s, want 2.5", sum.String())
	}
}

// TestClassifySameAligned — одинаковый интервал, время выровнено.
func TestClassifySameAligned(t *testing.T) {
	t1 := time.Date(2026, 1, 1, 8, 0, 0, 0, time.UTC)
	t2 := t1.Add(30 * time.Second) // skew 30s
	c := ClassifyInterval(8*time.Hour, 8*time.Hour, t1, t2, true, 1*time.Minute)
	if c != ClassSameIntervalAligned {
		t.Errorf("class = %s, want SAME_INTERVAL_ALIGNED", c)
	}
}

// TestClassifySameUnaligned — одинаковый интервал, время разъехалось больше skew.
func TestClassifySameUnaligned(t *testing.T) {
	t1 := time.Date(2026, 1, 1, 8, 0, 0, 0, time.UTC)
	t2 := t1.Add(10 * time.Minute) // skew 10min > maxSkew 1min
	c := ClassifyInterval(8*time.Hour, 8*time.Hour, t1, t2, true, 1*time.Minute)
	if c != ClassSameIntervalUnaligned {
		t.Errorf("class = %s, want SAME_INTERVAL_UNALIGNED", c)
	}
}

// TestClassifyDifferent — разные интервалы.
func TestClassifyDifferent(t *testing.T) {
	c := ClassifyInterval(8*time.Hour, 4*time.Hour, time.Now(), time.Now(), true, time.Minute)
	if c != ClassDifferentInterval {
		t.Errorf("class = %s, want DIFFERENT_INTERVAL", c)
	}
}

// TestBuildCalendarDifferentIntervals — сценарий DIFFERENT_INTERVAL: long 8h, short 4h.
// Должно построить события обеих ног.
func TestBuildCalendarDifferentIntervals(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	longEvents := BuildFundingCalendar(CalendarInput{
		Exchange: domain.ExchangeBinance, Side: domain.SideLong,
		PredictedRate: decimal.MustFromString("0.0001"),
		FundingInterval: 8 * time.Hour, NextFundingTime: now.Add(8 * time.Hour),
		Horizon: 16 * time.Hour, Confidence: domain.ConfidenceHigh,
		Notional: decimal.FromInt(1000),
	}, now)
	shortEvents := BuildFundingCalendar(CalendarInput{
		Exchange: domain.ExchangeBybit, Side: domain.SideShort,
		PredictedRate: decimal.MustFromString("0.0003"),
		FundingInterval: 4 * time.Hour, NextFundingTime: now.Add(4 * time.Hour),
		Horizon: 16 * time.Hour, Confidence: domain.ConfidenceHigh,
		Notional: decimal.FromInt(1000),
	}, now)

	// Long (8h, horizon 16h): события в +8h, +16h → 2.
	if len(longEvents) != 2 {
		t.Errorf("long events = %d, want 2", len(longEvents))
	}
	// Short (4h, horizon 16h): +4, +8, +12, +16 → 4.
	if len(shortEvents) != 4 {
		t.Errorf("short events = %d, want 4", len(shortEvents))
	}

	// Calendar корректно используется в SumExpectedFundingCashFlow для всей пары.
	all := append(longEvents, shortEvents...)
	_ = SumExpectedFundingCashFlow(all)
}

// TestCalendarOrdering — события в порядке ScheduledAt.
func TestCalendarOrdering(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	events := BuildFundingCalendar(CalendarInput{
		Side: domain.SideShort, PredictedRate: decimal.MustFromString("0.0001"),
		FundingInterval: 4 * time.Hour, NextFundingTime: now.Add(2 * time.Hour),
		Horizon: 24 * time.Hour, Confidence: domain.ConfidenceHigh,
		Notional: decimal.FromInt(1000),
	}, now)
	for i := 1; i < len(events); i++ {
		if !events[i].ScheduledAt.After(events[i-1].ScheduledAt) {
			t.Errorf("event %d not after %d", i, i-1)
		}
	}
}
