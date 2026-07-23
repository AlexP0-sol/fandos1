package risk

import (
	"testing"

	"github.com/thecd/fundarbitrage/internal/decimal"
	"github.com/thecd/fundarbitrage/internal/domain"
)

func defaultLimits() Limits {
	return Limits{
		DeltaToleranceBase:        decimal.MustFromString("1"),
		DeltaToleranceUSD:         decimal.MustFromString("100"),
		MaxDailyLossUSDT:          decimal.MustFromString("500"),
		MaxPositionLossUSDT:       decimal.MustFromString("200"),
		MinimumDistanceToLiquidationPercent: decimal.MustFromString("20"),
		EmergencyMarginRatio:      decimal.MustFromString("0.8"),
		CounterpartyHaircutPercent: map[domain.CounterpartyRiskTier]decimal.Decimal{
			domain.CounterpartyTierA: decimal.MustFromString("0.001"),
			domain.CounterpartyTierB: decimal.MustFromString("0.005"),
			domain.CounterpartyTierC: decimal.MustFromString("0.02"),
		},
		RiskSnapAfterMaxDailyLoss: true,
	}
}

// TestDeltaBaseHedged — хеджированная позиция не нарушает дельту.
func TestDeltaBaseHedged(t *testing.T) {
	p := PositionInput{
		LongBaseQty:   decimal.MustFromString("50"),
		ShortBaseQty:  decimal.MustFromString("50"),
		MarkPrice:     decimal.MustFromString("100"),
	}
	res := CheckPosition(p, defaultLimits())
	if res.HasCritical() {
		t.Errorf("hedged position should have no critical violations: %+v", res.Violations)
	}
	if !res.DeltaBase.IsZero() {
		t.Errorf("delta = %s, want 0", res.DeltaBase.String())
	}
}

// TestDeltaBaseBreach — разница в базе выше tolerance → critical.
func TestDeltaBaseBreach(t *testing.T) {
	p := PositionInput{
		LongBaseQty:  decimal.MustFromString("55"), // delta = 5 > tolerance 1
		ShortBaseQty: decimal.MustFromString("50"),
		MarkPrice:    decimal.MustFromString("100"),
	}
	res := CheckPosition(p, defaultLimits())
	if !res.HasCritical() {
		t.Fatal("expected critical violation on delta base breach")
	}
	found := false
	for _, v := range res.Violations {
		if v.Code == "DELTA_BASE_BREACH" {
			found = true
		}
	}
	if !found {
		t.Error("DELTA_BASE_BREACH not in violations")
	}
}

// TestDeltaUSDBreach — дельта в USD выше tolerance.
func TestDeltaUSDBreach(t *testing.T) {
	// tolerance base = 1, но USD = 1 × 1000 = 1000 > 100 USD tolerance.
	p := PositionInput{
		LongBaseQty:  decimal.MustFromString("51"),
		ShortBaseQty: decimal.MustFromString("50"),
		MarkPrice:    decimal.MustFromString("1000"),
	}
	res := CheckPosition(p, defaultLimits())
	found := false
	for _, v := range res.Violations {
		if v.Code == "DELTA_USD_BREACH" {
			found = true
		}
	}
	if !found {
		t.Error("expected DELTA_USD_BREACH")
	}
}

// TestEmergencyMarginRatio — margin ratio близко к ликвидации.
func TestEmergencyMarginRatio(t *testing.T) {
	p := PositionInput{
		LongBaseQty:   decimal.MustFromString("50"),
		ShortBaseQty:  decimal.MustFromString("50"),
		MarkPrice:     decimal.MustFromString("100"),
		MarginRatioPerLeg: map[domain.ExchangeID]decimal.Decimal{
			domain.ExchangeBinance: decimal.MustFromString("0.85"), // > emergency 0.8
		},
	}
	res := CheckPosition(p, defaultLimits())
	if !res.HasCritical() {
		t.Fatal("expected critical on emergency margin ratio")
	}
}

// TestLiquidationTooClose — дистанция до ликвидации ниже минимума.
func TestLiquidationTooClose(t *testing.T) {
	p := PositionInput{
		LongBaseQty:  decimal.MustFromString("50"),
		ShortBaseQty: decimal.MustFromString("50"),
		MarkPrice:    decimal.MustFromString("100"),
		LiquidationDistancePercent: map[domain.ExchangeID]decimal.Decimal{
			domain.ExchangeBinance: decimal.MustFromString("10"), // < 20 minimum
		},
	}
	res := CheckPosition(p, defaultLimits())
	found := false
	for _, v := range res.Violations {
		if v.Code == "LIQUIDATION_TOO_CLOSE" {
			found = true
		}
	}
	if !found {
		t.Error("expected LIQUIDATION_TOO_CLOSE")
	}
}

// TestMaxPositionLoss — убыток выше MaxPositionLoss.
func TestMaxPositionLoss(t *testing.T) {
	p := PositionInput{
		LongBaseQty:   decimal.MustFromString("50"),
		ShortBaseQty:  decimal.MustFromString("50"),
		MarkPrice:     decimal.MustFromString("100"),
		UnrealizedPnL: decimal.MustFromString("-300"), // |loss| > 200
	}
	res := CheckPosition(p, defaultLimits())
	found := false
	for _, v := range res.Violations {
		if v.Code == "MAX_POSITION_LOSS" {
			found = true
		}
	}
	if !found {
		t.Error("expected MAX_POSITION_LOSS")
	}
}

// TestMaxPositionLossNotTriggered — положительный PnL не считается убытком.
func TestMaxPositionLossNotTriggered(t *testing.T) {
	p := PositionInput{
		LongBaseQty:   decimal.MustFromString("50"),
		ShortBaseQty:  decimal.MustFromString("50"),
		MarkPrice:     decimal.MustFromString("100"),
		UnrealizedPnL: decimal.MustFromString("1000"), // прибыль, не убыток
	}
	res := CheckPosition(p, defaultLimits())
	for _, v := range res.Violations {
		if v.Code == "MAX_POSITION_LOSS" {
			t.Error("profit should not trigger MAX_POSITION_LOSS")
		}
	}
}

// TestDailyLossWithinLimit — убыток в пределах лимита.
func TestDailyLossWithinLimit(t *testing.T) {
	st, vs := CheckDailyLoss(decimal.MustFromString("-300"), defaultLimits())
	if st.Snapped {
		t.Error("300 < 500 should not snap")
	}
	if len(vs) != 0 {
		t.Errorf("expected 0 violations, got %d", len(vs))
	}
	if !st.RealisedLossUSDT.Equal(decimal.MustFromString("300")) {
		t.Errorf("loss = %s, want 300", st.RealisedLossUSDT.String())
	}
}

// TestDailyLossSnaps — убыток выше лимита + RiskSnap → SAFE_HALT.
func TestDailyLossSnaps(t *testing.T) {
	st, vs := CheckDailyLoss(decimal.MustFromString("-600"), defaultLimits())
	if !st.Snapped {
		t.Error("expected snap when loss > MaxDailyLoss and RiskSnap enabled")
	}
	if len(vs) == 0 {
		t.Error("expected MAX_DAILY_LOSS violation")
	}
}

// TestDailyLossNoSnapWhenDisabled — RiskSnap=false не вызывает snap.
func TestDailyLossNoSnapWhenDisabled(t *testing.T) {
	l := defaultLimits()
	l.RiskSnapAfterMaxDailyLoss = false
	st, _ := CheckDailyLoss(decimal.MustFromString("-600"), l)
	if st.Snapped {
		t.Error("should not snap when RiskSnapAfterMaxDailyLoss=false")
	}
}

// TestDailyLossPositivePnL — прибыль за день → нулевой loss.
func TestDailyLossPositivePnL(t *testing.T) {
	st, _ := CheckDailyLoss(decimal.MustFromString("300"), defaultLimits())
	if !st.RealisedLossUSDT.IsZero() {
		t.Errorf("positive day PnL should give zero loss, got %s", st.RealisedLossUSDT.String())
	}
}

// TestCounterpartyReserveByTier — haircut растёт от A к C.
func TestCounterpartyReserveByTier(t *testing.T) {
	limits := defaultLimits()
	notional := decimal.MustFromString("10000")
	rA := CounterpartyReserve(notional, domain.CounterpartyTierA, limits.CounterpartyHaircutPercent)
	rC := CounterpartyReserve(notional, domain.CounterpartyTierC, limits.CounterpartyHaircutPercent)
	if !rC.GreaterThan(rA) {
		t.Errorf("tier C reserve %s should exceed A %s", rC.String(), rA.String())
	}
	// 10000 × 0.02 = 200.
	if !rC.Equal(decimal.MustFromString("200")) {
		t.Errorf("tier C reserve = %s, want 200", rC.String())
	}
}

// TestCounterpartyReserveNil — nil haircut map → 0.
func TestCounterpartyReserveNil(t *testing.T) {
	r := CounterpartyReserve(decimal.MustFromString("1000"), domain.CounterpartyTierA, nil)
	if !r.IsZero() {
		t.Errorf("nil haircuts → %s, want 0", r.String())
	}
}

// TestADLExposureBreached
func TestADLExposureBreached(t *testing.T) {
	breached, over := ADLExposureBreached(decimal.MustFromString("1500"), decimal.MustFromString("1000"))
	if !breached {
		t.Error("expected breach")
	}
	if !over.Equal(decimal.FromInt(500)) {
		t.Errorf("over = %s, want 500", over.String())
	}
	// В пределах.
	breached2, _ := ADLExposureBreached(decimal.MustFromString("800"), decimal.MustFromString("1000"))
	if breached2 {
		t.Error("800 < 1000 should not breach")
	}
	// Без лимита.
	breached3, _ := ADLExposureBreached(decimal.MustFromString("99999"), decimal.Zero)
	if breached3 {
		t.Error("zero limit should not trigger breach")
	}
}
