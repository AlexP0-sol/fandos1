// Доменные типы рыночных данных и торговли (раздел 6 промпта v2).
// Нормализованные структуры: особенности конкретных бирж остаются внутри адаптеров.
package domain

import (
	"time"

	"github.com/thecd/fundarbitrage/internal/decimal"
)

// ============================================================
// Instrument (раздел 6.1)
// ============================================================

// CanonicalInstrument — нормализованное описание одного инструмента на бирже.
// В v1 поддерживается только LINEAR_USDT_PERPETUAL.
type CanonicalInstrument struct {
	Exchange           ExchangeID
	CanonicalBaseAsset AssetSymbol
	ExchangeSymbol     ExchangeSymbol
	InstrumentType     InstrumentType // всегда LINEAR_USDT_PERPETUAL в v1
	SettlementCurrency string         // USDT
	ContractMultiplier decimal.Decimal
	QtyStep            decimal.Decimal
	MinQty             decimal.Decimal
	MinNotional        decimal.Decimal
	TickSize           decimal.Decimal
	MaxLeverage        decimal.Decimal
	MaxMarketOrderQty  decimal.Decimal
	PositionLimit      decimal.Decimal
	FundingIntervalSec int64
	FundingPriceType   FundingPriceType
	SupportsADL        bool
	Status             InstrumentStatus
}

// ============================================================
// MarketSnapshot (раздел 6.2)
// ============================================================

// MarketSnapshot — нормализованное состояние рынка одного инструмента в момент.
// Включает realized + predicted funding rate, ConfidenceLevel, ADL queue.
type MarketSnapshot struct {
	Exchange           ExchangeID
	CanonicalBaseAsset AssetSymbol
	ExchangeSymbol     ExchangeSymbol

	BestBid    decimal.Decimal
	BestAsk    decimal.Decimal
	MarkPrice  decimal.Decimal
	IndexPrice decimal.Decimal
	LastPrice  decimal.Decimal

	QuoteVolume24h       decimal.Decimal
	OpenInterest         decimal.Decimal
	BidDepthForTargetQty decimal.Decimal
	AskDepthForTargetQty decimal.Decimal

	RealizedFundingRate  decimal.Decimal
	PredictedFundingRate decimal.Decimal
	FundingRateCap       *decimal.Decimal
	FundingRateFloor     *decimal.Decimal
	FundingIntervalSec   int64
	NextFundingTime      time.Time
	FundingConfidence    ConfidenceLevel

	ADLQueuePosition *ADLQueuePosition

	ExchangeTimestamp time.Time
	LocalReceiveTime  time.Time
	SequenceValid     bool
	IsFresh           bool
}

// ADLQueuePosition — позиция в очереди auto-deleveraging (раздел 23.2).
// nil, если биржа не публикует.
type ADLQueuePosition struct {
	// Очередь long/short в относительных единицах [0,1]; 0 = нет риска, 1 = максимум.
	LongQueue  decimal.Decimal
	ShortQueue decimal.Decimal
}

// IsStale — true, если snapshot устарел: либо структурный флаг IsFresh=false,
// либо возраст снимка превышает maxAge (раздел 6.3).
// Порог MaxDataAgeMs передаётся вызывающим для тестируемости.
func (s *MarketSnapshot) IsStale(now time.Time, maxAge time.Duration) bool {
	if !s.IsFresh {
		return true
	}
	return now.Sub(s.LocalReceiveTime) > maxAge
}

// ============================================================
// FundingInfo (раздел 15.1 — явные realized + predicted + ConfidenceLevel)
// ============================================================

// FundingInfo — нормализованная funding-информация одного инструмента.
type FundingInfo struct {
	ExchangeSymbol       ExchangeSymbol
	RealizedFundingRate  decimal.Decimal
	PredictedFundingRate decimal.Decimal
	RateType             FundingRateType
	FundingRateCap       *decimal.Decimal
	FundingRateFloor     *decimal.Decimal
	FundingIntervalSec   int64
	NextFundingTime      time.Time
	Confidence           ConfidenceLevel
	FundingPriceType     FundingPriceType
}

// ============================================================
// OrderBook (раздел 3.3, 7.1)
// ============================================================

// OrderBookSnapshot — снимок стакана на момент запроса/обновления.
type OrderBookSnapshot struct {
	Exchange   ExchangeID
	Symbol     ExchangeSymbol
	Bids       []PriceLevel
	Asks       []PriceLevel
	Timestamp  time.Time
	Sequence   int64 // для sequence-validation (6.3); 0 если биржа не даёт
	IsSnapshot bool  // true = полный снимок, false = incremental update
}

// PriceLevel — один уровень стакана.
type PriceLevel struct {
	Price decimal.Decimal
	Qty   decimal.Decimal
}

// BestBid/BestAsk — helper, безопасный для пустого стакана.
func (o *OrderBookSnapshot) BestBid() (decimal.Decimal, bool) {
	if len(o.Bids) == 0 {
		return decimal.Zero, false
	}
	return o.Bids[0].Price, true
}

// BestAsk — helper.
func (o *OrderBookSnapshot) BestAsk() (decimal.Decimal, bool) {
	if len(o.Asks) == 0 {
		return decimal.Zero, false
	}
	return o.Asks[0].Price, true
}

// ============================================================
// Ticker (раздел 6.2)
// ============================================================

// Ticker — лёгкий all-market тикер для Level 2 (раздел 7.1).
type Ticker struct {
	Symbol         ExchangeSymbol
	LastPrice      decimal.Decimal
	MarkPrice      decimal.Decimal
	IndexPrice     decimal.Decimal
	QuoteVolume24h decimal.Decimal
	Timestamp      time.Time
}

// ============================================================
// Balance / Position (раздел 15.1)
// ============================================================

// Balance — баланс на бирже по активу.
type Balance struct {
	Asset            string
	WalletBalance    decimal.Decimal // полный баланс
	AvailableBalance decimal.Decimal // доступная маржа
	// В v1 USDT-focused; cross-margin details оставлены адаптеру.
}

// Position — одна открытая позиция на бирже.
type Position struct {
	Symbol           ExchangeSymbol
	Side             Side
	ContractQty      decimal.Decimal
	BaseQty          decimal.Decimal //abs(ContractQty × multiplier)
	EntryPrice       decimal.Decimal
	MarkPrice        decimal.Decimal
	LiquidationPrice decimal.Decimal
	UnrealizedPnL    decimal.Decimal
	MarginMode       MarginMode
	Leverage         decimal.Decimal
	Margin           decimal.Decimal
	ADLQueue         *ADLQueuePosition
	Updated          time.Time
}

// ============================================================
// Order (раздел 15.1)
// ============================================================

// OrderStatus — жизненный цикл биржевого ордера.
type OrderStatus string

const (
	OrderStatusNew             OrderStatus = "new"
	OrderStatusAcknowledged    OrderStatus = "acknowledged"
	OrderStatusPartiallyFilled OrderStatus = "partially_filled"
	OrderStatusFilled          OrderStatus = "filled"
	OrderStatusCancelled       OrderStatus = "cancelled"
	OrderStatusRejected        OrderStatus = "rejected"
	OrderStatusExpired         OrderStatus = "expired"
	OrderStatusNotFound        OrderStatus = "not_found" // ордер не найден при query (ACK timeout path)
)

// IsTerminal — true, если финальный (дальнейших изменений не будет).
func (s OrderStatus) IsTerminal() bool {
	switch s {
	case OrderStatusFilled, OrderStatusCancelled, OrderStatusRejected, OrderStatusExpired:
		return true
	}
	return false
}

// Order — нормализованный биржевой ордер.
type Order struct {
	ExchangeOrderID   string
	ClientOrderID     ClientOrderID
	Symbol            ExchangeSymbol
	Side              Side
	OrderMode         OrderMode
	ReduceOnly        bool
	RequestedQty      decimal.Decimal // base qty
	FilledQty         decimal.Decimal // base qty
	AvgFillPrice      decimal.Decimal
	Fees              decimal.Decimal
	Status            OrderStatus
	ExchangeTimestamp time.Time
	AckState          AckState // для QUERY_THEN_DECIDE (раздел 5.3)
}

// AckState — результат ack path (раздел 5.3, 10.2).
type AckState string

const (
	AckStateAcked    AckState = "acked"
	AckStateQueried  AckState = "queried" // ack timed out, состояние восстановлено через query
	AckStateTimedOut AckState = "timed_out"
)

// OrderAck — немедленный ответ биржи на PlaceOrder.
type OrderAck struct {
	ExchangeOrderID string
	ClientOrderID   ClientOrderID
	Status          OrderStatus
	Timestamp       time.Time
}

// PlaceOrderRequest — запрос на размещение ордера.
type PlaceOrderRequest struct {
	ClientOrderID ClientOrderID
	Symbol        ExchangeSymbol
	Side          Side
	OrderMode     OrderMode
	BaseQty       decimal.Decimal
	Price         decimal.Decimal // для limit; для MARKET может быть 0
	ReduceOnly    bool
	PostOnly      bool
	TimeInForce   TimeInForce
}

// TimeInForce — тип ограничения по времени.
type TimeInForce string

const (
	TIFGTC TimeInForce = "GTC"
	TIFIOC TimeInForce = "IOC"
	TIFFOK TimeInForce = "FOK"
)

// CancelOrderRequest — запрос на отмену.
type CancelOrderRequest struct {
	ClientOrderID   ClientOrderID
	ExchangeOrderID string
	Symbol          ExchangeSymbol
}

// OrderQuery — запрос состояния ордера (для ACK timeout path).
type OrderQuery struct {
	ClientOrderID   ClientOrderID
	ExchangeOrderID string
	Symbol          ExchangeSymbol
}

// SetLeverageRequest
type SetLeverageRequest struct {
	Symbol   ExchangeSymbol
	Leverage decimal.Decimal
}

// SetMarginModeRequest
type SetMarginModeRequest struct {
	Symbol     ExchangeSymbol
	MarginMode MarginMode
}

// SetPositionModeRequest — установка one-way/hedge (раздел 5.3).
type SetPositionModeRequest struct {
	Mode PositionMode
}

// InternalTransferRequest — перевод main↔futures на одной бирже (раздел 12.2).
type InternalTransferRequest struct {
	Asset  string
	Amount decimal.Decimal
	From   string // "spot" / "futures" / аккаунт-тип на бирже
	To     string
}

// WithdrawalRequest — on-chain вывод (раздел 12.3).
type WithdrawalRequest struct {
	Asset   string
	Amount  decimal.Decimal
	Network string
	Address string
	Memo    string
}

// TransferQuery — фильтр для истории трансферов.
type TransferQuery struct {
	Asset string
	Since time.Time
	Limit int
}

// NetworkInfo — сведения о сети вывода актива.
type NetworkInfo struct {
	Network         string
	WithdrawEnabled bool
	DepositEnabled  bool
	WithdrawFee     decimal.Decimal
	WithdrawMin     decimal.Decimal
	DepositMin      decimal.Decimal
}

// WithdrawalResult / Deposit / Withdrawal (раздел 15.1).
type WithdrawalResult struct {
	WithdrawalID string
	TxID         string
	Status       string
}

type TransferResult struct {
	TransferID string
	Status     string
}

type Withdrawal struct {
	WithdrawalID string
	TxID         string
	Asset        string
	Network      string
	Amount       decimal.Decimal
	Fee          decimal.Decimal
	Status       string
	RequestedAt  time.Time
}

type Deposit struct {
	TxID        string
	Asset       string
	Network     string
	Amount      decimal.Decimal
	Status      string
	ConfirmedAt time.Time
}

// ADLState — индикатор ADL для инструмента (раздел 15.1, 23).
type ADLState struct {
	Symbol     ExchangeSymbol
	LongQueue  decimal.Decimal
	ShortQueue decimal.Decimal
	Timestamp  time.Time
}

// CredentialRef — ссылка на учётные данные (без самих секретов).
type CredentialRef struct {
	Exchange ExchangeID
	Kind     string // trade | withdrawal
	UserID   int64
}
