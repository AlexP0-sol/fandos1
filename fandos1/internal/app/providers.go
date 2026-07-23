// providers.go — реализации интерфейсов Mini App API (internal/telegram)
// поверх repository и lifecycle. Server-процесс отдаёт состояние из БД —
// единственного источника истины; транзиентные данные сканера живут в worker.
package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/thecd/fundarbitrage/internal/config"
	"github.com/thecd/fundarbitrage/internal/repository"
	"github.com/thecd/fundarbitrage/internal/scanner"
	"github.com/thecd/fundarbitrage/internal/telegram"
)

// ============================================================
// Settings: JSON DTO ↔ strategy_settings.payload
// ============================================================

// hotSettingsDTO — плоское JSON-представление HotSettings для payload/Mini App.
// Decimal-поля — строки (точность без float), map-поля — по ключу биржи.
type hotSettingsDTO struct {
	FundingSearchMode              string            `json:"fundingSearchMode"`
	RequireAlignedFundingTimes     bool              `json:"requireAlignedFundingTimes"`
	MinExpectedNetPnLUSDT          string            `json:"minExpectedNetPnlUsdt"`
	MinConfidenceLevel             int               `json:"minConfidenceLevel"`
	MinSecondsBeforeFundingToEnter int64             `json:"minSecondsBeforeFundingToEnter"`
	RequireBacktestPass            bool              `json:"requireBacktestPass"`
	Leverage                       string            `json:"leverage"`
	MarginMode                     string            `json:"marginMode"`
	PositionMode                   string            `json:"positionMode"`
	MaxDailyLossUSDT               string            `json:"maxDailyLossUsdt"`
	MaxPositionLossUSDT            string            `json:"maxPositionLossUsdt"`
	RiskSnapAfterMaxDailyLoss      bool              `json:"riskSnapAfterMaxDailyLoss"`
	JointSlippageCapBps            string            `json:"jointSlippageCapBps"`
	MaxExposurePerExchangeUSDT     map[string]string `json:"maxExposurePerExchangeUsdt,omitempty"`
	DeltaToleranceBase             string            `json:"deltaToleranceBase"`
	DeltaToleranceUSD              string            `json:"deltaToleranceUsd"`
	OrderMode                      string            `json:"orderMode"`
	AckTimeoutBehavior             string            `json:"ackTimeoutBehavior"`
	OrderAckTimeoutMs              int64             `json:"orderAckTimeoutMs"`
	ExitIfADLDetected              bool              `json:"exitIfAdlDetected"`
	ExitIfFundingSignChanges       bool              `json:"exitIfFundingSignChanges"`
	RebalanceEnabled               bool              `json:"rebalanceEnabled"`
	WithdrawalFeeCapUSDT           string            `json:"withdrawalFeeCapUsdt"`
	WithdrawalFailureThreshold     int               `json:"withdrawalFailureThreshold"`
	DepositGracePeriodMs           int64             `json:"depositGracePeriodMs"`
}

// settingsDTOFromHot конвертирует HotSettings → DTO.
func settingsDTOFromHot(h config.HotSettings) hotSettingsDTO {
	exposure := make(map[string]string, len(h.MaxExposurePerExchangeUSDT))
	for ex, v := range h.MaxExposurePerExchangeUSDT {
		exposure[string(ex)] = v.String()
	}
	return hotSettingsDTO{
		FundingSearchMode:              h.FundingSearchMode,
		RequireAlignedFundingTimes:     h.RequireAlignedFundingTimes,
		MinExpectedNetPnLUSDT:          h.MinExpectedNetPnLUSDT.String(),
		MinConfidenceLevel:             int(h.MinConfidenceLevel),
		MinSecondsBeforeFundingToEnter: h.MinSecondsBeforeFundingToEnter,
		RequireBacktestPass:            h.RequireBacktestPass,
		Leverage:                       h.Leverage.String(),
		MarginMode:                     string(h.MarginMode),
		PositionMode:                   string(h.PositionMode),
		MaxDailyLossUSDT:               h.MaxDailyLossUSDT.String(),
		MaxPositionLossUSDT:            h.MaxPositionLossUSDT.String(),
		RiskSnapAfterMaxDailyLoss:      h.RiskSnapAfterMaxDailyLoss,
		JointSlippageCapBps:            h.JointSlippageCapBps.String(),
		MaxExposurePerExchangeUSDT:     exposure,
		DeltaToleranceBase:             h.DeltaToleranceBase.String(),
		DeltaToleranceUSD:              h.DeltaToleranceUSD.String(),
		OrderMode:                      string(h.OrderMode),
		AckTimeoutBehavior:             h.AckTimeoutBehavior,
		OrderAckTimeoutMs:              h.OrderAckTimeoutMs,
		ExitIfADLDetected:              h.ExitIfADLDetected,
		ExitIfFundingSignChanges:       h.ExitIfFundingSignChanges,
		RebalanceEnabled:               h.RebalanceEnabled,
		WithdrawalFeeCapUSDT:           h.WithdrawalFeeCapUSDT.String(),
		WithdrawalFailureThreshold:     h.WithdrawalFailureThreshold,
		DepositGracePeriodMs:           h.DepositGracePeriodMs,
	}
}

// DBSettingsProvider — SettingsProvider поверх strategy_settings (CAS-версия).
type DBSettingsProvider struct {
	Repo *repository.SettingsRepo
}

// Get возвращает payload и версию.
func (p *DBSettingsProvider) Get(ctx context.Context) (json.RawMessage, int64, error) {
	payload, version, err := p.Repo.LoadHot(ctx)
	if err != nil {
		return nil, 0, err
	}
	return payload, version, nil
}

// Save применяет новый payload при совпадении версии (409 наверху при конфликте).
func (p *DBSettingsProvider) Save(ctx context.Context, raw json.RawMessage, version int64) error {
	// Валидация структуры до записи: неизвестные поля отвергаем.
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var dto hotSettingsDTO
	if err := dec.Decode(&dto); err != nil {
		return fmt.Errorf("app: invalid settings payload: %w", err)
	}
	return p.Repo.SaveHot(ctx, raw, version)
}

// ============================================================
// Status: сборка dashboard DTO из БД + lifecycle
// ============================================================

// DBStatusProvider — StatusProvider поверх repository/halter.
type DBStatusProvider struct {
	Boot *Bootstrap
}

// Status собирает состояние системы для Mini App (раздел 14.1).
func (p *DBStatusProvider) Status(ctx context.Context) (telegram.StatusDTO, error) {
	state := "RUNNING"
	if halted, _ := p.Boot.Halter.IsHalted(); halted {
		state = "SAFE_HALT"
	}
	open, err := p.Boot.Positions.LoadByStates(ctx,
		"OPENING", "PARTIALLY_HEDGED", "HEDGED", "MONITORING", "EXIT_REQUESTED", "EXITING", "DEGRADED")
	if err != nil {
		return telegram.StatusDTO{}, fmt.Errorf("app: load open positions: %w", err)
	}
	return telegram.StatusDTO{
		SystemState:   state,
		RunMode:       string(p.Boot.Cold.RunMode),
		OpenPositions: len(open),
		// Equity/PnL агрегаты появятся из account_snapshots, когда worker начнёт их писать.
		TotalEquityUSDT: "0",
		RealizedPnL:     "0",
		UnrealizedPnL:   "0",
		FundingPnL:      "0",
	}, nil
}

// ============================================================
// Candidates / Close — v1-заглушки серверного процесса
// ============================================================

// EmptyCandidatesProvider — кандидаты сканера транзиентны и живут в worker;
// серверный список появится после снапшот-таблицы (backlog этапа 12).
type EmptyCandidatesProvider struct{}

// Candidates возвращает пустой список.
func (EmptyCandidatesProvider) Candidates(context.Context) ([]scanner.Candidate, error) {
	return nil, nil
}

// LockingCloseRequester — ручное закрытие из Mini App (semi-auto v1):
// фиксирует запрос в audit_log; исполнение подхватывает worker.
type LockingCloseRequester struct {
	Boot *Bootstrap
}

// RequestClose записывает намерение оператора закрыть позицию.
func (r *LockingCloseRequester) RequestClose(ctx context.Context, positionID string) error {
	tx, err := r.Boot.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("app: begin close request tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if err := r.Boot.Audit.WriteTx(ctx, tx, "user:miniapp", "REQUEST_CLOSE", positionID,
		map[string]string{"position_id": positionID}, "queued"); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
