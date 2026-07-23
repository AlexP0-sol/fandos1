package allocation

import (
	"strings"
	"testing"

	"github.com/thecd/fundarbitrage/internal/decimal"
	"github.com/thecd/fundarbitrage/internal/domain"
)

func req(id string, score, qty, notional string, longEx, shortEx domain.ExchangeID, asset string) CandidateRequest {
	return CandidateRequest{
		ID:              id,
		Asset:           domain.AssetSymbol(asset),
		LongExchange:    longEx,
		ShortExchange:   shortEx,
		CompositeScore:  decimal.MustFromString(score),
		DesiredQty:      decimal.MustFromString(qty),
		NotionalPerUnit: decimal.MustFromString(notional),
	}
}

func fullLimits() Limits {
	return Limits{
		MaxExposurePerExchangeUSDT: map[domain.ExchangeID]decimal.Decimal{
			domain.ExchangeBinance: decimal.MustFromString("10000"),
			domain.ExchangeBybit:   decimal.MustFromString("10000"),
			domain.ExchangeOKX:     decimal.MustFromString("10000"),
		},
		CurrentExposureUSDT: map[domain.ExchangeID]decimal.Decimal{},
		MaxCorrelatedNotionalUSDT: map[domain.AssetSymbol]decimal.Decimal{
			"BTC": decimal.MustFromString("8000"),
			"ETH": decimal.MustFromString("8000"),
		},
		CurrentCorrelatedUSDT: map[domain.AssetSymbol]decimal.Decimal{},
		TotalBudgetUSDT:       decimal.MustFromString("20000"),
		CurrentTotalUSDT:      decimal.Zero,
	}
}

// TestSingleCandidateFullAllocation — один кандидат, все лимиты свободны → полный объём.
func TestSingleCandidateFullAllocation(t *testing.T) {
	r := req("c1", "0.9", "10", "100", domain.ExchangeBinance, domain.ExchangeBybit, "BTC")
	plan := Allocate([]CandidateRequest{r}, fullLimits())
	if len(plan.Allocations) != 1 {
		t.Fatalf("got %d allocations", len(plan.Allocations))
	}
	a := plan.Allocations[0]
	if a.Rejected {
		t.Errorf("rejected: %s", a.Reason)
	}
	// 10 qty × 100 = 1000 USDT — полностью вписывается.
	if !a.AllocatedQty.Equal(decimal.MustFromString("10")) {
		t.Errorf("qty = %s, want 10", a.AllocatedQty.String())
	}
	if !a.AllocatedNotional.Equal(decimal.MustFromString("1000")) {
		t.Errorf("notional = %s, want 1000", a.AllocatedNotional.String())
	}
}

// TestBudgetCapsAllocation — бюджет меньше desired → частичная аллокация.
func TestBudgetCapsAllocation(t *testing.T) {
	r := req("c1", "0.9", "100", "100", domain.ExchangeBinance, domain.ExchangeBybit, "BTC")
	limits := fullLimits()
	limits.TotalBudgetUSDT = decimal.MustFromString("5000") // 100×100=10000 > 5000
	plan := Allocate([]CandidateRequest{r}, limits)
	a := plan.Allocations[0]
	if a.Rejected {
		t.Fatal("should not reject, just cap")
	}
	// 5000/100 = 50 qty.
	if !a.AllocatedQty.Equal(decimal.MustFromString("50")) {
		t.Errorf("qty = %s, want 50 (budget cap)", a.AllocatedQty.String())
	}
}

// TestPerExchangeExposureCap — лимит на бирже ограничивает обе ноги.
func TestPerExchangeExposureCap(t *testing.T) {
	r := req("c1", "0.9", "100", "100", domain.ExchangeBinance, domain.ExchangeBybit, "BTC")
	limits := fullLimits()
	// Binance cap = 3000 → 30 qty.
	limits.MaxExposurePerExchangeUSDT[domain.ExchangeBinance] = decimal.MustFromString("3000")
	plan := Allocate([]CandidateRequest{r}, limits)
	a := plan.Allocations[0]
	if !a.AllocatedQty.Equal(decimal.MustFromString("30")) {
		t.Errorf("qty = %s, want 30 (binance cap)", a.AllocatedQty.String())
	}
}

// TestCorrelationCap — лимит по активу ограничивает суммарный notional этого актива.
func TestCorrelationCap(t *testing.T) {
	// Два кандидата на BTC (разные пары бирж) — суммарный notional ограничен asset cap.
	r1 := req("c1", "0.9", "50", "100", domain.ExchangeBinance, domain.ExchangeBybit, "BTC")
	r2 := req("c2", "0.8", "50", "100", domain.ExchangeBinance, domain.ExchangeOKX, "BTC")
	limits := fullLimits()
	limits.MaxCorrelatedNotionalUSDT["BTC"] = decimal.MustFromString("5000")
	plan := Allocate([]CandidateRequest{r1, r2}, limits)

	// c1 (более высокий score) берёт 50×100=5000 (весь asset cap).
	a1 := plan.Allocations[0]
	if !a1.AllocatedQty.Equal(decimal.MustFromString("50")) {
		t.Errorf("c1 qty = %s, want 50", a1.AllocatedQty.String())
	}
	// c2 должен быть rejected (asset cap исчерпан).
	a2 := plan.Allocations[1]
	if !a2.Rejected {
		t.Errorf("c2 should be rejected (BTC cap exhausted), got qty=%s", a2.AllocatedQty.String())
	}
}

// TestScorePriority — выше score получает приоритет при ограниченном бюджете.
func TestScorePriority(t *testing.T) {
	// Два кандидата, бюджет только на один.
	r1 := req("c1", "0.5", "50", "100", domain.ExchangeBinance, domain.ExchangeBybit, "BTC")
	r2 := req("c2", "0.9", "50", "100", domain.ExchangeBinance, domain.ExchangeOKX, "ETH")
	limits := fullLimits()
	limits.TotalBudgetUSDT = decimal.MustFromString("5000") // только 50 qty × 100
	plan := Allocate([]CandidateRequest{r1, r2}, limits)

	// c2 (score 0.9) должен быть первым и получить полный объём.
	first := plan.Allocations[0]
	if first.Request.ID != "c2" {
		t.Errorf("expected c2 first by score, got %s", first.Request.ID)
	}
	if first.Rejected {
		t.Error("higher-score candidate rejected")
	}
}

// TestCurrentExposureReducesAvailable — открытые позиции уменьшают доступный budget.
func TestCurrentExposureReducesAvailable(t *testing.T) {
	r := req("c1", "0.9", "100", "100", domain.ExchangeBinance, domain.ExchangeBybit, "BTC")
	limits := fullLimits()
	// Уже есть 4000 на Binance → осталось 6000.
	limits.CurrentExposureUSDT[domain.ExchangeBinance] = decimal.MustFromString("4000")
	plan := Allocate([]CandidateRequest{r}, limits)
	a := plan.Allocations[0]
	// Binance cap: (10000 - 4000) / 100 = 60 qty.
	if !a.AllocatedQty.Equal(decimal.MustFromString("60")) {
		t.Errorf("qty = %s, want 60 (after current exposure)", a.AllocatedQty.String())
	}
}

// TestRejectNonPositiveQty — нулевой/отрицательный desired отклоняется.
func TestRejectNonPositiveQty(t *testing.T) {
	r := req("c1", "0.9", "0", "100", domain.ExchangeBinance, domain.ExchangeBybit, "BTC")
	plan := Allocate([]CandidateRequest{r}, fullLimits())
	if !plan.Allocations[0].Rejected {
		t.Error("zero qty should be rejected")
	}
}

// TestRejectNonPositiveScore
func TestRejectNonPositiveScore(t *testing.T) {
	r := req("c1", "0", "10", "100", domain.ExchangeBinance, domain.ExchangeBybit, "BTC")
	plan := Allocate([]CandidateRequest{r}, fullLimits())
	if !plan.Allocations[0].Rejected {
		t.Error("zero score should be rejected")
	}
}

// TestRejectNoBudget — все лимиты исчерпаны.
func TestRejectNoBudget(t *testing.T) {
	r := req("c1", "0.9", "10", "100", domain.ExchangeBinance, domain.ExchangeBybit, "BTC")
	limits := fullLimits()
	limits.TotalBudgetUSDT = decimal.Zero
	plan := Allocate([]CandidateRequest{r}, limits)
	if !plan.Allocations[0].Rejected {
		t.Error("zero budget should reject")
	}
}

// TestPlanSums — TotalAllocatedUSDT корректно суммирует.
func TestPlanSums(t *testing.T) {
	r1 := req("c1", "0.9", "10", "100", domain.ExchangeBinance, domain.ExchangeBybit, "BTC")
	r2 := req("c2", "0.8", "5", "100", domain.ExchangeBinance, domain.ExchangeOKX, "ETH")
	plan := Allocate([]CandidateRequest{r1, r2}, fullLimits())
	// 10×100 + 5×100 = 1500.
	if !plan.TotalAllocatedUSDT.Equal(decimal.MustFromString("1500")) {
		t.Errorf("total = %s, want 1500", plan.TotalAllocatedUSDT.String())
	}
	// Remaining = 20000 - 1500 = 18500.
	if !plan.RemainingBudgetUSDT.Equal(decimal.MustFromString("18500")) {
		t.Errorf("remaining = %s, want 18500", plan.RemainingBudgetUSDT.String())
	}
}

// TestEligibleAllocationsFilter
func TestEligibleAllocationsFilter(t *testing.T) {
	r1 := req("c1", "0.9", "10", "100", domain.ExchangeBinance, domain.ExchangeBybit, "BTC")
	r2 := req("c2", "0", "10", "100", domain.ExchangeBinance, domain.ExchangeOKX, "ETH") // rejected
	plan := Allocate([]CandidateRequest{r1, r2}, fullLimits())
	elig := EligibleAllocations(plan)
	if len(elig) != 1 {
		t.Errorf("eligible = %d, want 1", len(elig))
	}
}

// TestFirstRejectReason
func TestFirstRejectReason(t *testing.T) {
	r := req("c1", "0", "10", "100", domain.ExchangeBinance, domain.ExchangeBybit, "BTC")
	plan := Allocate([]CandidateRequest{r}, fullLimits())
	reason, ok := FirstRejectReason(plan)
	if !ok || reason == "" {
		t.Error("expected reject reason")
	}
}

// TestSameExchangeNoDoubleCount — когда long и short на одной бирже,
// notional в exchange bucket добавляется только один раз.
func TestSameExchangeNoDoubleCount(t *testing.T) {
	// Binance cap = 5000. long=Binance, short=Binance.
	// Desired notional = 10 × 100 = 1000. Должен пройти целиком — без двойного счёта.
	limits := fullLimits()
	limits.MaxExposurePerExchangeUSDT[domain.ExchangeBinance] = decimal.MustFromString("5000")

	r := req("c1", "0.9", "10", "100", domain.ExchangeBinance, domain.ExchangeBinance, "BTC")
	plan := Allocate([]CandidateRequest{r}, limits)
	a := plan.Allocations[0]
	if a.Rejected {
		t.Errorf("same-exchange should not reject: %s", a.Reason)
	}
	if !a.AllocatedQty.Equal(decimal.MustFromString("10")) {
		t.Errorf("qty = %s, want 10 (no double count)", a.AllocatedQty.String())
	}
	// После аллокации в exchange bucket должно быть 1000, а не 2000.
	// Проверяем косвенно: следующий кандидат берёт оставшиеся 4000/100=40 qty.
	r2 := req("c2", "0.8", "100", "100", domain.ExchangeBinance, domain.ExchangeBinance, "ETH")
	plan2 := Allocate([]CandidateRequest{r, r2}, limits)
	a2 := plan2.Allocations[1]
	if a2.Rejected {
		t.Errorf("c2 should not reject after same-exchange c1: %s", a2.Reason)
	}
	// c1 заняла 1000 → осталось 4000, c2 может взять 40 qty.
	if !a2.AllocatedQty.Equal(decimal.MustFromString("40")) {
		t.Errorf("c2 qty = %s, want 40 (4000 remaining after single-count c1)", a2.AllocatedQty.String())
	}
}

// TestOverLimitExchangeRejectedWithName — биржа с нулевым/отрицательным доступным
// бюджетом отклоняется с указанием имени биржи в причине.
func TestOverLimitExchangeRejectedWithName(t *testing.T) {
	limits := fullLimits()
	// Полностью заполняем Binance.
	limits.CurrentExposureUSDT[domain.ExchangeBinance] = limits.MaxExposurePerExchangeUSDT[domain.ExchangeBinance]

	r := req("c1", "0.9", "10", "100", domain.ExchangeBinance, domain.ExchangeBybit, "BTC")
	plan := Allocate([]CandidateRequest{r}, limits)
	a := plan.Allocations[0]
	if !a.Rejected {
		t.Error("over-limit exchange should reject")
	}
	if !strings.Contains(a.Reason, string(domain.ExchangeBinance)) {
		t.Errorf("reject reason %q should mention exchange name %q", a.Reason, domain.ExchangeBinance)
	}
}

// TestEmptyRequests — пустой список запросов → пустой план, полный остаток бюджета.
func TestEmptyRequests(t *testing.T) {
	limits := fullLimits()
	plan := Allocate(nil, limits)
	if len(plan.Allocations) != 0 {
		t.Errorf("allocations = %d, want 0", len(plan.Allocations))
	}
	if !plan.TotalAllocatedUSDT.IsZero() {
		t.Errorf("total allocated = %s, want 0", plan.TotalAllocatedUSDT.String())
	}
	if !plan.RemainingBudgetUSDT.Equal(limits.TotalBudgetUSDT) {
		t.Errorf("remaining = %s, want %s (full budget)", plan.RemainingBudgetUSDT.String(), limits.TotalBudgetUSDT.String())
	}
}
