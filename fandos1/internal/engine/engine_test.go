package engine

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thecd/fundarbitrage/internal/decimal"
	"github.com/thecd/fundarbitrage/internal/domain"
	"github.com/thecd/fundarbitrage/internal/exchange"
	mockexchange "github.com/thecd/fundarbitrage/internal/exchange/mock"
	"github.com/thecd/fundarbitrage/internal/execution"
	"github.com/thecd/fundarbitrage/internal/instrument"
	"github.com/thecd/fundarbitrage/internal/marketdata"
	"github.com/thecd/fundarbitrage/internal/portfolio"
	"github.com/thecd/fundarbitrage/internal/scanner"
)

// memPersister — in-memory Persister: движку не нужна БД для unit-тестов.
type memPersister struct {
	mu     sync.Mutex
	events []portfolio.Transition
}

func (m *memPersister) OnTransition(_ *portfolio.Position, t portfolio.Transition) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = append(m.events, t)
	return nil
}

// stubHalter — управляемый SAFE_HALT.
type stubHalter struct{ halted bool }

func (s stubHalter) IsHalted() (bool, string) { return s.halted, "test" }

// testRig — полный стенд: два mock-адаптера, реестр, кэш, сканер, движок.
type testRig struct {
	engine    *Engine
	binance   *mockexchange.Mock
	bybit     *mockexchange.Mock
	cache     *marketdata.Cache
	registry  *instrument.Registry
	persister *memPersister
	clock     time.Time
}

func d(s string) decimal.Decimal { return decimal.MustFromString(s) }

func instrumentFor(ex domain.ExchangeID, asset string) domain.CanonicalInstrument {
	return domain.CanonicalInstrument{
		Exchange:           ex,
		ExchangeSymbol:     domain.ExchangeSymbol(asset + "USDT"),
		CanonicalBaseAsset: domain.AssetSymbol(asset),
		InstrumentType:     domain.InstrumentLinearUSDTPerpetual,
		SettlementCurrency: "USDT",
		Status:             domain.InstrumentStatusActive,
		ContractMultiplier: decimal.One,
		QtyStep:            d("0.001"),
		MinQty:             d("0.001"),
		TickSize:           d("0.1"),
		FundingIntervalSec: 8 * 3600,
	}
}

// snapshotFor — рыночный снимок с заданными ценой и funding rate.
func snapshotFor(ex domain.ExchangeID, asset string, px, rate decimal.Decimal, now time.Time) *domain.MarketSnapshot {
	spread := px.Mul(d("0.0002"))
	return &domain.MarketSnapshot{
		Exchange:             ex,
		CanonicalBaseAsset:   domain.AssetSymbol(asset),
		ExchangeSymbol:       domain.ExchangeSymbol(asset + "USDT"),
		BestBid:              px.Sub(spread),
		BestAsk:              px.Add(spread),
		MarkPrice:            px,
		LastPrice:            px,
		QuoteVolume24h:       decimal.FromInt(5_000_000),
		BidDepthForTargetQty: decimal.FromInt(1_000_000),
		AskDepthForTargetQty: decimal.FromInt(1_000_000),
		PredictedFundingRate: rate,
		FundingIntervalSec:   8 * 3600,
		NextFundingTime:      now.Add(90 * time.Minute),
		FundingConfidence:    domain.ConfidenceHigh,
		IsFresh:              true,
		SequenceValid:        true,
		LocalReceiveTime:     now,
	}
}

// newRig — стенд с положительным funding-спредом (bybit платит short-у больше).
func newRig(t *testing.T, ordersEnabled bool) *testRig {
	t.Helper()
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

	bin := mockexchange.New(domain.ExchangeBinance)
	byb := mockexchange.New(domain.ExchangeBybit)
	for _, m := range []*mockexchange.Mock{bin, byb} {
		sym := domain.ExchangeSymbol("BTCUSDT")
		m.SetOrderBook(sym,
			[]domain.PriceLevel{{Price: d("49990"), Qty: d("100")}},
			[]domain.PriceLevel{{Price: d("50010"), Qty: d("100")}},
		)
		m.SetFillRule(sym, mockexchange.FillRule{FillFraction: decimal.One})
	}
	bin.SetInstruments([]domain.CanonicalInstrument{instrumentFor(domain.ExchangeBinance, "BTC")})
	byb.SetInstruments([]domain.CanonicalInstrument{instrumentFor(domain.ExchangeBybit, "BTC")})

	registry := instrument.New()
	registry.Replace([]domain.CanonicalInstrument{
		instrumentFor(domain.ExchangeBinance, "BTC"),
		instrumentFor(domain.ExchangeBybit, "BTC"),
	})

	cache := marketdata.New()
	// binance rate 0.0001, bybit 0.0005 → long binance / short bybit выгодно.
	cache.Update(snapshotFor(domain.ExchangeBinance, "BTC", d("50000"), d("0.0001"), now))
	cache.Update(snapshotFor(domain.ExchangeBybit, "BTC", d("50030"), d("0.0005"), now))

	persister := &memPersister{}
	adapters := map[domain.ExchangeID]exchange.ExchangeAdapter{
		domain.ExchangeBinance: bin, domain.ExchangeBybit: byb,
	}
	executors := map[domain.ExchangeID]*execution.OrderExecutor{
		domain.ExchangeBinance: execution.NewOrderExecutor(bin, 0),
		domain.ExchangeBybit:   execution.NewOrderExecutor(byb, 0),
	}

	eng, err := New(Deps{
		Adapters:  adapters,
		Executors: executors,
		Registry:  registry,
		Cache:     cache,
		Scanner:   scanner.New(registry, cache),
		Positions: nil, // Restore в unit-тестах не используется
		Persister: persister,
		Orders:    nil, // персист ордеров выключен в unit-тестах
		Halter:    stubHalter{},
		Log:       testLogger(),
		Clock:     func() time.Time { return now },
	}, Config{
		Scan: scanner.Config{
			MinQuoteVolume24h:       decimal.FromInt(1000),
			MinOrderBookDepthUSDT:   decimal.FromInt(100),
			MaxDataAgeMs:            60_000,
			MinConfidenceLevel:      domain.ConfidenceLow,
			MinSecondsBeforeFunding: 60,
			MinExpectedNetPnL:       d("0.5"),
			TargetQty:               d("0.05"),
			FeeRateBps:              d("5"),
			Horizon:                 24 * time.Hour,
		},
		MaxOpenPositions:         1,
		OrdersEnabled:            ordersEnabled,
		ProtectionTicks:          2,
		MaxRequotes:              3,
		DeltaToleranceBase:       d("0.0005"),
		ExitIfFundingSignChanges: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &testRig{engine: eng, binance: bin, bybit: byb, cache: cache,
		registry: registry, persister: persister, clock: now}
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// singleOpen — снапшот единственной активной позиции.
func (r *testRig) singleOpen(t *testing.T) portfolio.Snapshot {
	t.Helper()
	r.engine.mu.Lock()
	defer r.engine.mu.Unlock()
	if len(r.engine.open) != 1 {
		t.Fatalf("open positions = %d, want 1", len(r.engine.open))
	}
	for _, op := range r.engine.open {
		return op.pos.Snapshot()
	}
	panic("unreachable")
}

func TestOpenHappyPath(t *testing.T) {
	rig := newRig(t, true)
	rig.engine.Tick(context.Background())

	snap := rig.singleOpen(t)
	if snap.State != portfolio.StateMonitoring {
		t.Fatalf("state = %s, want MONITORING", snap.State)
	}
	if !snap.LongBaseQty.Equal(d("0.05")) || !snap.ShortBaseQty.Equal(d("0.05")) {
		t.Errorf("qty long=%s short=%s, want 0.05/0.05", snap.LongBaseQty, snap.ShortBaseQty)
	}
	if snap.LongExchange != domain.ExchangeBinance || snap.ShortExchange != domain.ExchangeBybit {
		t.Errorf("legs %s/%s, want binance/bybit (funding spread)", snap.LongExchange, snap.ShortExchange)
	}
	if !snap.DeltaBase.IsZero() {
		t.Errorf("delta = %s, want 0", snap.DeltaBase)
	}
}

func TestSecondTickDoesNotDuplicate(t *testing.T) {
	rig := newRig(t, true)
	rig.engine.Tick(context.Background())
	rig.engine.Tick(context.Background())
	if n := rig.engine.OpenCount(); n != 1 {
		t.Fatalf("open = %d, want 1 (MaxOpenPositions=1)", n)
	}
}

func TestObserveModeDoesNotOpen(t *testing.T) {
	rig := newRig(t, false)
	rig.engine.Tick(context.Background())
	if n := rig.engine.OpenCount(); n != 0 {
		t.Fatalf("observe mode must not open, got %d", n)
	}
}

func TestHaltedTickIsNoop(t *testing.T) {
	rig := newRig(t, true)
	rig.engine.deps.Halter = stubHalter{halted: true}
	rig.engine.Tick(context.Background())
	if n := rig.engine.OpenCount(); n != 0 {
		t.Fatalf("halted engine must not open, got %d", n)
	}
}

func TestManualCloseFullCycle(t *testing.T) {
	rig := newRig(t, true)
	ctx := context.Background()
	rig.engine.Tick(ctx)
	snap := rig.singleOpen(t)

	rig.engine.RequestClose(string(snap.ID), "operator test close")
	rig.engine.Tick(ctx)

	// Исходная позиция закрыта и забыта движком. (В авторежиме движок вправе
	// сразу открыть НОВУЮ позицию по всё ещё выгодному кандидату — это норма.)
	rig.engine.mu.Lock()
	_, stillOpen := rig.engine.open[snap.ID]
	_, reqPending := rig.engine.closeReq[snap.ID]
	rig.engine.mu.Unlock()
	if stillOpen {
		t.Fatal("closed position must be forgotten by engine")
	}
	if reqPending {
		t.Fatal("close request must be cleared")
	}
	// Проверяем полный путь переходов в persister.
	var states []string
	for _, ev := range rig.persister.events {
		states = append(states, string(ev.To))
	}
	joined := strings.Join(states, ",")
	for _, want := range []string{"MONITORING", "EXIT_REQUESTED", "EXITING", "RECONCILING", "CLOSED"} {
		if !strings.Contains(joined, want) {
			t.Errorf("transition %s missing in %s", want, joined)
		}
	}
}

func TestFundingFlipTriggersClose(t *testing.T) {
	rig := newRig(t, true)
	ctx := context.Background()
	rig.engine.Tick(ctx)
	snap := rig.singleOpen(t)

	// Спред переворачивается: bybit теперь платит МЕНЬШЕ binance.
	rig.cache.Update(snapshotFor(domain.ExchangeBinance, "BTC", d("50000"), d("0.0005"), rig.clock))
	rig.cache.Update(snapshotFor(domain.ExchangeBybit, "BTC", d("50030"), d("0.0001"), rig.clock))

	rig.engine.Tick(ctx)
	// Исходная позиция закрыта; повторное открытие невозможно — спред невыгоден.
	rig.engine.mu.Lock()
	_, stillOpen := rig.engine.open[snap.ID]
	total := len(rig.engine.open)
	rig.engine.mu.Unlock()
	if stillOpen || total != 0 {
		t.Fatalf("flipped funding must close position (still=%v total=%d)", stillOpen, total)
	}
}

// TestAckTimeoutRecoveredViaQuery — QUERY_THEN_DECIDE сквозь весь движок:
// биржа не прислала ack, но query нашёл исполненный ордер → позиция открыта штатно.
func TestAckTimeoutRecoveredViaQuery(t *testing.T) {
	rig := newRig(t, true)
	rig.bybit.AckTimeoutFor("BTCUSDT")

	rig.engine.Tick(context.Background())
	snap := rig.singleOpen(t)
	if snap.State != portfolio.StateMonitoring {
		t.Fatalf("state = %s, want MONITORING (query recovered the order)", snap.State)
	}
	if !snap.LongBaseQty.Equal(snap.ShortBaseQty) || !snap.LongBaseQty.IsPositive() {
		t.Fatalf("recovered legs must be balanced: long=%s short=%s", snap.LongBaseQty, snap.ShortBaseQty)
	}
}

// deadAdapter — обёртка: place И query падают сетевой ошибкой →
// состояние ноги действительно НЕИЗВЕСТНО (настоящий ambiguous).
type deadAdapter struct {
	exchange.ExchangeAdapter
}

func (d deadAdapter) PlaceOrder(context.Context, domain.PlaceOrderRequest) (domain.OrderAck, error) {
	return domain.OrderAck{}, exchange.ErrNetwork
}

func (d deadAdapter) GetOrder(context.Context, domain.OrderQuery) (domain.Order, error) {
	return domain.Order{}, exchange.ErrNetwork
}

func TestAmbiguousEntryGoesDegraded(t *testing.T) {
	rig := newRig(t, true)
	// Short-нога: и place, и query — сетевой сбой → исход неизвестен.
	rig.engine.deps.Executors[domain.ExchangeBybit] =
		execution.NewOrderExecutor(deadAdapter{rig.bybit}, 0)

	rig.engine.Tick(context.Background())
	snap := rig.singleOpen(t)
	if snap.State != portfolio.StateDegraded {
		t.Fatalf("state = %s, want DEGRADED (ambiguous entry)", snap.State)
	}
}

func TestPartialShortRepairsToCommon(t *testing.T) {
	rig := newRig(t, true)
	// Short-нога всегда исполняется наполовину: первичный вход 50%,
	// добор 50% от shortfall → останется излишек на long → reduce до общего уровня.
	rig.bybit.SetFillRule("BTCUSDT", mockexchange.FillRule{FillFraction: d("0.5")})

	rig.engine.Tick(context.Background())
	snap := rig.singleOpen(t)

	if !snap.LongBaseQty.Equal(snap.ShortBaseQty) {
		t.Fatalf("legs unbalanced after repair: long=%s short=%s",
			snap.LongBaseQty, snap.ShortBaseQty)
	}
	if !snap.LongBaseQty.IsPositive() {
		t.Fatal("common qty must be positive")
	}
	if snap.State != portfolio.StateMonitoring {
		t.Fatalf("state = %s, want MONITORING after successful repair", snap.State)
	}
	if !snap.DeltaBase.IsZero() {
		t.Errorf("delta = %s, want 0", snap.DeltaBase)
	}
}
