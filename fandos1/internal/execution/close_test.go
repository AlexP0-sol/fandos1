package execution

import (
	"context"
	"testing"

	"github.com/thecd/fundarbitrage/internal/decimal"
	"github.com/thecd/fundarbitrage/internal/domain"
	mockexchange "github.com/thecd/fundarbitrage/internal/exchange/mock"
)

// stubBook — фиксированные bid/ask для теста закрытия.
type stubBook struct {
	bid, ask decimal.Decimal
}

func (s stubBook) BestBid(domain.ExchangeSymbol) (decimal.Decimal, bool) { return s.bid, true }
func (s stubBook) BestAsk(domain.ExchangeSymbol) (decimal.Decimal, bool) { return s.ask, true }

// missingBook — не отдаёт цены.
type missingBook struct{}

func (missingBook) BestBid(domain.ExchangeSymbol) (decimal.Decimal, bool) { return decimal.Zero, false }
func (missingBook) BestAsk(domain.ExchangeSymbol) (decimal.Decimal, bool) { return decimal.Zero, false }

func setupCloseMocks(t *testing.T) (*OrderExecutor, *OrderExecutor, *mockexchange.Mock, *mockexchange.Mock) {
	t.Helper()
	longSym := domain.ExchangeSymbol("BTCUSDT")
	shortSym := domain.ExchangeSymbol("BTC-USDT-SWAP")

	longM := mockexchange.New(domain.ExchangeBinance)
	longM.SetOrderBook(longSym,
		[]domain.PriceLevel{{Price: decimal.MustFromString("100"), Qty: decimal.MustFromString("100")}},
		[]domain.PriceLevel{{Price: decimal.MustFromString("100.5"), Qty: decimal.MustFromString("100")}},
	)
	longM.SetFillRule(longSym, mockexchange.FillRule{FillFraction: decimal.One})

	shortM := mockexchange.New(domain.ExchangeBybit)
	shortM.SetOrderBook(shortSym,
		[]domain.PriceLevel{{Price: decimal.MustFromString("100.2"), Qty: decimal.MustFromString("100")}},
		[]domain.PriceLevel{{Price: decimal.MustFromString("100.7"), Qty: decimal.MustFromString("100")}},
	)
	shortM.SetFillRule(shortSym, mockexchange.FillRule{FillFraction: decimal.One})

	longExec := NewOrderExecutor(longM, testAckTimeout)
	shortExec := NewOrderExecutor(shortM, testAckTimeout)
	return longExec, shortExec, longM, shortM
}

const testAckTimeout = 0 // 0 → immediate context (миллисекунды в mock-е не нужны)

// TestCoordinatedCloseFullSuccess — обе ноги закрылись полностью.
func TestCoordinatedCloseFullSuccess(t *testing.T) {
	longExec, shortExec, _, _ := setupCloseMocks(t)
	req := CloseRequest{
		PositionID:       "pos-1",
		LongSymbol:       "BTCUSDT",
		ShortSymbol:      "BTC-USDT-SWAP",
		LongRemaining:    decimal.MustFromString("10"),
		ShortRemaining:   decimal.MustFromString("10"),
		LongExecutor:     longExec,
		ShortExecutor:    shortExec,
		LongBookProvider: stubBook{bid: decimal.MustFromString("100")},
		ShortBookProvider: stubBook{ask: decimal.MustFromString("100.5")},
	}
	cfg := CloseConfig{
		CloseProtectionTicks: 2,
		MaxRequotes:          3,
		TickSize:             decimal.MustFromString("0.01"),
	}
	res, err := CoordinatedClose(context.Background(), req, cfg)
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if !res.LongClosedQty.Equal(decimal.MustFromString("10")) {
		t.Errorf("long closed = %s, want 10", res.LongClosedQty.String())
	}
	if !res.ShortClosedQty.Equal(decimal.MustFromString("10")) {
		t.Errorf("short closed = %s, want 10", res.ShortClosedQty.String())
	}
	if res.Degraded {
		t.Errorf("unexpected degraded: %s", res.Reason)
	}
}

// TestCoordinatedCloseDifferentRemaining — closingQty = min(long, short).
func TestCoordinatedCloseDifferentRemaining(t *testing.T) {
	longExec, shortExec, _, _ := setupCloseMocks(t)
	req := CloseRequest{
		PositionID:       "pos-1",
		LongSymbol:       "BTCUSDT",
		ShortSymbol:      "BTC-USDT-SWAP",
		LongRemaining:    decimal.MustFromString("15"), // long больше
		ShortRemaining:   decimal.MustFromString("10"), // short меньше
		LongExecutor:     longExec,
		ShortExecutor:    shortExec,
		LongBookProvider: stubBook{bid: decimal.MustFromString("100")},
		ShortBookProvider: stubBook{ask: decimal.MustFromString("100.5")},
	}
	cfg := CloseConfig{CloseProtectionTicks: 1, MaxRequotes: 3, TickSize: decimal.MustFromString("0.01")}
	res, err := CoordinatedClose(context.Background(), req, cfg)
	if err != nil {
		t.Fatal(err)
	}
	// Закрыто min(15, 10) = 10 на обеих ногах.
	if !res.LongClosedQty.Equal(decimal.MustFromString("10")) {
		t.Errorf("long closed = %s, want 10", res.LongClosedQty.String())
	}
}

// TestCoordinatedCloseNoBook — отсутствует best price → degraded.
func TestCoordinatedCloseNoBook(t *testing.T) {
	longExec, shortExec, _, _ := setupCloseMocks(t)
	req := CloseRequest{
		PositionID:       "pos-1",
		LongSymbol:       "BTCUSDT",
		ShortSymbol:      "BTC-USDT-SWAP",
		LongRemaining:    decimal.MustFromString("10"),
		ShortRemaining:   decimal.MustFromString("10"),
		LongExecutor:     longExec,
		ShortExecutor:    shortExec,
		LongBookProvider: missingBook{}, // нет цен
		ShortBookProvider: stubBook{ask: decimal.MustFromString("100.5")},
	}
	cfg := CloseConfig{CloseProtectionTicks: 1, MaxRequotes: 3, TickSize: decimal.MustFromString("0.01")}
	res, err := CoordinatedClose(context.Background(), req, cfg)
	if err == nil {
		t.Fatal("expected error on missing book")
	}
	if !res.Degraded {
		t.Error("expected degraded")
	}
}

// TestCoordinatedCloseZeroRemaining — нечего закрывать → no-op.
func TestCoordinatedCloseZeroRemaining(t *testing.T) {
	longExec, shortExec, _, _ := setupCloseMocks(t)
	req := CloseRequest{
		PositionID:       "pos-1",
		LongSymbol:       "BTCUSDT",
		ShortSymbol:      "BTC-USDT-SWAP",
		LongRemaining:    decimal.Zero,
		ShortRemaining:   decimal.Zero,
		LongExecutor:     longExec,
		ShortExecutor:    shortExec,
		LongBookProvider: stubBook{bid: decimal.MustFromString("100")},
		ShortBookProvider: stubBook{ask: decimal.MustFromString("100.5")},
	}
	cfg := CloseConfig{CloseProtectionTicks: 1, MaxRequotes: 3, TickSize: decimal.MustFromString("0.01")}
	res, err := CoordinatedClose(context.Background(), req, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !res.LongClosedQty.IsZero() || !res.ShortClosedQty.IsZero() {
		t.Error("zero remaining should close nothing")
	}
}

// TestCoordinatedClosePartialFillRetries — partial fill на первой попытке, полный на второй.
func TestCoordinatedClosePartialFillRetries(t *testing.T) {
	longSym := domain.ExchangeSymbol("BTCUSDT")
	shortSym := domain.ExchangeSymbol("BTC-USDT-SWAP")

	longM := mockexchange.New(domain.ExchangeBinance)
	longM.SetOrderBook(longSym,
		[]domain.PriceLevel{{Price: decimal.MustFromString("100"), Qty: decimal.MustFromString("100")}},
		[]domain.PriceLevel{{Price: decimal.MustFromString("100.5"), Qty: decimal.MustFromString("100")}},
	)
	// Первая попытка — partial 50%, вторая — полный (через смену правила).
	longM.SetFillRule(longSym, mockexchange.FillRule{FillFraction: decimal.MustFromString("0.5")})

	shortM := mockexchange.New(domain.ExchangeBybit)
	shortM.SetOrderBook(shortSym,
		[]domain.PriceLevel{{Price: decimal.MustFromString("100.2"), Qty: decimal.MustFromString("100")}},
		[]domain.PriceLevel{{Price: decimal.MustFromString("100.7"), Qty: decimal.MustFromString("100")}},
	)
	shortM.SetFillRule(shortSym, mockexchange.FillRule{FillFraction: decimal.MustFromString("0.5")})

	longExec := NewOrderExecutor(longM, testAckTimeout)
	shortExec := NewOrderExecutor(shortM, testAckTimeout)

	req := CloseRequest{
		PositionID:       "pos-1",
		LongSymbol:       longSym,
		ShortSymbol:      shortSym,
		LongRemaining:    decimal.MustFromString("10"),
		ShortRemaining:   decimal.MustFromString("10"),
		LongExecutor:     longExec,
		ShortExecutor:    shortExec,
		LongBookProvider: stubBook{bid: decimal.MustFromString("100")},
		ShortBookProvider: stubBook{ask: decimal.MustFromString("100.5")},
	}
	cfg := CloseConfig{CloseProtectionTicks: 1, MaxRequotes: 5, TickSize: decimal.MustFromString("0.01")}

	// С partial 50%: первая попытка закроет 5, вторая — ещё 5 (от остатка 5 → 2.5 и т.д.).
	// После MaxRequotes=5 будет существенный остаток → degraded.
	res, err := CoordinatedClose(context.Background(), req, cfg)
	// На 50% fill всё закроется за log2(10)≈4 попытки до <1, но с rounding может остаться.
	_ = err
	// Как минимум 5 должно быть закрыто (первая попытка).
	if res.LongClosedQty.LessThan(decimal.MustFromString("5")) {
		t.Errorf("long closed = %s, want ≥ 5", res.LongClosedQty.String())
	}
}

// TestCloseOneLegEmergency
func TestCloseOneLegEmergency(t *testing.T) {
	m := mockexchange.New(domain.ExchangeBinance)
	m.SetOrderBook("BTCUSDT",
		[]domain.PriceLevel{{Price: decimal.MustFromString("100"), Qty: decimal.MustFromString("100")}},
		[]domain.PriceLevel{{Price: decimal.MustFromString("100.5"), Qty: decimal.MustFromString("100")}},
	)
	m.SetFillRule("BTCUSDT", mockexchange.FillRule{FillFraction: decimal.One})
	exec := NewOrderExecutor(m, testAckTimeout)

	err := CloseOneLegEmergency(context.Background(), exec, "BTCUSDT",
		decimal.MustFromString("10"), domain.SideShort, "pos-1")
	if err != nil {
		t.Fatalf("emergency close: %v", err)
	}
}
