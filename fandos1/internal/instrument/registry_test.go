package instrument

import (
	"testing"
	"time"

	"github.com/thecd/fundarbitrage/internal/decimal"
	"github.com/thecd/fundarbitrage/internal/domain"
)

// helper: строит тестовый инструмент.
func mkIns(ex domain.ExchangeID, asset string, sym string) domain.CanonicalInstrument {
	return domain.CanonicalInstrument{
		Exchange:           ex,
		CanonicalBaseAsset: domain.AssetSymbol(asset),
		ExchangeSymbol:     domain.ExchangeSymbol(sym),
		InstrumentType:     domain.InstrumentLinearUSDTPerpetual,
		SettlementCurrency: "USDT",
		ContractMultiplier: decimal.One,
		QtyStep:            decimal.MustFromString("0.001"),
		MinQty:             decimal.MustFromString("0.001"),
		MinNotional:        decimal.MustFromString("5"),
		TickSize:           decimal.MustFromString("0.01"),
		MaxLeverage:        decimal.MustFromString("20"),
		FundingIntervalSec: 28800,
		FundingPriceType:   domain.FundingPriceMark,
		Status:             domain.InstrumentStatusActive,
	}
}

// TestReplaceAndGet — основная операция: загрузить и искать.
func TestReplaceAndGet(t *testing.T) {
	r := New()
	r.Replace([]domain.CanonicalInstrument{
		mkIns(domain.ExchangeBinance, "BTC", "BTCUSDT"),
		mkIns(domain.ExchangeBybit, "BTC", "BTC-USDT-SWAP"),
		mkIns(domain.ExchangeOKX, "ETH", "ETH-USDT-SWAP"),
	})

	// Прямой lookup.
	got, ok := r.Get(domain.ExchangeBinance, "BTC")
	if !ok || got.ExchangeSymbol != "BTCUSDT" {
		t.Errorf("Binance BTC: got sym=%s ok=%v", got.ExchangeSymbol, ok)
	}

	// LookupSymbol — обратный поиск по биржевому символу.
	got2, ok2 := r.LookupSymbol(domain.ExchangeBybit, "BTC-USDT-SWAP")
	if !ok2 || got2.CanonicalBaseAsset != "BTC" {
		t.Errorf("Bybit BTC-USDT-SWAP → asset: got=%s ok=%v", got2.CanonicalBaseAsset, ok2)
	}

	// SymbolFor — альтернатива конкатенации (раздел 6.1).
	sym, ok3 := r.SymbolFor(domain.ExchangeOKX, "ETH")
	if !ok3 || sym != "ETH-USDT-SWAP" {
		t.Errorf("OKX ETH symbol: got=%s ok=%v", sym, ok3)
	}

	// Несуществующее.
	if _, ok := r.Get(domain.ExchangeMEXC, "BTC"); ok {
		t.Error("MEXC BTC should not exist")
	}
	if _, ok := r.LookupSymbol(domain.ExchangeBinance, "NOPE"); ok {
		t.Error("NOPE should not resolve")
	}
}

// TestReplaceClearsPrevious — повторный Replace полностью замещает данные.
func TestReplaceClearsPrevious(t *testing.T) {
	r := New()
	r.Replace([]domain.CanonicalInstrument{mkIns(domain.ExchangeBinance, "BTC", "BTCUSDT")})
	if _, ok := r.Get(domain.ExchangeBinance, "BTC"); !ok {
		t.Fatal("expected BTC after first Replace")
	}

	r.Replace([]domain.CanonicalInstrument{mkIns(domain.ExchangeBinance, "ETH", "ETHUSDT")})
	// BTC теперь должен исчезнуть.
	if _, ok := r.Get(domain.ExchangeBinance, "BTC"); ok {
		t.Error("BTC should be cleared after Replace")
	}
	if _, ok := r.Get(domain.ExchangeBinance, "ETH"); !ok {
		t.Error("ETH should exist after Replace")
	}
}

// TestNonPerpFiltered — spot/inverse инструменты отбрасываются (раздел 1.1).
func TestNonPerpFiltered(t *testing.T) {
	r := New()
	spot := mkIns(domain.ExchangeBinance, "BTC", "BTCUSDT")
	spot.InstrumentType = "SPOT"
	r.Replace([]domain.CanonicalInstrument{spot})

	if _, ok := r.Get(domain.ExchangeBinance, "BTC"); ok {
		t.Error("SPOT instrument must be filtered out")
	}
}

// TestInvalidExchangeFiltered — биржа с невалидным ID отбрасывается.
func TestInvalidExchangeFiltered(t *testing.T) {
	r := New()
	bad := mkIns(domain.ExchangeID("bogus"), "BTC", "BTCUSDT")
	r.Replace([]domain.CanonicalInstrument{bad})

	if _, ok := r.Get(domain.ExchangeID("bogus"), "BTC"); ok {
		t.Error("invalid exchange must be filtered out")
	}
}

// TestInstrumentsForAsset — все инструменты одного актива (для построения пар).
func TestInstrumentsForAsset(t *testing.T) {
	r := New()
	r.Replace([]domain.CanonicalInstrument{
		mkIns(domain.ExchangeBinance, "BTC", "BTCUSDT"),
		mkIns(domain.ExchangeBybit, "BTC", "BTC-USDT-SWAP"),
		mkIns(domain.ExchangeOKX, "BTC", "BTC-USDT-SWAP"),
		mkIns(domain.ExchangeBinance, "ETH", "ETHUSDT"),
	})

	btcs := r.InstrumentsForAsset("BTC")
	if len(btcs) != 3 {
		t.Errorf("BTC on %d exchanges, want 3", len(btcs))
	}
	// Должен вернуть копию, не внутренний слайс.
	btcs[0] = domain.CanonicalInstrument{}
	again := r.InstrumentsForAsset("BTC")
	if len(again) != 3 {
		t.Error("mutation of returned slice affected registry")
	}
}

// TestAssetsByExchange — итерация по бирже.
func TestAssetsByExchange(t *testing.T) {
	r := New()
	r.Replace([]domain.CanonicalInstrument{
		mkIns(domain.ExchangeBinance, "BTC", "BTCUSDT"),
		mkIns(domain.ExchangeBinance, "ETH", "ETHUSDT"),
		mkIns(domain.ExchangeBybit, "BTC", "BTC-USDT-SWAP"),
	})

	list := r.AssetsByExchange(domain.ExchangeBinance)
	if len(list) != 2 {
		t.Errorf("Binance has %d, want 2", len(list))
	}
	empty := r.AssetsByExchange(domain.ExchangeGate)
	if len(empty) != 0 {
		t.Errorf("Gate should be empty, got %d", len(empty))
	}
}

// TestStats — корректность сводной статистики.
func TestStats(t *testing.T) {
	r := New()
	r.Replace([]domain.CanonicalInstrument{
		mkIns(domain.ExchangeBinance, "BTC", "BTCUSDT"),
		mkIns(domain.ExchangeBybit, "BTC", "BTC-USDT-SWAP"),
		mkIns(domain.ExchangeBinance, "ETH", "ETHUSDT"),
	})
	s := r.Stats()
	if s.TotalInstruments != 3 {
		t.Errorf("total=%d, want 3", s.TotalInstruments)
	}
	if s.ByExchange[domain.ExchangeBinance] != 2 {
		t.Errorf("Binance count=%d, want 2", s.ByExchange[domain.ExchangeBinance])
	}
	if s.ByAsset["BTC"] != 2 {
		t.Errorf("BTC count=%d, want 2", s.ByAsset["BTC"])
	}
}

// TestLastRefresh — обновляется при Replace.
func TestLastRefresh(t *testing.T) {
	r := New()
	if !r.LastRefresh().IsZero() {
		t.Error("fresh registry should have zero LastRefresh")
	}
	r.Replace([]domain.CanonicalInstrument{mkIns(domain.ExchangeBinance, "BTC", "BTCUSDT")})
	if r.LastRefresh().IsZero() {
		t.Error("LastRefresh should be set after Replace")
	}
	if time.Since(r.LastRefresh()) > 5*time.Second {
		t.Error("LastRefresh is stale")
	}
}

// TestSymbolForRejectsConcatenation — символ нельзя получить для незарегистрированного актива
// даже если "выглядит как" конкатенация (раздел 6.1: только через реестр).
func TestSymbolForRejectsConcatenation(t *testing.T) {
	r := New()
	r.Replace([]domain.CanonicalInstrument{mkIns(domain.ExchangeOKX, "BTC", "BTC-USDT-SWAP")})

	// OKX BTC даёт "BTC-USDT-SWAP", а не "BTCUSDT".
	sym, ok := r.SymbolFor(domain.ExchangeOKX, "BTC")
	if !ok {
		t.Fatal("expected OKX BTC")
	}
	if sym == "BTCUSDT" {
		t.Error("OKX symbol must not be naive concatenation BTCUSDT")
	}
	if sym != "BTC-USDT-SWAP" {
		t.Errorf("OKX symbol=%s, want BTC-USDT-SWAP", sym)
	}
}
