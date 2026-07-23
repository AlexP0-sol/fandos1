package mock

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/thecd/fundarbitrage/internal/decimal"
	"github.com/thecd/fundarbitrage/internal/domain"
	"github.com/thecd/fundarbitrage/internal/exchange"
)

const testSym = domain.ExchangeSymbol("BTCUSDT")

func setupBook(m *Mock) {
	m.SetOrderBook(testSym,
		[]domain.PriceLevel{
			{Price: decimal.MustFromString("100.00"), Qty: decimal.MustFromString("10")},
			{Price: decimal.MustFromString("99.00"), Qty: decimal.MustFromString("20")},
		},
		[]domain.PriceLevel{
			{Price: decimal.MustFromString("100.50"), Qty: decimal.MustFromString("10")},
			{Price: decimal.MustFromString("101.00"), Qty: decimal.MustFromString("20")},
		},
	)
}

// TestFullFill — ордер полностью исполняется.
func TestFullFill(t *testing.T) {
	m := New(domain.ExchangeBinance)
	setupBook(m)
	m.SetFillRule(testSym, FillRule{FillFraction: decimal.One})

	ack, err := m.PlaceOrder(context.Background(), domain.PlaceOrderRequest{
		ClientOrderID: "c1",
		Symbol:        testSym,
		Side:          domain.SideLong,
		OrderMode:     domain.OrderMarketableLimitIOC,
		BaseQty:       decimal.MustFromString("5"),
		Price:         decimal.MustFromString("100.50"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if ack.Status != domain.OrderStatusFilled {
		t.Errorf("status = %s, want filled", ack.Status)
	}
	o, ok := m.OrderByClient("c1")
	if !ok {
		t.Fatal("order not found")
	}
	if !o.FilledQty.Equal(decimal.MustFromString("5")) {
		t.Errorf("filled = %s, want 5", o.FilledQty.String())
	}
	if !o.AvgFillPrice.Equal(decimal.MustFromString("100.50")) {
		t.Errorf("price = %s, want 100.50", o.AvgFillPrice.String())
	}
}

// TestDefaultFullFill — при отсутствии FillRule ордер исполняется полностью по умолчанию.
func TestDefaultFullFill(t *testing.T) {
	m := New(domain.ExchangeBinance)
	setupBook(m)
	// FillRule не задан → frac = One по умолчанию.

	ack, err := m.PlaceOrder(context.Background(), domain.PlaceOrderRequest{
		ClientOrderID: "c-default",
		Symbol:        testSym,
		Side:          domain.SideLong,
		BaseQty:       decimal.MustFromString("3"),
		Price:         decimal.MustFromString("100.50"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if ack.Status != domain.OrderStatusFilled {
		t.Errorf("default fill status = %s, want filled", ack.Status)
	}
	o, ok := m.OrderByClient("c-default")
	if !ok {
		t.Fatal("order not found")
	}
	if !o.FilledQty.Equal(decimal.MustFromString("3")) {
		t.Errorf("default filled = %s, want 3", o.FilledQty.String())
	}
}

// TestPartialFill — частичное исполнение (50%).
// Сценарий 60/50-типа (раздел 10.3).
func TestPartialFill(t *testing.T) {
	m := New(domain.ExchangeBinance)
	setupBook(m)
	m.SetFillRule(testSym, FillRule{FillFraction: decimal.MustFromString("0.5")})

	_, err := m.PlaceOrder(context.Background(), domain.PlaceOrderRequest{
		ClientOrderID: "c2",
		Symbol:        testSym,
		Side:          domain.SideLong,
		BaseQty:       decimal.MustFromString("10"),
		Price:         decimal.MustFromString("100.50"),
	})
	if err != nil {
		t.Fatal(err)
	}
	o, _ := m.OrderByClient("c2")
	if o.Status != domain.OrderStatusPartiallyFilled {
		t.Errorf("status = %s, want partially_filled", o.Status)
	}
	if !o.FilledQty.Equal(decimal.MustFromString("5")) {
		t.Errorf("filled = %s, want 5", o.FilledQty.String())
	}
}

// TestReject — one-leg rejection через FillRule (раздел 18.3).
func TestReject(t *testing.T) {
	m := New(domain.ExchangeBinance)
	setupBook(m)
	m.SetFillRule(testSym, FillRule{Reject: true})

	ack, err := m.PlaceOrder(context.Background(), domain.PlaceOrderRequest{
		ClientOrderID: "c3",
		Symbol:        testSym,
		Side:          domain.SideLong,
		BaseQty:       decimal.MustFromString("5"),
		Price:         decimal.MustFromString("100.50"),
	})
	if err != nil {
		t.Fatalf("place should not error on reject: %v", err)
	}
	if ack.Status != domain.OrderStatusRejected {
		t.Errorf("status = %s, want rejected", ack.Status)
	}
}

// TestSetRejectNext — SetRejectNext декрементирует счётчик, возвращает reject в ack.
func TestSetRejectNext(t *testing.T) {
	m := New(domain.ExchangeBinance)
	setupBook(m)
	m.SetRejectNext(2)

	// Первые два вызова — reject.
	for i := 0; i < 2; i++ {
		id := domain.ClientOrderID(fmt.Sprintf("rn-%d", i))
		ack, err := m.PlaceOrder(context.Background(), domain.PlaceOrderRequest{
			ClientOrderID: id,
			Symbol:        testSym,
			Side:          domain.SideLong,
			BaseQty:       decimal.MustFromString("1"),
			Price:         decimal.MustFromString("100.50"),
		})
		if err != nil {
			t.Fatalf("call %d: unexpected error: %v", i, err)
		}
		if ack.Status != domain.OrderStatusRejected {
			t.Errorf("call %d: status = %s, want rejected", i, ack.Status)
		}
	}

	// Третий вызов — успех (счётчик исчерпан).
	ack, err := m.PlaceOrder(context.Background(), domain.PlaceOrderRequest{
		ClientOrderID: "rn-ok",
		Symbol:        testSym,
		Side:          domain.SideLong,
		BaseQty:       decimal.MustFromString("1"),
		Price:         decimal.MustFromString("100.50"),
	})
	if err != nil {
		t.Fatalf("third call should succeed: %v", err)
	}
	if ack.Status != domain.OrderStatusFilled {
		t.Errorf("third call status = %s, want filled", ack.Status)
	}
}

// TestSetNetworkErrors — SetNetworkErrors декрементирует счётчик, возвращает ErrNetwork.
func TestSetNetworkErrors(t *testing.T) {
	m := New(domain.ExchangeBinance)
	setupBook(m)
	m.SetNetworkErrors(2)

	// Первые два вызова — ErrNetwork.
	for i := 0; i < 2; i++ {
		id := domain.ClientOrderID(fmt.Sprintf("ne-%d", i))
		_, err := m.PlaceOrder(context.Background(), domain.PlaceOrderRequest{
			ClientOrderID: id,
			Symbol:        testSym,
			Side:          domain.SideLong,
			BaseQty:       decimal.MustFromString("1"),
			Price:         decimal.MustFromString("100.50"),
		})
		if !errors.Is(err, exchange.ErrNetwork) {
			t.Errorf("call %d: expected ErrNetwork, got %v", i, err)
		}
	}

	// Третий вызов — успех.
	ack, err := m.PlaceOrder(context.Background(), domain.PlaceOrderRequest{
		ClientOrderID: "ne-ok",
		Symbol:        testSym,
		Side:          domain.SideLong,
		BaseQty:       decimal.MustFromString("1"),
		Price:         decimal.MustFromString("100.50"),
	})
	if err != nil {
		t.Fatalf("third call after network errors should succeed: %v", err)
	}
	if ack.Status != domain.OrderStatusFilled {
		t.Errorf("third call status = %s, want filled", ack.Status)
	}
}

// TestAckTimeoutThenQueryThenDecide — критичный путь (раздел 5.3, 10.2):
// PlaceOrder таймаутит, но GetOrder находит созданный ордер.
// Это доказывает, что QUERY_THEN_DECIDE восстанавливает состояние без слепой повторной отправки.
func TestAckTimeoutThenQueryThenDecide(t *testing.T) {
	m := New(domain.ExchangeBinance)
	setupBook(m)
	m.SetFillRule(testSym, FillRule{FillFraction: decimal.One})
	m.AckTimeoutFor(testSym)

	_, err := m.PlaceOrder(context.Background(), domain.PlaceOrderRequest{
		ClientOrderID: "c4",
		Symbol:        testSym,
		Side:          domain.SideLong,
		BaseQty:       decimal.MustFromString("5"),
		Price:         decimal.MustFromString("100.50"),
	})
	if !errors.Is(err, exchange.ErrTimeout) {
		t.Fatalf("expected ErrTimeout, got %v", err)
	}

	// QUERY: ордер существует несмотря на таймаут ack — состояние восстановлено.
	// При FillFraction=1 ордер уже filled на стороне биржи (ack просто затерялся).
	o, err := m.GetOrder(context.Background(), domain.OrderQuery{ClientOrderID: "c4"})
	if err != nil {
		t.Fatalf("GetOrder after timeout should find order: %v", err)
	}
	if o.ClientOrderID != "c4" {
		t.Errorf("client id = %s, want c4", o.ClientOrderID)
	}
	if o.Status != domain.OrderStatusFilled {
		t.Errorf("status = %s, want filled (order executed despite lost ack)", o.Status)
	}
	if !o.FilledQty.Equal(decimal.MustFromString("5")) {
		t.Errorf("filled = %s, want 5", o.FilledQty.String())
	}
}

// TestGetOrderNotFound — запрос несуществующего даёт ErrOrderNotFound.
func TestGetOrderNotFound(t *testing.T) {
	m := New(domain.ExchangeBinance)
	_, err := m.GetOrder(context.Background(), domain.OrderQuery{ClientOrderID: "nope"})
	if !errors.Is(err, exchange.ErrOrderNotFound) {
		t.Errorf("expected ErrOrderNotFound, got %v", err)
	}
}

// TestGetOrderRateLimited — GetOrder при rate-limit возвращает ErrRateLimited
// (критично для QUERY_THEN_DECIDE — caller должен уметь обработать этот случай).
func TestGetOrderRateLimited(t *testing.T) {
	m := New(domain.ExchangeBinance)
	setupBook(m)
	// Сначала создаём ордер.
	_, _ = m.PlaceOrder(context.Background(), domain.PlaceOrderRequest{
		ClientOrderID: "rl-order",
		Symbol:        testSym,
		Side:          domain.SideLong,
		BaseQty:       decimal.MustFromString("1"),
		Price:         decimal.MustFromString("100.50"),
	})
	// Включаем rate limit.
	m.SetRateLimited(true)
	_, err := m.GetOrder(context.Background(), domain.OrderQuery{ClientOrderID: "rl-order"})
	if !errors.Is(err, exchange.ErrRateLimited) {
		t.Errorf("GetOrder under rate-limit: expected ErrRateLimited, got %v", err)
	}
}

// TestCancelOrder — отмена переводит в cancelled.
func TestCancelOrder(t *testing.T) {
	m := New(domain.ExchangeBinance)
	setupBook(m)
	m.SetFillRule(testSym, FillRule{FillFraction: decimal.Zero})

	_, _ = m.PlaceOrder(context.Background(), domain.PlaceOrderRequest{
		ClientOrderID: "c5",
		Symbol:        testSym,
		Side:          domain.SideLong,
		BaseQty:       decimal.MustFromString("5"),
		Price:         decimal.MustFromString("100.50"),
	})
	if err := m.CancelOrder(context.Background(), domain.CancelOrderRequest{ClientOrderID: "c5"}); err != nil {
		t.Fatal(err)
	}
	o, _ := m.OrderByClient("c5")
	if o.Status != domain.OrderStatusCancelled {
		t.Errorf("status = %s, want cancelled", o.Status)
	}
	// Двойная отмена не меняет терминальный статус.
	_ = m.CancelOrder(context.Background(), domain.CancelOrderRequest{ClientOrderID: "c5"})
	o2, _ := m.OrderByClient("c5")
	if o2.Status != domain.OrderStatusCancelled {
		t.Errorf("double cancel changed terminal status to %s", o2.Status)
	}
}

// TestWithdrawSuspended — моделирует suspended wallet (раздел 26).
func TestWithdrawSuspended(t *testing.T) {
	m := New(domain.ExchangeBinance)
	m.WithdrawalSuspended(true)
	_, err := m.Withdraw(context.Background(), domain.WithdrawalRequest{
		Asset: "USDT", Amount: decimal.MustFromString("100"), Network: "TRX", Address: "Txxx",
	})
	if !errors.Is(err, exchange.ErrWithdrawalSuspended) {
		t.Errorf("expected ErrWithdrawalSuspended, got %v", err)
	}
}

// TestRateLimited — все запросы возвращают ErrRateLimited.
func TestRateLimited(t *testing.T) {
	m := New(domain.ExchangeBinance)
	m.SetRateLimited(true)
	if _, err := m.GetInstruments(context.Background()); !errors.Is(err, exchange.ErrRateLimited) {
		t.Errorf("instruments: expected rate limited, got %v", err)
	}
	if _, err := m.GetBalances(context.Background()); !errors.Is(err, exchange.ErrRateLimited) {
		t.Errorf("balances: expected rate limited, got %v", err)
	}
}

// TestOrderBookDepthClamp — depth ограничивает возвращаемые уровни.
func TestOrderBookDepthClamp(t *testing.T) {
	m := New(domain.ExchangeBinance)
	m.SetOrderBook(testSym,
		[]domain.PriceLevel{{Price: decimal.FromInt(1)}, {Price: decimal.FromInt(2)}, {Price: decimal.FromInt(3)}},
		[]domain.PriceLevel{{Price: decimal.FromInt(4)}, {Price: decimal.FromInt(5)}, {Price: decimal.FromInt(6)}},
	)
	ob, err := m.GetOrderBookSnapshot(context.Background(), testSym, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(ob.Bids) != 2 || len(ob.Asks) != 2 {
		t.Errorf("depth clamp: bids=%d asks=%d, want 2/2", len(ob.Bids), len(ob.Asks))
	}
}

// TestInvalidSymbol — запрос несуществующего стакана даёт ErrInvalidSymbol.
func TestInvalidSymbol(t *testing.T) {
	m := New(domain.ExchangeBinance)
	_, err := m.GetOrderBookSnapshot(context.Background(), "NOPE", 5)
	if !errors.Is(err, exchange.ErrInvalidSymbol) {
		t.Errorf("expected ErrInvalidSymbol, got %v", err)
	}
}

// TestWSInject — события впрыскиваются в подписки (раздел 18.3).
func TestWSInject(t *testing.T) {
	m := New(domain.ExchangeBinance)
	pubCh, err := m.SubscribePublic(context.Background(), []exchange.PublicSubscription{{Channel: exchange.ChannelTicker, Symbol: testSym}})
	if err != nil {
		t.Fatal(err)
	}
	privCh, err := m.SubscribePrivate(context.Background(), domain.CredentialRef{Exchange: domain.ExchangeBinance, Kind: "trade"})
	if err != nil {
		t.Fatal(err)
	}

	m.EmitPublic(exchange.PublicEvent{Channel: exchange.ChannelTicker, Symbol: testSym, ExchangeTS: time.Now(), ReceivedAt: time.Now()})
	m.EmitPrivate(exchange.PrivateEvent{Kind: exchange.PrivateEventOrder, ExchangeTS: time.Now(), ReceivedAt: time.Now()})

	select {
	case <-pubCh:
	default:
		t.Fatal("public event not received")
	}
	select {
	case <-privCh:
	default:
		t.Fatal("private event not received")
	}
}

// TestDoubleSubscribePublic — повторный SubscribePublic закрывает старый канал.
func TestDoubleSubscribePublic(t *testing.T) {
	m := New(domain.ExchangeBinance)
	ch1, err := m.SubscribePublic(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	// Второй SubscribePublic должен закрыть ch1.
	_, err = m.SubscribePublic(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	// ch1 должен быть закрыт — чтение вернёт zero value, ok=false.
	select {
	case _, ok := <-ch1:
		if ok {
			t.Error("old public channel should be closed after re-subscribe")
		}
	default:
		t.Error("old public channel should be closed (readable as closed)")
	}
}

// TestDoubleSubscribePrivate — повторный SubscribePrivate закрывает старый канал.
func TestDoubleSubscribePrivate(t *testing.T) {
	m := New(domain.ExchangeBinance)
	cred := domain.CredentialRef{Exchange: domain.ExchangeBinance, Kind: "trade"}
	ch1, err := m.SubscribePrivate(context.Background(), cred)
	if err != nil {
		t.Fatal(err)
	}
	_, err = m.SubscribePrivate(context.Background(), cred)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case _, ok := <-ch1:
		if ok {
			t.Error("old private channel should be closed after re-subscribe")
		}
	default:
		t.Error("old private channel should be closed (readable as closed)")
	}
}

// TestADLState — возвращает заданное ADL-состояние (раздел 23).
func TestADLState(t *testing.T) {
	m := New(domain.ExchangeBinance)
	m.SetADL(testSym, domain.ADLState{
		Symbol:     testSym,
		LongQueue:  decimal.MustFromString("0.2"),
		ShortQueue: decimal.MustFromString("0.8"),
	})
	st, err := m.GetADLState(context.Background(), testSym)
	if err != nil {
		t.Fatal(err)
	}
	if !st.ShortQueue.Equal(decimal.MustFromString("0.8")) {
		t.Errorf("short queue = %s, want 0.8", st.ShortQueue.String())
	}
}

// TestMarketablePriceFromBook — MARKET-ордер берёт цену из стакана.
func TestMarketablePriceFromBook(t *testing.T) {
	m := New(domain.ExchangeBinance)
	setupBook(m)
	m.SetFillRule(testSym, FillRule{FillFraction: decimal.One})

	// long без явной цены → берёт ask 100.50
	_, err := m.PlaceOrder(context.Background(), domain.PlaceOrderRequest{
		ClientOrderID: "c6",
		Symbol:        testSym,
		Side:          domain.SideLong,
		OrderMode:     domain.OrderMarket,
		BaseQty:       decimal.MustFromString("2"),
	})
	if err != nil {
		t.Fatal(err)
	}
	o, _ := m.OrderByClient("c6")
	if !o.AvgFillPrice.Equal(decimal.MustFromString("100.50")) {
		t.Errorf("long market price = %s, want 100.50", o.AvgFillPrice.String())
	}

	// short без явной цены → берёт bid 100.00
	_, err = m.PlaceOrder(context.Background(), domain.PlaceOrderRequest{
		ClientOrderID: "c7",
		Symbol:        testSym,
		Side:          domain.SideShort,
		OrderMode:     domain.OrderMarket,
		BaseQty:       decimal.MustFromString("2"),
	})
	if err != nil {
		t.Fatal(err)
	}
	o2, _ := m.OrderByClient("c7")
	if !o2.AvgFillPrice.Equal(decimal.MustFromString("100.00")) {
		t.Errorf("short market price = %s, want 100.00", o2.AvgFillPrice.String())
	}
}

// TestConcurrentPlaceOrder — параллельные PlaceOrder не вызывают гонок.
func TestConcurrentPlaceOrder(t *testing.T) {
	m := New(domain.ExchangeBinance)
	setupBook(m)

	var wg sync.WaitGroup
	const n = 20
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := domain.ClientOrderID(fmt.Sprintf("concurrent-%d", i))
			m.PlaceOrder(context.Background(), domain.PlaceOrderRequest{
				ClientOrderID: id,
				Symbol:        testSym,
				Side:          domain.SideLong,
				BaseQty:       decimal.MustFromString("1"),
				Price:         decimal.MustFromString("100.50"),
			})
		}(i)
	}
	wg.Wait()
}

// TestImplementsInterface — гарантия на уровне компиляции уже есть (var _),
// но дублируем runtime-проверкой для уверенности при рефакторинге.
func TestImplementsInterface(t *testing.T) {
	var _ exchange.ExchangeAdapter = New(domain.ExchangeBybit)
}
