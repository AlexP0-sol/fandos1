// Package scanner реализует Level 3 сканер кандидатов (раздел 8 промпта v2).
//
// Scanner объединяет:
//   - instrument registry (доступные инструменты)
//   - market data cache (снимки рынка)
//   - funding calendar (раздел 8.3)
//   - ExpectedNetPnL (раздел 3.4)
//   - candidate scoring (раздел 8.4)
//
// Для каждого канонического актива scanner строит пары бирж (long на A, short на B),
// проверяет первичные фильтры, считает VWAP basis, ExpectedNetPnL, scores,
// и возвращает ranked list eligible кандидатов.
//
// Scanner НЕ размещает ордера — он только оценивает. Решение об открытии принимает
// пользователь (semi-auto) или portfolio engine (auto) после capital allocation.
package scanner

import (
	"sort"
	"time"

	"github.com/thecd/fundarbitrage/internal/decimal"
	"github.com/thecd/fundarbitrage/internal/domain"
	"github.com/thecd/fundarbitrage/internal/instrument"
	"github.com/thecd/fundarbitrage/internal/marketdata"
	"github.com/thecd/fundarbitrage/internal/orderbook"
	"github.com/thecd/fundarbitrage/internal/strategy"
)

// Candidate — один評価енный кандидат пары long/short.
type Candidate struct {
	Asset             domain.AssetSymbol
	LongExchange      domain.ExchangeID
	ShortExchange     domain.ExchangeID
	LongSymbol        domain.ExchangeSymbol
	ShortSymbol       domain.ExchangeSymbol

	// Market data на момент оценки.
	LongSnapshot  *domain.MarketSnapshot
	ShortSnapshot *domain.MarketSnapshot

	// Funding.
	LongFundingInterval  time.Duration
	ShortFundingInterval time.Duration
	IntervalClass        strategy.IntervalClass

	// VWAP basis.
	EntryBasisVWAP decimal.Decimal
	LongEntryVWAP  decimal.Decimal
	ShortEntryVWAP decimal.Decimal

	// PnL.
	PnLBreakdown strategy.PnLBreakdown

	// Scores.
	Scores strategy.CandidateScores
	CompositeScore decimal.Decimal

	// Eligibility.
	Eligible bool
	Reason   string // причина отклонения

	// Metadata.
	EvaluatedAt time.Time
	SecondsToFunding int64 // до ближайшего funding события (min из двух ног)
}

// Config — параметры сканирования (из HotSettings раздела 5.1).
type Config struct {
	AllowedExchanges      []domain.ExchangeID
	MinQuoteVolume24h     decimal.Decimal
	MinOrderBookDepthUSDT decimal.Decimal
	MaxDataAgeMs          int64
	MinConfidenceLevel    domain.ConfidenceLevel
	MinSecondsBeforeFunding int64
	MinExpectedNetPnL     decimal.Decimal
	TargetQty             decimal.Decimal // целевой объём для VWAP (раздел 9)
	FeeRateBps            decimal.Decimal // торговая комиссия в bps (для оценки)
	Horizon               time.Duration   // горизонт удержания для funding calendar
}

// Scanner — сканер кандидатов.
type Scanner struct {
	registry *instrument.Registry
	cache    *marketdata.Cache
}

// New создаёт сканер.
func New(reg *instrument.Registry, cache *marketdata.Cache) *Scanner {
	return &Scanner{registry: reg, cache: cache}
}

// Scan обходит все канонические активы и строит ranked candidates.
// now передаётся явно для тестируемости.
func (s *Scanner) Scan(cfg Config, now time.Time) []Candidate {
	assets := s.registry.AllCanonicalAssets()
	allowed := allowedSet(cfg.AllowedExchanges)

	var candidates []Candidate
	for _, asset := range assets {
		instruments := s.registry.InstrumentsForAsset(asset)
		// Строим все пары (long на exA, short на exB), A != B.
		for i, longIns := range instruments {
			if !allowed[longIns.Exchange] {
				continue
			}
			for j, shortIns := range instruments {
				if i == j {
					continue
				}
				if !allowed[shortIns.Exchange] {
					continue
				}
				cand := s.evaluatePair(cfg, longIns, shortIns, now)
				if cand != nil {
					candidates = append(candidates, *cand)
				}
			}
		}
	}

	// Ранжирование: eligible первыми, по убыванию ExpectedNetPnL.
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].Eligible != candidates[j].Eligible {
			return candidates[i].Eligible // eligible выше
		}
		return candidates[i].PnLBreakdown.Net.GreaterThan(candidates[j].PnLBreakdown.Net)
	})

	return candidates
}

// evaluatePair оценивает одну пару (longIns, shortIns).
// Возвращает nil, если данные отсутствуют или первичные фильтры не пройдены.
func (s *Scanner) evaluatePair(cfg Config, longIns, shortIns domain.CanonicalInstrument, now time.Time) *Candidate {
	// 1. Проверяем наличие снимков.
	longSnap, ok := s.cache.Get(longIns.Exchange, longIns.CanonicalBaseAsset)
	if !ok {
		return nil
	}
	shortSnap, ok := s.cache.Get(shortIns.Exchange, shortIns.CanonicalBaseAsset)
	if !ok {
		return nil
	}

	cand := &Candidate{
		Asset:         longIns.CanonicalBaseAsset,
		LongExchange:  longIns.Exchange,
		ShortExchange: shortIns.Exchange,
		LongSymbol:    longIns.ExchangeSymbol,
		ShortSymbol:   shortIns.ExchangeSymbol,
		LongSnapshot:  longSnap,
		ShortSnapshot: shortSnap,
		EvaluatedAt:   now,
	}

	// 2. Первичные фильтры (раздел 8.1).
	if reason := s.primaryFilters(cfg, longIns, shortIns, longSnap, shortSnap, now); reason != "" {
		cand.Reason = reason
		return cand
	}

	// 3. VWAP basis (через orderbook, если глубина доступна в snapshot).
	// Для сканера используем BestBid/BestAsk как аппроксимацию (точный VWAP в Level 3/preflight).
	if longSnap.BestAsk.IsZero() || shortSnap.BestBid.IsZero() {
		cand.Reason = "missing BBO for basis"
		return cand
	}
	cand.LongEntryVWAP = longSnap.BestAsk
	cand.ShortEntryVWAP = shortSnap.BestBid
	// EntryBasisRaw = ShortBestBid / LongBestAsk - 1 (раздел 3.3).
	cand.EntryBasisVWAP = shortSnap.BestBid.Div(longSnap.BestAsk).Sub(decimal.One)

	// 4. Funding calendar (раздел 8.3).
	longFundingInt := time.Duration(longIns.FundingIntervalSec) * time.Second
	shortFundingInt := time.Duration(shortIns.FundingIntervalSec) * time.Second
	cand.LongFundingInterval = longFundingInt
	cand.ShortFundingInterval = shortFundingInt
	cand.IntervalClass = strategy.ClassifyInterval(
		longFundingInt, shortFundingInt,
		longSnap.NextFundingTime, shortSnap.NextFundingTime,
		true, // requireAligned — упрощение для v1; полная настройка в portfolio engine
		5*time.Minute,
	)

	// 5. Seconds to funding (минимум из двух ног).
	longSecs := int64(time.Until(longSnap.NextFundingTime).Seconds())
	shortSecs := int64(time.Until(shortSnap.NextFundingTime).Seconds())
	cand.SecondsToFunding = minInt64(longSecs, shortSecs)

	// 6. ExpectedFundingPnL через funding calendar.
	notional := cfg.TargetQty.Mul(longSnap.MarkPrice)
	longEvents := strategy.BuildFundingCalendar(strategy.CalendarInput{
		Exchange: longIns.Exchange, Symbol: longIns.ExchangeSymbol, Side: domain.SideLong,
		PredictedRate: longSnap.PredictedFundingRate,
		FundingInterval: longFundingInt, NextFundingTime: longSnap.NextFundingTime,
		Horizon: cfg.Horizon, Confidence: longSnap.FundingConfidence, Notional: notional,
	}, now)
	shortEvents := strategy.BuildFundingCalendar(strategy.CalendarInput{
		Exchange: shortIns.Exchange, Symbol: shortIns.ExchangeSymbol, Side: domain.SideShort,
		PredictedRate: shortSnap.PredictedFundingRate,
		FundingInterval: shortFundingInt, NextFundingTime: shortSnap.NextFundingTime,
		Horizon: cfg.Horizon, Confidence: shortSnap.FundingConfidence, Notional: notional,
	}, now)
	expectedFundingPnL := strategy.SumExpectedFundingCashFlow(append(longEvents, shortEvents...))

	// 7. ExpectedBasisPnL (упрощённо: entry basis × notional).
	expectedBasisPnL := cand.EntryBasisVWAP.Mul(notional)

	// 8. Fees.
	feeCost := cfg.FeeRateBps.MulInt(10000).Mul(notional) // bps → fraction × notional
	// На самом деле feeRateBps уже в bps, нужно notional × feeRate/10000.
	// Исправлено ниже.
	feeCost = notional.Mul(cfg.FeeRateBps).Div(decimal.MustFromString("10000")).MulInt(2) // entry+exit

	// 9. ExpectedNetPnL.
	bd := strategy.ExpectedNetPnL(strategy.PnLInput{
		ExpectedFundingPnL: expectedFundingPnL,
		ExpectedBasisPnL:   expectedBasisPnL,
		EstimatedEntryFees: feeCost.Div(decimal.FromInt(2)),
		EstimatedExitFees:  feeCost.Div(decimal.FromInt(2)),
	})
	cand.PnLBreakdown = bd

	// 10. Scores (раздел 8.4).
	cand.Scores = s.calculateScores(cfg, longSnap, shortSnap)
	cand.CompositeScore = cand.Scores.Composite(strategy.DefaultWeights)

	// 11. Eligibility.
	check := strategy.CheckEligibility(
		bd.Net, cfg.MinExpectedNetPnL,
		minConfidence(longSnap.FundingConfidence, shortSnap.FundingConfidence),
		cfg.MinConfidenceLevel,
		cand.SecondsToFunding, cfg.MinSecondsBeforeFunding,
	)
	cand.Eligible = check.Eligible
	cand.Reason = check.Reason

	return cand
}

// primaryFilters — раздел 8.1. Возвращает непустую причину при отклонении.
func (s *Scanner) primaryFilters(cfg Config, longIns, shortIns domain.CanonicalInstrument,
	longSnap, shortSnap *domain.MarketSnapshot, now time.Time) string {

	// Активный статус инструмента.
	if !longIns.Status.IsTradable() {
		return "long instrument not tradable"
	}
	if !shortIns.Status.IsTradable() {
		return "short instrument not tradable"
	}
	// Свежесть данных.
	maxAge := time.Duration(cfg.MaxDataAgeMs) * time.Millisecond
	if now.Sub(longSnap.LocalReceiveTime) > maxAge {
		return "long data stale"
	}
	if now.Sub(shortSnap.LocalReceiveTime) > maxAge {
		return "short data stale"
	}
	// 24h volume.
	if longSnap.QuoteVolume24h.LessThan(cfg.MinQuoteVolume24h) {
		return "long 24h volume below threshold"
	}
	if shortSnap.QuoteVolume24h.LessThan(cfg.MinQuoteVolume24h) {
		return "short 24h volume below threshold"
	}
	// Order book depth (через snapshot fields).
	if longSnap.AskDepthForTargetQty.LessThan(cfg.MinOrderBookDepthUSDT) {
		return "long depth below threshold"
	}
	if shortSnap.BidDepthForTargetQty.LessThan(cfg.MinOrderBookDepthUSDT) {
		return "short depth below threshold"
	}
	return ""
}

// calculateScores — набор scores из рыночных данных (раздел 8.4).
func (s *Scanner) calculateScores(cfg Config, longSnap, shortSnap *domain.MarketSnapshot) strategy.CandidateScores {
	// Liquidity: средний depth ratio двух ног.
	longDepth := strategy.DepthRatioToLiquidityScore(
		longSnap.AskDepthForTargetQty.Div(cfg.MinOrderBookDepthUSDT))
	shortDepth := strategy.DepthRatioToLiquidityScore(
		shortSnap.BidDepthForTargetQty.Div(cfg.MinOrderBookDepthUSDT))
	liq := strategy.Score{longDepth.Value.Add(shortDepth.Value).Div(decimal.FromInt(2))}

	// FundingConfidence: минимальная из двух ног.
	confScore := strategy.ConfidenceToScore(
		minConfidence(longSnap.FundingConfidence, shortSnap.FundingConfidence))

	// DataQuality.
	dqLong := strategy.DataQualityFromFlags(longSnap.IsFresh, longSnap.SequenceValid)
	dqShort := strategy.DataQualityFromFlags(shortSnap.IsFresh, shortSnap.SequenceValid)
	dq := strategy.Score{dqLong.Value.Add(dqShort.Value).Div(decimal.FromInt(2))}

	// ADL.
	longADL := decimal.Zero
	if longSnap.ADLQueuePosition != nil {
		longADL = maxDec(longSnap.ADLQueuePosition.LongQueue, longSnap.ADLQueuePosition.ShortQueue)
	}
	shortADL := decimal.Zero
	if shortSnap.ADLQueuePosition != nil {
		shortADL = maxDec(shortSnap.ADLQueuePosition.LongQueue, shortSnap.ADLQueuePosition.ShortQueue)
	}
	adlScore := strategy.ADLQueueToScore(maxDec(longADL, shortADL))

	return strategy.CandidateScores{
		LiquidityScore:         liq,
		FundingConfidenceScore: confScore,
		DataQualityScore:       dq,
		ADLRiskScore:           adlScore,
		// BasisStability / ExecutionRisk / Counterparty — заполняются portfolio engine
		// на основе более полной информации (историческая стабильность, tier биржи).
	}
}

// allowedSet — конвертирует список разрешённых бирж в set.
func allowedSet(exchanges []domain.ExchangeID) map[domain.ExchangeID]bool {
	if len(exchanges) == 0 {
		// Пустой список = все разрешены.
		set := make(map[domain.ExchangeID]bool)
		for _, ex := range domain.SupportedExchanges() {
			set[ex] = true
		}
		return set
	}
	set := make(map[domain.ExchangeID]bool, len(exchanges))
	for _, ex := range exchanges {
		set[ex] = true
	}
	return set
}

func minConfidence(a, b domain.ConfidenceLevel) domain.ConfidenceLevel {
	if a < b {
		return a
	}
	return b
}

func minInt64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func maxDec(a, b decimal.Decimal) decimal.Decimal {
	if a.GreaterThan(b) {
		return a
	}
	return b
}

// EligibleCount — helper для метрик: число eligible кандидатов.
func EligibleCount(candidates []Candidate) int {
	n := 0
	for _, c := range candidates {
		if c.Eligible {
			n++
		}
	}
	return n
}

// Top returns up to n eligible candidates sorted by composite score.
func Top(candidates []Candidate, n int) []Candidate {
	var eligible []Candidate
	for _, c := range candidates {
		if c.Eligible {
			eligible = append(eligible, c)
		}
	}
	sort.Slice(eligible, func(i, j int) bool {
		return eligible[i].CompositeScore.GreaterThan(eligible[j].CompositeScore)
	})
	if len(eligible) > n {
		eligible = eligible[:n]
	}
	return eligible
}

// Suppress unused import for now — orderbook referenced indirectly.
var _ = orderbook.CheckDepthSufficient
