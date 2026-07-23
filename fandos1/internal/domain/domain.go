// Package domain содержит доменные типы, общие для всей системы.
// Здесь нет бизнес-логики — только идентичность, перечисления и инварианты типов.
// Соответствует разделам 3, 4.3, 6, 10, 12.5 промпта v2.
package domain

import (
	"errors"
	"fmt"
	"time"
)

// ExchangeID — идентификатор поддерживаемой биржи (раздел 0).
type ExchangeID string

const (
	ExchangeBinance ExchangeID = "binance"
	ExchangeBybit   ExchangeID = "bybit"
	ExchangeMEXC    ExchangeID = "mexc"
	ExchangeOKX     ExchangeID = "okx"
	ExchangeBitget  ExchangeID = "bitget"
	ExchangeKuCoin  ExchangeID = "kucoin"
	ExchangeGate    ExchangeID = "gate"
)

// SupportedExchanges возвращает все поддерживаемые в v1 биржи.
func SupportedExchanges() []ExchangeID {
	return []ExchangeID{
		ExchangeBinance, ExchangeBybit, ExchangeMEXC, ExchangeOKX,
		ExchangeBitget, ExchangeKuCoin, ExchangeGate,
	}
}

// IsValid проверяет, что биржа поддерживается.
func (e ExchangeID) IsValid() bool {
	switch e {
	case ExchangeBinance, ExchangeBybit, ExchangeMEXC, ExchangeOKX,
		ExchangeBitget, ExchangeKuCoin, ExchangeGate:
		return true
	}
	return false
}

// Side — сторона позиции/ордера (раздел 3.1).
type Side string

const (
	SideLong  Side = "long"
	SideShort Side = "short"
)

// IsValid проверяет корректность стороны.
func (s Side) IsValid() bool {
	return s == SideLong || s == SideShort
}

// SideSign возвращает +1 для long, -1 для short (конвенция funding, раздел 3.2).
// panic при невалидной стороне невозможен, т.к. используется только после IsValid.
func (s Side) Sign() int {
	if s == SideLong {
		return 1
	}
	return -1
}

// InstrumentType — тип инструмента. В v1 поддерживается только LINEAR_USDT_PERPETUAL (раздел 1.1).
type InstrumentType string

const (
	InstrumentLinearUSDTPerpetual InstrumentType = "LINEAR_USDT_PERPETUAL"
)

// InstrumentStatus — статус инструмента на бирже (раздел 6.1).
type InstrumentStatus string

const (
	InstrumentStatusActive    InstrumentStatus = "active"
	InstrumentStatusDelisted  InstrumentStatus = "delisted"
	InstrumentStatusHalted    InstrumentStatus = "halted"
	InstrumentStatusReduceOnly InstrumentStatus = "reduce_only"
)

// IsTradable — true, если по инструменту можно открывать новые позиции.
// При reduce_only/halted/delisted новые входы запрещены (раздел 1.3, 6.3).
func (s InstrumentStatus) IsTradable() bool {
	return s == InstrumentStatusActive
}

// FundingPriceType — цена, используемая биржей для начисления funding (раздел 3.2, 6.1).
type FundingPriceType string

const (
	FundingPriceMark  FundingPriceType = "mark"
	FundingPriceIndex FundingPriceType = "index"
)

// FundingRateType — realized (свершившийся) vs predicted (прогноз) funding rate (раздел 3.2).
type FundingRateType string

const (
	FundingRateRealized  FundingRateType = "realized"
	FundingRatePredicted FundingRateType = "predicted"
)

// ConfidenceLevel — уверенность в predicted funding rate (раздел 3.2, 8.3).
// Влияет на FundingUncertaintyReserve и eligibility (MinConfidenceLevel).
type ConfidenceLevel int

const (
	ConfidenceNone ConfidenceLevel = iota
	ConfidenceLow
	ConfidenceMedium
	ConfidenceHigh
)

// String возвращает текстовое представление для UI/логов.
func (c ConfidenceLevel) String() string {
	switch c {
	case ConfidenceNone:
		return "none"
	case ConfidenceLow:
		return "low"
	case ConfidenceMedium:
		return "medium"
	case ConfidenceHigh:
		return "high"
	default:
		return fmt.Sprintf("unknown(%d)", int(c))
	}
}

// AtLeast возвращает true, если уровень уверенности не ниже требуемого.
// Используется в фильтре MinConfidenceLevel (раздел 5.1).
func (c ConfidenceLevel) AtLeast(min ConfidenceLevel) bool {
	return c >= min
}

// CounterpartyRiskTier — риск-уровень биржи как контрагента (раздел 5.2, 23.4).
type CounterpartyRiskTier string

const (
	CounterpartyTierA CounterpartyRiskTier = "A" // крупная, регулируемая, высокий insurance fund
	CounterpartyTierB CounterpartyRiskTier = "B" // средняя
	CounterpartyTierC CounterpartyRiskTier = "C" // высокая неопределённость solvency/опер.риска
)

// IsValid проверяет корректность tier.
func (c CounterpartyRiskTier) IsValid() bool {
	return c == CounterpartyTierA || c == CounterpartyTierB || c == CounterpartyTierC
}

// MarginMode — режим маржи (раздел 5.2). По умолчанию ISOLATED.
type MarginMode string

const (
	MarginIsolated MarginMode = "isolated"
	MarginCross    MarginMode = "cross"
)

// PositionMode — режим позиций на бирже (раздел 5.3, 10.1).
type PositionMode string

const (
	PositionOneWay PositionMode = "one_way"
	PositionHedge  PositionMode = "hedge"
)

// OrderMode — режим исполнения ордеров (раздел 5.3).
type OrderMode string

const (
	OrderMarketableLimitIOC OrderMode = "marketable_limit_ioc" // по умолчанию
	OrderMarket             OrderMode = "market"                // только при явном разрешении
)

// SystemState — глобальное состояние системы (раздел 4.3).
type SystemState string

const (
	StateStarting        SystemState = "STARTING"
	StateReady           SystemState = "READY"
	StatePausedByUser    SystemState = "PAUSED_BY_USER"
	StateSafeHalt        SystemState = "SAFE_HALT"
	StateTradingLocked   SystemState = "TRADING_LOCKED"
	StateRebalanceLocked SystemState = "REBALANCE_LOCKED"
	StateRecoveryRequired SystemState = "RECOVERY_REQUIRED"
)

// AllowsNewEntry — можно ли открывать новые позиции в этом состоянии (раздел 4.3, 1.3).
// Только READY разрешает новые входы; все остальные состояния блокируют.
func (s SystemState) AllowsNewEntry() bool {
	return s == StateReady
}

// RequiresRecovery — true для состояний, требующих вмешательства/восстановления.
func (s SystemState) RequiresRecovery() bool {
	return s == StateRecoveryRequired || s == StateSafeHalt || s == StateTradingLocked
}

// RunMode — режим запуска системы (раздел 19).
type RunMode string

const (
	RunModeDryRun  RunMode = "dry_run"
	RunModePaper   RunMode = "paper"
	RunModeTestnet RunMode = "testnet"
	RunModeLive    RunMode = "live"
)

// SendsRealOrders — true, если режим подразумевает отправку реальных ордеров на биржу.
// dry_run/paper — нет; testnet/live — да (но testnet на sandbox-окружении).
func (r RunMode) SendsRealOrders() bool {
	return r == RunModeTestnet || r == RunModeLive
}

// IsLive — true только для настоящего production-режима.
func (r RunMode) IsLive() bool {
	return r == RunModeLive
}

// LegSide — сторона ноги в парной позиции (раздел 3.1).
// Совпадает с Side по значениям, но семантически — это роль в pair position.
type LegSide = Side

// TenantID — идентификатор арендатора. В v1 single-tenant (ADR-0001).
type TenantID string

// DefaultTenant — единственный tenant в v1 (ADR-0001).
const DefaultTenant TenantID = "default"

// ID-типы (строковые, типобезопасные). Новые-type, не alias, чтобы избежать смешения.
type (
	// AssetSymbol — канонический базовый актив, например "BTC", "ARB".
	AssetSymbol string
	// ExchangeSymbol — символ инструмента на конкретной бирже, напр. "BTCUSDT", "BTC-USDT-SWAP".
	ExchangeSymbol string
	// ClientOrderID — idempotency-идентификатор ордера, генерируемый backend (раздел 5.3).
	ClientOrderID string
	// PositionID — идентификатор парной позиции.
	PositionID string
	// LegID — идентификатор одной ноги.
	LegID string
	// ExecutionPlanID — идентификатор immutable execution plan (раздел 10.1).
	ExecutionPlanID string
	// TransferPlanID — идентификатор плана ребалансировки.
	TransferPlanID string
)

// Now — обёртка над time.Now для тестируемости (внедряется clock).
// По умолчанию — реальное время; в тестах подменяется fake clock.
var Now = time.Now

// ErrInvalidArgument — доменная ошибка невалидного аргумента.
var ErrInvalidArgument = errors.New("invalid argument")
