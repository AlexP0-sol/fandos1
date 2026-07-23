// Package exchange определяет строгий интерфейс адаптера биржи (раздел 15.1 промпта v2),
// не позволяющий стратегии зависеть от конкретной биржи. Особенности конкретных exchange API
// остаются внутри соответствующих под-пакетов (binance/, bybit/, ...).
//
// Все структуры, которыми оперирует интерфейс, — нормализованные доменные типы
// из internal/domain. Адаптер отвечает за перевод «сырых» форматов биржи ↔ домен.
package exchange

import (
	"context"
	"errors"
	"time"

	"github.com/thecd/fundarbitrage/internal/decimal"
	"github.com/thecd/fundarbitrage/internal/domain"
)

// ExchangeAdapter — единый интерфейс ко всем операциям биржи.
// Реализуется каждым под-пакетом (binance, bybit, mexc, okx, bitget, kucoin, gate)
// и mock-биржей для тестов (раздел 22, Этап 3).
type ExchangeAdapter interface {
	// ID возвращает идентификатор биржи.
	ID() domain.ExchangeID

	// ---------- Публичные REST (раздел 6) ----------

	// GetServerTime — серверное время биржи; для clock-sync (раздел 24) и recvWindow.
	GetServerTime(ctx context.Context) (time.Time, error)

	// GetInstruments — реестр инструментов (Level 1, раздел 7.1).
	GetInstruments(ctx context.Context) ([]domain.CanonicalInstrument, error)

	// GetFunding — realized + predicted funding rate + ConfidenceLevel (раздел 3.2, 6.2).
	GetFunding(ctx context.Context, symbol domain.ExchangeSymbol) (domain.FundingInfo, error)

	// GetTicker — лёгкий тикер (Level 2).
	GetTicker(ctx context.Context, symbol domain.ExchangeSymbol) (domain.Ticker, error)

	// GetOrderBookSnapshot — полный снимок стакана (Level 3).
	GetOrderBookSnapshot(ctx context.Context, symbol domain.ExchangeSymbol, depth int) (domain.OrderBookSnapshot, error)

	// ---------- WebSocket (раздел 7.1, 7.3) ----------

	// SubscribePublic — публичные каналы (ticker/BBO/depth/funding). Возвращает канал
	// событий; coalescing/backpressure — ответственность вызывающего (marketdata package).
	SubscribePublic(ctx context.Context, subscriptions []PublicSubscription) (<-chan PublicEvent, error)

	// SubscribePrivate — приватные каналы (orders/fills/positions/balance/funding).
	// Приватные события терять нельзя; переполнение → reconciliation (раздел 7.3).
	SubscribePrivate(ctx context.Context, credentials domain.CredentialRef) (<-chan PrivateEvent, error)

	// ---------- Приватные REST (раздел 15.1) ----------

	GetBalances(ctx context.Context) ([]domain.Balance, error)
	GetPositions(ctx context.Context) ([]domain.Position, error)
	GetOpenOrders(ctx context.Context, symbol domain.ExchangeSymbol) ([]domain.Order, error)

	SetLeverage(ctx context.Context, req domain.SetLeverageRequest) error
	SetMarginMode(ctx context.Context, req domain.SetMarginModeRequest) error
	SetPositionMode(ctx context.Context, req domain.SetPositionModeRequest) error

	// PlaceOrder размещает ордер с clientOrderId (idempotency). При таймауте ack
	// вызывающий обязан вызвать GetOrder (QUERY_THEN_DECIDE), а не переотправлять.
	PlaceOrder(ctx context.Context, req domain.PlaceOrderRequest) (domain.OrderAck, error)
	CancelOrder(ctx context.Context, req domain.CancelOrderRequest) error
	GetOrder(ctx context.Context, req domain.OrderQuery) (domain.Order, error)

	// GetADLState — индикатор ADL, если биржа публикует (раздел 23.2).
	GetADLState(ctx context.Context, symbol domain.ExchangeSymbol) (domain.ADLState, error)

	// ---------- Трансферы (раздел 12) ----------

	InternalTransfer(ctx context.Context, req domain.InternalTransferRequest) (domain.TransferResult, error)
	Withdraw(ctx context.Context, req domain.WithdrawalRequest) (domain.WithdrawalResult, error)
	GetWithdrawalHistory(ctx context.Context, query domain.TransferQuery) ([]domain.Withdrawal, error)
	GetDepositHistory(ctx context.Context, query domain.TransferQuery) ([]domain.Deposit, error)
	GetNetworkInfo(ctx context.Context, asset string) ([]domain.NetworkInfo, error)
}

// Note: все торговые типы берутся из internal/domain; decimal — из internal/decimal.

// ============================================================
// Subscription / Event типы для WS
// ============================================================

// PublicSubscription — запрос на подписку на публичный канал.
type PublicSubscription struct {
	Channel Channel // тип канала
	Symbol  domain.ExchangeSymbol
}

// Channel — публичный WS-канал.
type Channel string

const (
	ChannelTicker    Channel = "ticker"
	ChannelBBO       Channel = "bbo"
	ChannelDepth     Channel = "depth"
	ChannelMarkPrice Channel = "mark_price"
	ChannelFunding   Channel = "funding"
)

// PublicEvent — нормализованное публичное WS-событие.
type PublicEvent struct {
	Channel    Channel
	Symbol     domain.ExchangeSymbol
	Ticker     *domain.Ticker
	OrderBook  *domain.OrderBookSnapshot // может быть snapshot или incremental
	Funding    *domain.FundingInfo
	MarkPrice  *domain.Ticker // используем Ticker как контейнер для цены
	ExchangeTS time.Time
	ReceivedAt time.Time
}

// PrivateEvent — нормализованное приватное WS-событие.
// Приватные события нельзя терять; вызывающий обязан обрабатывать упорядоченно
// и сверять с REST при сомнении (раздел 7.3).
type PrivateEvent struct {
	Kind       PrivateEventKind
	Order      *domain.Order
	Fill       *Fill
	Position   *domain.Position
	Balance    *domain.Balance
	Funding    *FundingPayment
	ExchangeTS time.Time
	ReceivedAt time.Time
}

// PrivateEventKind — тип приватного события.
type PrivateEventKind string

const (
	PrivateEventOrder    PrivateEventKind = "order"
	PrivateEventFill     PrivateEventKind = "fill"
	PrivateEventPosition PrivateEventKind = "position"
	PrivateEventBalance  PrivateEventKind = "balance"
	PrivateEventFunding  PrivateEventKind = "funding"
)

// Fill — исполнение ордера (trade).
type Fill struct {
	ExchangeOrderID string
	ClientOrderID   domain.ClientOrderID
	Symbol          domain.ExchangeSymbol
	Side            domain.Side
	BaseQty         decimal.Decimal
	Price           decimal.Decimal
	Fee             decimal.Decimal
	FeeAsset        string
	IsMaker         bool
	Timestamp       time.Time
}

// FundingPayment — фактически начисленный funding (подтверждение, раздел 3.2).
type FundingPayment struct {
	Symbol      domain.ExchangeSymbol
	Amount      decimal.Decimal // со знаком: + получено, - уплачено
	Rate        decimal.Decimal
	Notional    decimal.Decimal
	FundingTime time.Time
}

// ============================================================
// Стандартные ошибки адаптеров
// ============================================================

// Стандартные sentinel-ошибки, общие для всех адаптеров.
var (
	// ErrInvalidSymbol — символ не существует на бирже.
	ErrInvalidSymbol = errors.New("exchange: invalid symbol")
	// ErrInsufficientMargin — недостаточно маржи.
	ErrInsufficientMargin = errors.New("exchange: insufficient margin")
	// ErrRateLimited — превышен rate limit.
	ErrRateLimited = errors.New("exchange: rate limited")
	// ErrUnauthorized — проблема с учётными данными.
	ErrUnauthorized = errors.New("exchange: unauthorized")
	// ErrTimeout — таймаут запроса (включая ack timeout).
	ErrTimeout = errors.New("exchange: timeout")
	// ErrOrderNotFound — ордер не найден при GetOrder (QUERY_THEN_DECIDE path).
	ErrOrderNotFound = errors.New("exchange: order not found")
	// ErrNetwork — сетевая ошибка (retryable).
	ErrNetwork = errors.New("exchange: network error")
	// ErrWithdrawalSuspended — вывод отключён на стороне биржи (раздел 26).
	ErrWithdrawalSuspended = errors.New("exchange: withdrawal suspended")
)
