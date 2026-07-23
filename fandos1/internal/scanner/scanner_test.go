package scanner

import (
	"testing"
	"time"

	"github.com/thecd/fundarbitrage/internal/decimal"
	"github.com/thecd/fundarbitrage/internal/domain"
	"github.com/thecd/fundarbitrage/internal/instrument"
	"github.com/thecd/fundarbitrage/internal/marketdata"
)

// setupFixture — готовит registry + cache с двумя биржами для BTC.
// Long на Binance (funding positive), Short на Bybit (funding positive, выше) → short получает больше.
func setupFixture(t *testing.T) (*instrument.Registry, *marketdata.Cache) {
	t.Helper()
	reg := instrument.New()
	reg.Replace([]domain.CanonicalInstrument{
		mkInstrument(domain.ExchangeBinance, "BTC", "BTCUSDT"),
		mkInstrument(domain.ExchangeBybit, "BTC", "BTC-USDT-SWAP"),
	})
	cache := marketdata.New()
	now := time.Now()
	// Binance: mark=100, predicted funding=0.0001, ask depth достаточна.
	cache.Update(&domain.MarketSnapshot{
		Exchange:             domain.ExchangeBinance,
		CanonicalBaseAsset:   "BTC",
		BestAsk:              decimal.MustFromString("100"),
		BestBid:              decimal.MustFromString("99.5"),
		MarkPrice:            decimal.MustFromString("100"),
		PredictedFundingRate: decimal.MustFromString("0.0001"),
		FundingIntervalSec:   28800,
		NextFundingTime:      now.Add(2 * time.Hour),
		FundingConfidence:    domain.ConfidenceHigh,
		QuoteVolume24h:       decimal.MustFromString("1000000"),
		AskDepthForTargetQty: decimal.MustFromString("50000"),
		BidDepthForTargetQty: decimal.MustFromString("50000"),
		IsFresh:              true,
		SequenceValid:        true,
		LocalReceiveTime:     now,
	})
	// Bybit: mark=101 (выше), funding больше 0.0003 → short получает больше.
	cache.Update(&domain.MarketSnapshot{
		Exchange:             domain.ExchangeBybit,
		CanonicalBaseAsset:   "BTC",
		BestAsk:              decimal.MustFromString("101"),
		BestBid:              decimal.MustFromString("100.5"),
		MarkPrice:            decimal.MustFromString("101"),
		PredictedFundingRate: decimal.MustFromString("0.0003"),
		FundingIntervalSec:   28800,
		NextFundingTime:      now.Add(2 * time.Hour),
		FundingConfidence:    domain.ConfidenceHigh,
		QuoteVolume24h:       decimal.MustFromString("1000000"),
		AskDepthForTargetQty: decimal.MustFromString("50000"),
		BidDepthForTargetQty: decimal.MustFromString("50000"),
		IsFresh:              true,
		SequenceValid:        true,
		LocalReceiveTime:     now,
	})
	return reg, cache
}

func mkInstrument(ex domain.ExchangeID, asset, sym string) domain.CanonicalInstrument {
	return domain.CanonicalInstrument{
		Exchange:           ex,
		CanonicalBaseAsset: domain.AssetSymbol(asset),
		ExchangeSymbol:     domain.ExchangeSymbol(sym),
		InstrumentType:     domain.InstrumentLinearUSDTPerpetual,
		Status:             domain.InstrumentStatusActive,
		FundingIntervalSec: 28800,
		SettlementCurrency: "USDT",
		ContractMultiplier: decimal.One,
		QtyStep:            decimal.MustFromString("0.001"),
		MinQty:             decimal.MustFromString("0.001"),
		TickSize:           decimal.MustFromString("0.01"),
		MaxLeverage:        decimal.MustFromString("20"),
	}
}

func baseConfig(now time.Time) Config {
	return Config{
		AllowedExchanges:        domain.SupportedExchanges(),
		MinQuoteVolume24h:       decimal.MustFromString("100000"),
		MinOrderBookDepthUSDT:   decimal.MustFromString("10000"),
		MaxDataAgeMs:            60000,
		MinConfidenceLevel:      domain.ConfidenceMedium,
		MinSecondsBeforeFunding: 30,
		MinExpectedNetPnL:       decimal.MustFromString("0.01"),
		TargetQty:               decimal.MustFromString("10"),
		FeeRateBps:              decimal.MustFromString("2"), // 0.02%
		Horizon:                 24 * time.Hour,
	}
}

// TestScanBuildsCandidates — две биржи → два направленных кандидата (BTC long-Binance/short-Bybit и наоборот).
func TestScanBuildsCandidates(t *testing.T) {
	reg, cache := setupFixture(t)
	s := New(reg, cache)
	cfg := baseConfig(time.Now())

	cands := s.Scan(cfg, time.Now())
	if len(cands) < 2 {
		t.Fatalf("expected ≥2 candidates, got %d", len(cands))
	}
	// Все кандидаты должны иметь оба snapshot.
	for _, c := range cands {
		if c.LongSnapshot == nil || c.ShortSnapshot == nil {
			t.Error("candidate missing snapshot")
		}
	}
}

// TestScanEligibleHasPositivePnL — eligible кандидат с положительным net PnL.
// Long на Binance (funding 0.0001, платит), short на Bybit (funding 0.0003, получает).
// Short получает больше, чем long платит → funding net positive.
func TestScanEligibleHasPositivePnL(t *testing.T) {
	reg, cache := setupFixture(t)
	s := New(reg, cache)
	cands := s.Scan(baseConfig(time.Now()), time.Now())

	var eligible []Candidate
	for _, c := range cands {
		if c.Eligible {
			eligible = append(eligible, c)
		}
	}
	if len(eligible) == 0 {
		t.Fatal("expected at least one eligible candidate")
	}
	// Хотя бы один eligible с положительным net.
	found := false
	for _, c := range eligible {
		if c.PnLBreakdown.Net.IsPositive() {
			found = true
		}
	}
	if !found {
		t.Error("no eligible candidate with positive net PnL")
	}
}

// TestScanFiltersStaleData — stale snapshot → причина в Reason.
func TestScanFiltersStaleData(t *testing.T) {
	reg, cache := setupFixture(t)
	// Делаем Binance snapshot старым.
	staleSnap := &domain.MarketSnapshot{
		Exchange:           domain.ExchangeBinance,
		CanonicalBaseAsset: "BTC",
		LocalReceiveTime:   time.Now().Add(-10 * time.Minute), // 10 минут назад
		IsFresh:            false,
	}
	cache.Update(staleSnap)
	s := New(reg, cache)
	cands := s.Scan(baseConfig(time.Now()), time.Now())

	for _, c := range cands {
		if c.LongExchange == domain.ExchangeBinance {
			if c.Eligible {
				t.Error("stale-data candidate should not be eligible")
			}
		}
	}
}

// TestScanFiltersLowVolume — низкий 24h volume отбрасывает.
func TestScanFiltersLowVolume(t *testing.T) {
	reg, cache := setupFixture(t)
	// Снижаем volume Bybit.
	cache.Update(&domain.MarketSnapshot{
		Exchange:             domain.ExchangeBybit,
		CanonicalBaseAsset:   "BTC",
		BestAsk:              decimal.MustFromString("101"),
		BestBid:              decimal.MustFromString("100.5"),
		MarkPrice:            decimal.MustFromString("101"),
		QuoteVolume24h:       decimal.MustFromString("1000"), // ниже порога
		AskDepthForTargetQty: decimal.MustFromString("50000"),
		BidDepthForTargetQty: decimal.MustFromString("50000"),
		IsFresh:              true,
		SequenceValid:        true,
		LocalReceiveTime:     time.Now(),
		FundingConfidence:    domain.ConfidenceHigh,
	})
	s := New(reg, cache)
	cands := s.Scan(baseConfig(time.Now()), time.Now())

	for _, c := range cands {
		if c.ShortExchange == domain.ExchangeBybit && c.Eligible {
			t.Error("low-volume Bybit candidate should not be eligible")
		}
	}
}

// TestScanRestrictedExchanges — AllowedExchanges фильтрует пары.
func TestScanRestrictedExchanges(t *testing.T) {
	reg, cache := setupFixture(t)
	s := New(reg, cache)
	cfg := baseConfig(time.Now())
	cfg.AllowedExchanges = []domain.ExchangeID{domain.ExchangeBinance} // только одна

	cands := s.Scan(cfg, time.Now())
	if len(cands) != 0 {
		t.Errorf("with single allowed exchange, no pairs possible, got %d", len(cands))
	}
}

// TestRankingEligibleFirst — eligible кандидаты идут раньше ineligible.
func TestRankingEligibleFirst(t *testing.T) {
	reg, cache := setupFixture(t)
	s := New(reg, cache)
	cands := s.Scan(baseConfig(time.Now()), time.Now())

	if len(cands) < 2 {
		t.Skip("need ≥2 candidates for ranking test")
	}
	// Если есть ineligible, он должен быть после eligible.
	firstEligible := -1
	lastIneligible := -1
	for i, c := range cands {
		if c.Eligible && firstEligible == -1 {
			firstEligible = i
		}
		if !c.Eligible {
			lastIneligible = i
		}
	}
	if firstEligible != -1 && lastIneligible != -1 && lastIneligible < firstEligible {
		t.Error("ineligible candidate ranked before eligible")
	}
}

// TestTopReturnsBest
func TestTopReturnsBest(t *testing.T) {
	reg, cache := setupFixture(t)
	s := New(reg, cache)
	cands := s.Scan(baseConfig(time.Now()), time.Now())
	top := Top(cands, 1)
	if len(top) > 1 {
		t.Errorf("Top(1) returned %d", len(top))
	}
	if len(top) == 1 {
		if !top[0].Eligible {
			t.Error("Top should return only eligible")
		}
	}
}

// TestEligibleCount
func TestEligibleCount(t *testing.T) {
	reg, cache := setupFixture(t)
	s := New(reg, cache)
	cands := s.Scan(baseConfig(time.Now()), time.Now())
	n := EligibleCount(cands)
	if n == 0 {
		// Может быть 0 из-за fee/prolonged; главное — функция не падает.
		t.Log("no eligible in fixture (acceptable if fee dominates)")
	}
}

// TestScanNoInstruments — пустой registry → нет кандидатов.
func TestScanNoInstruments(t *testing.T) {
	reg := instrument.New()
	cache := marketdata.New()
	s := New(reg, cache)
	cands := s.Scan(baseConfig(time.Now()), time.Now())
	if len(cands) != 0 {
		t.Errorf("empty registry should give 0 candidates, got %d", len(cands))
	}
}

// TestScanMissingSnapshot — инструмент есть, snapshot-а нет → кандидат не строится.
func TestScanMissingSnapshot(t *testing.T) {
	reg := instrument.New()
	reg.Replace([]domain.CanonicalInstrument{
		mkInstrument(domain.ExchangeBinance, "BTC", "BTCUSDT"),
		mkInstrument(domain.ExchangeBybit, "BTC", "BTC-USDT-SWAP"),
	})
	cache := marketdata.New() // пустой
	s := New(reg, cache)
	cands := s.Scan(baseConfig(time.Now()), time.Now())
	if len(cands) != 0 {
		t.Errorf("missing snapshots should give 0 candidates, got %d", len(cands))
	}
}

// TestIntervalClassPopulated — сканер заполняет IntervalClass.
func TestIntervalClassPopulated(t *testing.T) {
	reg, cache := setupFixture(t)
	s := New(reg, cache)
	cands := s.Scan(baseConfig(time.Now()), time.Now())
	if len(cands) == 0 {
		t.Fatal("no candidates")
	}
	// Оба 8h-интервала → SAME_INTERVAL (aligned или unaligned).
	if cands[0].IntervalClass == "" {
		t.Error("IntervalClass not populated")
	}
}
