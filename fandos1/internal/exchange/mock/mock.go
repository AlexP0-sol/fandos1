// Package mock реализует программируемый mock-адаптер биржи для тестов (раздел 18.3).
// Позволяет сценарию заранее задать реакции (полное исполнение, partial fill,
// one-leg rejection, таймаут ack, WS-дубликаты, out-of-order, rate limit, ADL и т.д.).
//
// Mock НЕ выполняет реальной сетевой работы; используется в unit/integration-тестах
// scanner, strategy, execution, risk, recovery (раздел 18).
package mock

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/thecd/fundarbitrage/internal/decimal"
	"github.com/thecd/fundarbitrage/internal/domain"
	"github.com/thecd/fundarbitrage/internal/exchange"
)

// Mock — программируемая биржа.
type Mock struct {
	id domain.ExchangeID

	mu       sync.Mutex
	instruments []domain.CanonicalInstrument
	books      map[domain.ExchangeSymbol]*orderBookState
	orders     map[string]*domain.Order // keyed by client order id
	fillRules  map[domain.ExchangeSymbol]FillRule
	positions  []domain.Position
	balances   []domain.Balance
	adlStates  map[domain.ExchangeSymbol]domain.ADLState
	serverTime time.Time

	// Поведенческие флаги
	ackTimeoutSymbols map[domain.ExchangeSymbol]bool // символы, чьи PlaceOrder не отвечают (ack timeout)
	rejectNext        int                              // счётчик: отклонить следующие N order placements
	networkErrors     int                              // сетевые ошибки до успеха
	latency           time.Duration                    // искусственная задержка ответов
	withdrawSuspended bool
	rateLimited       bool

	// WS
	publicCh  chan exchange.PublicEvent
	privateCh chan exchange.PrivateEvent
	publicSubs  []exchange.PublicSubscription
}

// New создаёт mock биржу с заданным id.
func New(id domain.ExchangeID) *Mock {
	return &Mock{
		id:                id,
		books:             make(map[domain.ExchangeSymbol]*orderBookState),
		orders:            make(map[string]*domain.Order),
		fillRules:         make(map[domain.ExchangeSymbol]FillRule),
		adlStates:         make(map[domain.ExchangeSymbol]domain.ADLState),
		ackTimeoutSymbols: make(map[domain.ExchangeSymbol]bool),
		serverTime:        time.Unix(1700000000, 0).UTC(),
	}
}

func (m *Mock) ID() domain.ExchangeID { return m.id }

// orderBookState — внутреннее состояние стакана mock.
type orderBookState struct {
	bids []domain.PriceLevel
	asks []domain.PriceLevel
	seq  int64
}

// FillRule описывает, как mock исполняет ордера по символу.
type FillRule struct {
	// FillFraction ∈ [0,1]: какая доля BaseQty исполняется немедленно.
	// 1 = полное, 0.5 = half fill, 0 = no fill (остаётся в книге или отвергается).
	FillFraction decimal.Decimal
	// Reject: если true, ордер отвергается сразу (one-leg rejection scenario).
	Reject bool
}

// ============================================================
// Конфигурация сценария (вызывается тестом)
// ============================================================

// SetInstruments устанавливает реестр инструментов.
func (m *Mock) SetInstruments(ins []domain.CanonicalInstrument) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.instruments = ins
}

// SetOrderBook устанавливает стакан для символа (Level 3 depth).
func (m *Mock) SetOrderBook(sym domain.ExchangeSymbol, bids, asks []domain.PriceLevel) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.books[sym] = &orderBookState{bids: bids, asks: asks, seq: 1}
}

// SetFillRule задаёт, как исполняются ордера по символу.
func (m *Mock) SetFillRule(sym domain.ExchangeSymbol, rule FillRule) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.fillRules[sym] = rule
}

// SetPositions / SetBalances / SetADL — initial state.
func (m *Mock) SetPositions(p []domain.Position) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.positions = p
}
func (m *Mock) SetBalances(b []domain.Balance) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.balances = b
}
func (m *Mock) SetADL(sym domain.ExchangeSymbol, s domain.ADLState) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.adlStates[sym] = s
}

// AckTimeoutFor — PlaceOrder по этому символу не ответит (моделирует ack timeout
// для проверки QUERY_THEN_DECIDE, раздел 5.3, 10.2).
func (m *Mock) AckTimeoutFor(sym domain.ExchangeSymbol) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ackTimeoutSymbols[sym] = true
}

// WithdrawalSuspended — Withdraw вернёт ErrWithdrawalSuspended (раздел 26).
func (m *Mock) WithdrawalSuspended(v bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.withdrawSuspended = v
}

// SetRateLimited — все запросы возвращают ErrRateLimited.
func (m *Mock) SetRateLimited(v bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rateLimited = v
}

// ============================================================
// ExchangeAdapter реализация
// ============================================================

func (m *Mock) GetServerTime(ctx context.Context) (time.Time, error) {
	m.sleepLatency()
	return m.serverTime, nil
}

func (m *Mock) GetInstruments(ctx context.Context) ([]domain.CanonicalInstrument, error) {
	if err := m.guard(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]domain.CanonicalInstrument, len(m.instruments))
	copy(out, m.instruments)
	return out, nil
}

func (m *Mock) GetFunding(ctx context.Context, sym domain.ExchangeSymbol) (domain.FundingInfo, error) {
	if err := m.guard(); err != nil {
		return domain.FundingInfo{}, err
	}
	return domain.FundingInfo{
		ExchangeSymbol:       sym,
		PredictedFundingRate: decimal.Zero,
		RealizedFundingRate:  decimal.Zero,
		Confidence:           domain.ConfidenceHigh,
		FundingPriceType:     domain.FundingPriceMark,
		FundingIntervalSec:   28800, // 8h
	}, nil
}

func (m *Mock) GetTicker(ctx context.Context, sym domain.ExchangeSymbol) (domain.Ticker, error) {
	if err := m.guard(); err != nil {
		return domain.Ticker{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	t := domain.Ticker{Symbol: sym, Timestamp: m.serverTime}
	if b, ok := m.books[sym]; ok {
		if len(b.bids) > 0 {
			t.LastPrice = b.bids[0].Price
		}
	}
	return t, nil
}

func (m *Mock) GetOrderBookSnapshot(ctx context.Context, sym domain.ExchangeSymbol, depth int) (domain.OrderBookSnapshot, error) {
	if err := m.guard(); err != nil {
		return domain.OrderBookSnapshot{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.books[sym]
	if !ok {
		return domain.OrderBookSnapshot{}, exchange.ErrInvalidSymbol
	}
	return domain.OrderBookSnapshot{
		Exchange:   m.id,
		Symbol:     sym,
		Bids:       cloneLevels(b.bids, depth),
		Asks:       cloneLevels(b.asks, depth),
		Timestamp:  m.serverTime,
		Sequence:   b.seq,
		IsSnapshot: true,
	}, nil
}

func (m *Mock) SubscribePublic(ctx context.Context, subs []exchange.PublicSubscription) (<-chan exchange.PublicEvent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.publicSubs = subs
	m.publicCh = make(chan exchange.PublicEvent, 128)
	return m.publicCh, nil
}

func (m *Mock) SubscribePrivate(ctx context.Context, cred domain.CredentialRef) (<-chan exchange.PrivateEvent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.privateCh = make(chan exchange.PrivateEvent, 128)
	return m.privateCh, nil
}

// EmitPublic / EmitPrivate — тест впрыскивает события в подписки.
func (m *Mock) EmitPublic(ev exchange.PublicEvent) {
	m.mu.Lock()
	ch := m.publicCh
	m.mu.Unlock()
	if ch != nil {
		select {
		case ch <- ev:
		default:
		}
	}
}
func (m *Mock) EmitPrivate(ev exchange.PrivateEvent) {
	m.mu.Lock()
	ch := m.privateCh
	m.mu.Unlock()
	if ch != nil {
		select {
		case ch <- ev:
		default:
		}
	}
}

func (m *Mock) GetBalances(ctx context.Context) ([]domain.Balance, error) {
	if err := m.guard(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]domain.Balance, len(m.balances))
	copy(out, m.balances)
	return out, nil
}

func (m *Mock) GetPositions(ctx context.Context) ([]domain.Position, error) {
	if err := m.guard(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]domain.Position, len(m.positions))
	copy(out, m.positions)
	return out, nil
}

func (m *Mock) GetOpenOrders(ctx context.Context, sym domain.ExchangeSymbol) ([]domain.Order, error) {
	if err := m.guard(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []domain.Order
	for _, o := range m.orders {
		if o.Symbol == sym && !o.Status.IsTerminal() {
			out = append(out, *o)
		}
	}
	return out, nil
}

func (m *Mock) SetLeverage(ctx context.Context, req domain.SetLeverageRequest) error {
	return m.guard()
}
func (m *Mock) SetMarginMode(ctx context.Context, req domain.SetMarginModeRequest) error {
	return m.guard()
}
func (m *Mock) SetPositionMode(ctx context.Context, req domain.SetPositionModeRequest) error {
	return m.guard()
}

// PlaceOrder — основная логика исполнения mock.
//
// ВАЖНО для ack-timeout модели (раздел 5.3, 10.2): ордер создаётся ВСЕГДА,
// даже когда ack «затерялся». Реальная биржа при таймауте ответа могла уже принять
// ордер, поэтому GetOrder обязан его находить — это и есть основа QUERY_THEN_DECIDE:
// восстановить состояние через query, а не слепо переотправлять.
func (m *Mock) PlaceOrder(ctx context.Context, req domain.PlaceOrderRequest) (domain.OrderAck, error) {
	if err := m.guard(); err != nil {
		return domain.OrderAck{}, err
	}

	m.mu.Lock()
	rule, hasRule := m.fillRules[req.Symbol]
	ackTimeout := m.ackTimeoutSymbols[req.Symbol]
	m.mu.Unlock()

	// Ордер создаётся ВСЕГДА (даже при reject/ack-timeout), чтобы GetOrder его нашёл.
	o := &domain.Order{
		ClientOrderID:    req.ClientOrderID,
		Symbol:           req.Symbol,
		Side:             req.Side,
		OrderMode:        req.OrderMode,
		ReduceOnly:       req.ReduceOnly,
		RequestedQty:     req.BaseQty,
		Status:           domain.OrderStatusNew,
		ExchangeTimestamp: m.serverTime,
		ExchangeOrderID:  fmt.Sprintf("%s-%d", req.ClientOrderID, m.serverTime.UnixNano()),
	}

	// Determine fill fraction.
	frac := decimal.One
	if hasRule {
		frac = rule.FillFraction
	}
	o.FilledQty = req.BaseQty.Mul(frac)
	if req.Price.GreaterThan(decimal.Zero) {
		o.AvgFillPrice = req.Price
	} else if b, ok := m.bookFor(req.Symbol); ok {
		if req.Side == domain.SideLong && len(b.asks) > 0 {
			o.AvgFillPrice = b.asks[0].Price
		} else if req.Side == domain.SideShort && len(b.bids) > 0 {
			o.AvgFillPrice = b.bids[0].Price
		}
	}

	// Reject scenario: ордер создан, но отвергнут.
	if hasRule && rule.Reject {
		o.Status = domain.OrderStatusRejected
	} else if o.FilledQty.IsZero() {
		o.Status = domain.OrderStatusNew
	} else if o.FilledQty.GreaterThanOrEqual(req.BaseQty) {
		o.Status = domain.OrderStatusFilled
	} else {
		o.Status = domain.OrderStatusPartiallyFilled
	}

	m.mu.Lock()
	m.orders[string(req.ClientOrderID)] = o
	m.mu.Unlock()

	// Ack timeout: ордер на бирже ЕСТЬ, но ack не дошёл. Возвращаем ErrTimeout,
	// чтобы вызывающий пошёл в GetOrder.
	if ackTimeout {
		return domain.OrderAck{}, exchange.ErrTimeout
	}

	return domain.OrderAck{
		ExchangeOrderID: o.ExchangeOrderID,
		ClientOrderID:   req.ClientOrderID,
		Status:          o.Status,
		Timestamp:       m.serverTime,
	}, nil
}

func (m *Mock) CancelOrder(ctx context.Context, req domain.CancelOrderRequest) error {
	if err := m.guard(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if o, ok := m.orders[string(req.ClientOrderID)]; ok {
		if !o.Status.IsTerminal() {
			o.Status = domain.OrderStatusCancelled
		}
	}
	return nil
}

// GetOrder — критичен для QUERY_THEN_DECIDE (раздел 10.2).
// Возвращает актуальное состояние ордера по client order id.
func (m *Mock) GetOrder(ctx context.Context, req domain.OrderQuery) (domain.Order, error) {
	if err := m.guard(); err != nil {
		return domain.Order{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	o, ok := m.orders[string(req.ClientOrderID)]
	if !ok {
		return domain.Order{}, exchange.ErrOrderNotFound
	}
	return *o, nil
}

func (m *Mock) GetADLState(ctx context.Context, sym domain.ExchangeSymbol) (domain.ADLState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.adlStates[sym]
	if !ok {
		return domain.ADLState{Symbol: sym}, nil
	}
	return s, nil
}

func (m *Mock) InternalTransfer(ctx context.Context, req domain.InternalTransferRequest) (domain.TransferResult, error) {
	if err := m.guard(); err != nil {
		return domain.TransferResult{}, err
	}
	return domain.TransferResult{TransferID: "mock-tx", Status: "ok"}, nil
}

func (m *Mock) Withdraw(ctx context.Context, req domain.WithdrawalRequest) (domain.WithdrawalResult, error) {
	if err := m.guard(); err != nil {
		return domain.WithdrawalResult{}, err
	}
	m.mu.Lock()
	suspended := m.withdrawSuspended
	m.mu.Unlock()
	if suspended {
		return domain.WithdrawalResult{}, exchange.ErrWithdrawalSuspended
	}
	return domain.WithdrawalResult{WithdrawalID: "mock-wd", TxID: "mock-txid", Status: "ok"}, nil
}

func (m *Mock) GetWithdrawalHistory(ctx context.Context, q domain.TransferQuery) ([]domain.Withdrawal, error) {
	return nil, nil
}
func (m *Mock) GetDepositHistory(ctx context.Context, q domain.TransferQuery) ([]domain.Deposit, error) {
	return nil, nil
}
func (m *Mock) GetNetworkInfo(ctx context.Context, asset string) ([]domain.NetworkInfo, error) {
	return []domain.NetworkInfo{
		{Network: "TRX", WithdrawEnabled: true, DepositEnabled: true, WithdrawFee: decimal.MustFromString("1")},
	}, nil
}

// ============================================================
// Helpers
// ============================================================

// guard проверяет rate-limit/network флаги.
func (m *Mock) guard() error {
	m.sleepLatency()
	m.mu.Lock()
	rl := m.rateLimited
	m.mu.Unlock()
	if rl {
		return exchange.ErrRateLimited
	}
	return nil
}

func (m *Mock) sleepLatency() {
	if m.latency > 0 {
		time.Sleep(m.latency)
	}
}

func (m *Mock) bookFor(sym domain.ExchangeSymbol) (*orderBookState, bool) {
	b, ok := m.books[sym]
	return b, ok
}

func cloneLevels(src []domain.PriceLevel, depth int) []domain.PriceLevel {
	if depth > 0 && depth < len(src) {
		src = src[:depth]
	}
	out := make([]domain.PriceLevel, len(src))
	copy(out, src)
	return out
}

// SetServerTime — для тестов с clock skew.
func (m *Mock) SetServerTime(t time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.serverTime = t
}

// SetLatency — искусственная задержка ответов.
func (m *Mock) SetLatency(d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.latency = d
}

// OrderByClient — инспекция состояния ордера в тестах.
func (m *Mock) OrderByClient(id domain.ClientOrderID) (domain.Order, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	o, ok := m.orders[string(id)]
	if !ok {
		return domain.Order{}, false
	}
	return *o, true
}

// Ensure Mock реализует интерфейс во время компиляции.
var _ exchange.ExchangeAdapter = (*Mock)(nil)
