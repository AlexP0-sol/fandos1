// Package config реализует двухкатегорийную конфигурационную модель (раздел 15.3):
//   - ColdConfig — процессная конфигурация, immutable после старта (env/config.yaml);
//   - HotSettings — пользовательские настройки стратегии (БД → Mini App),
//     перезагружаемые без рестарта через atomic pointer swap.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	dec "github.com/thecd/fundarbitrage/internal/decimal"
	"github.com/thecd/fundarbitrage/internal/domain"
)

// ============================================================
// COLD — immutable после старта процесса
// ============================================================

type ColdConfig struct {
	HTTPAddr          string
	PublicBaseURL     string
	DBDSN             string
	DBMaxOpenConns    int
	DBMaxIdleConns    int
	MasterKeyEnv      string // имя env-переменной с master key (сам ключ НЕ хранится здесь)
	KMSProvider       string // env | aws | gcp
	NTPServers        []string
	MaxClockOffsetMs  int64
	ClockSyncInterval time.Duration
	PrometheusAddr    string
	OTLPEndpoint      string
	ShutdownTimeout   time.Duration
	LogLevel          string
	RunMode           domain.RunMode
}

// LoadCold читает env с безопасными defaults и валидирует.
// RunMode по умолчанию dry_run — live только явно (принцип safe defaults).
// Если переменная окружения задана, но не разбирается — возвращает ошибку
// (fail-fast при опечатках в конфиге).
func LoadCold() (*ColdConfig, error) {
	maxOpenConns, err := envInt("DB_MAX_OPEN_CONNS", 25)
	if err != nil {
		return nil, err
	}
	maxIdleConns, err := envInt("DB_MAX_IDLE_CONNS", 5)
	if err != nil {
		return nil, err
	}
	maxClockOffsetMs, err := envInt("MAX_CLOCK_OFFSET_MS", 500)
	if err != nil {
		return nil, err
	}
	clockSyncInterval, err := envDur("CLOCK_SYNC_INTERVAL", 30*time.Second)
	if err != nil {
		return nil, err
	}
	shutdownTimeout, err := envDur("SHUTDOWN_TIMEOUT", 30*time.Second)
	if err != nil {
		return nil, err
	}
	c := &ColdConfig{
		HTTPAddr:          envStr("HTTP_ADDR", ":8080"),
		PublicBaseURL:     envStr("PUBLIC_BASE_URL", ""),
		DBDSN:             envStr("DATABASE_URL", ""),
		DBMaxOpenConns:    maxOpenConns,
		DBMaxIdleConns:    maxIdleConns,
		MasterKeyEnv:      envStr("MASTER_KEY_ENV", "MASTER_KEY"),
		KMSProvider:       envStr("KMS_PROVIDER", "env"),
		NTPServers:        strings.Split(envStr("NTP_SERVERS", "pool.ntp.org"), ","),
		MaxClockOffsetMs:  int64(maxClockOffsetMs),
		ClockSyncInterval: clockSyncInterval,
		PrometheusAddr:    envStr("PROM_ADDR", ":9090"),
		OTLPEndpoint:      envStr("OTLP_ENDPOINT", ""),
		ShutdownTimeout:   shutdownTimeout,
		LogLevel:          envStr("LOG_LEVEL", "info"),
		RunMode:           domain.RunMode(envStr("RUN_MODE", string(domain.RunModeDryRun))),
	}
	return c, c.validate()
}

func (c *ColdConfig) validate() error {
	if c.DBDSN == "" {
		return fmt.Errorf("config: DATABASE_URL обязателен")
	}
	switch c.RunMode {
	case domain.RunModeDryRun, domain.RunModePaper, domain.RunModeTestnet, domain.RunModeLive:
	default:
		return fmt.Errorf("config: неизвестный RUN_MODE %q", c.RunMode)
	}
	if c.MaxClockOffsetMs <= 0 {
		return fmt.Errorf("config: MAX_CLOCK_OFFSET_MS должен быть > 0")
	}
	if c.KMSProvider != "env" && c.KMSProvider != "aws" && c.KMSProvider != "gcp" {
		return fmt.Errorf("config: неизвестный KMS_PROVIDER %q", c.KMSProvider)
	}
	// Ограничения пула соединений: idle не должен превышать open.
	if c.DBMaxIdleConns > c.DBMaxOpenConns {
		return fmt.Errorf("config: DB_MAX_IDLE_CONNS (%d) > DB_MAX_OPEN_CONNS (%d)", c.DBMaxIdleConns, c.DBMaxOpenConns)
	}
	// ShutdownTimeout и ClockSyncInterval должны быть положительными.
	if c.ShutdownTimeout <= 0 {
		return fmt.Errorf("config: SHUTDOWN_TIMEOUT должен быть > 0")
	}
	if c.ClockSyncInterval <= 0 {
		return fmt.Errorf("config: CLOCK_SYNC_INTERVAL должен быть > 0")
	}
	return nil
}

func envStr(k, d string) string {
	if v, ok := os.LookupEnv(k); ok {
		// Защита от типичных ошибок .env-файлов: хвостовые пробелы и
		// inline-комментарии («значение   # пояснение») — обрезаем.
		if i := strings.Index(v, " #"); i >= 0 {
			v = v[:i]
		}
		if i := strings.Index(v, "\t#"); i >= 0 {
			v = v[:i]
		}
		return strings.TrimSpace(v)
	}
	return d
}

// envInt читает целочисленную переменную окружения.
// Если переменная задана, но не является числом — возвращает ошибку (fail-fast).
func envInt(k string, d int) (int, error) {
	v, ok := os.LookupEnv(k)
	if !ok {
		return d, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("config: %s=%q: %w", k, v, err)
	}
	return n, nil
}

// envDur читает duration-переменную окружения.
// Если переменная задана, но не разбирается — возвращает ошибку (fail-fast).
func envDur(k string, d time.Duration) (time.Duration, error) {
	v, ok := os.LookupEnv(k)
	if !ok {
		return d, nil
	}
	p, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("config: %s=%q: %w", k, v, err)
	}
	return p, nil
}

// ============================================================
// HOT — atomic-обновляемые user settings (раздел 5)
// ============================================================

// HotSettings — снимок пользовательских настроек. Immutable по конвенции:
// изменение = Swap нового снимка целиком (никаких мутаций полей на живом объекте).
// Полный перечень — раздел 5 промпта v2; здесь ключевые поля 1:1 с БД (strategy_settings.payload).
type HotSettings struct {
	Version int64 // монотонная версия строки в БД — для LISTEN/NOTIFY-reload

	// 5.1 Стратегия поиска
	FundingSearchMode              string
	RequireAlignedFundingTimes     bool
	MinExpectedNetPnLUSDT          dec.Decimal
	MinConfidenceLevel             domain.ConfidenceLevel
	MinSecondsBeforeFundingToEnter int64
	RequireBacktestPass            bool

	// 5.2 Риск и капитал
	Leverage                   dec.Decimal
	MarginMode                 domain.MarginMode
	PositionMode               domain.PositionMode
	MaxDailyLossUSDT           dec.Decimal
	MaxPositionLossUSDT        dec.Decimal
	RiskSnapAfterMaxDailyLoss  bool
	JointSlippageCapBps        dec.Decimal
	MaxExposurePerExchangeUSDT map[domain.ExchangeID]dec.Decimal
	CounterpartyRiskTier       map[domain.ExchangeID]domain.CounterpartyRiskTier
	ADLExposureLimitPercent    map[domain.ExchangeID]dec.Decimal
	DeltaToleranceBase         dec.Decimal
	DeltaToleranceUSD          dec.Decimal

	// 5.3 Исполнение
	OrderMode          domain.OrderMode
	AckTimeoutBehavior string // QUERY_THEN_DECIDE
	OrderAckTimeoutMs  int64

	// 5.4 Выход
	ExitIfADLDetected        bool
	ExitIfFundingSignChanges bool

	// 5.5 Ребалансировка
	RebalanceEnabled           bool
	WithdrawalFeeCapUSDT       dec.Decimal
	WithdrawalFailureThreshold int
	DepositGracePeriodMs       int64
}

// Defaults — консервативные значения по умолчанию (CONFIG_MODEL.md п.1.4):
// AUTO/ребаланс выключены, risk snap включён, backtest обязателен.
func Defaults() *HotSettings {
	return &HotSettings{
		FundingSearchMode:              "SAME_INTERVAL",
		RequireAlignedFundingTimes:     true,
		MinExpectedNetPnLUSDT:          dec.MustFromString("1"),
		MinConfidenceLevel:             domain.ConfidenceMedium,
		MinSecondsBeforeFundingToEnter: 30,
		RequireBacktestPass:            true,
		Leverage:                       dec.MustFromString("2"),
		MarginMode:                     domain.MarginIsolated,
		PositionMode:                   domain.PositionOneWay,
		RiskSnapAfterMaxDailyLoss:      true,
		JointSlippageCapBps:            dec.MustFromString("20"),
		OrderMode:                      domain.OrderMarketableLimitIOC,
		AckTimeoutBehavior:             "QUERY_THEN_DECIDE",
		OrderAckTimeoutMs:              3000,
		ExitIfADLDetected:              true,
		ExitIfFundingSignChanges:       true,
		RebalanceEnabled:               false,
		WithdrawalFailureThreshold:     3,
		DepositGracePeriodMs:           30 * 60 * 1000,
	}
}

// Warning — небезопасная, но допустимая комбинация (CONFIG_MODEL.md п.4).
type Warning struct{ Code, Message string }

// Validate возвращает жёсткие ошибки и список предупреждений для UI.
func (h *HotSettings) Validate() ([]Warning, error) {
	var warns []Warning
	if h.MinSecondsBeforeFundingToEnter < 0 || h.OrderAckTimeoutMs <= 0 {
		return nil, fmt.Errorf("hot settings: отрицательные/нулевые тайминги")
	}
	if h.AckTimeoutBehavior != "QUERY_THEN_DECIDE" {
		return nil, fmt.Errorf("hot settings: единственный допустимый AckTimeoutBehavior в v1 — QUERY_THEN_DECIDE")
	}
	// Leverage должен быть строго положительным.
	if !h.Leverage.IsPositive() {
		return nil, fmt.Errorf("hot settings: Leverage должен быть > 0, получено %s", h.Leverage)
	}
	// Дневной лимит убытка и лимит убытка позиции не могут быть отрицательными.
	if h.MaxDailyLossUSDT.IsNegative() {
		return nil, fmt.Errorf("hot settings: MaxDailyLossUSDT не может быть отрицательным")
	}
	if h.MaxPositionLossUSDT.IsNegative() {
		return nil, fmt.Errorf("hot settings: MaxPositionLossUSDT не может быть отрицательным")
	}
	// MarginMode должен быть одним из допустимых значений домена.
	if h.MarginMode != domain.MarginIsolated && h.MarginMode != domain.MarginCross {
		return nil, fmt.Errorf("hot settings: неизвестный MarginMode %q", h.MarginMode)
	}
	// PositionMode должен быть одним из допустимых значений домена.
	if h.PositionMode != domain.PositionOneWay && h.PositionMode != domain.PositionHedge {
		return nil, fmt.Errorf("hot settings: неизвестный PositionMode %q", h.PositionMode)
	}
	if h.OrderMode == domain.OrderMarket {
		warns = append(warns, Warning{"MARKET_MODE", "MARKET-режим: неконтролируемый slippage"})
	}
	if !h.RiskSnapAfterMaxDailyLoss {
		warns = append(warns, Warning{"NO_RISK_SNAP", "нет авто-останова после дневного убытка"})
	}
	if !h.RequireBacktestPass {
		warns = append(warns, Warning{"NO_BACKTEST", "допуск пар без backtest"})
	}
	if h.MinSecondsBeforeFundingToEnter < 5 {
		warns = append(warns, Warning{"LATE_ENTRY", "вход в последние секунды перед funding: edge может обнулиться"})
	}
	if h.MaxDailyLossUSDT.IsZero() {
		warns = append(warns, Warning{"NO_DAILY_STOP", "не задан дневной стоп-лимит"})
	}
	// Предупреждение при высоком плече.
	if h.Leverage.GreaterThan(dec.MustFromString("20")) {
		warns = append(warns, Warning{"HIGH_LEVERAGE", fmt.Sprintf("высокое плечо %s: повышенный риск ликвидации", h.Leverage)})
	}
	return warns, nil
}

// Store — потокобезопасный держатель актуального снимка HotSettings.
type Store struct{ p atomic.Pointer[HotSettings] }

func NewStore(initial *HotSettings) (*Store, []Warning, error) {
	w, err := initial.Validate()
	if err != nil {
		return nil, nil, err
	}
	s := &Store{}
	s.p.Store(initial)
	return s, w, nil
}

// Current — текущий снимок (не мутировать!).
func (s *Store) Current() *HotSettings { return s.p.Load() }

// Swap валидирует и атомарно подменяет снимок. Отклоняет устаревшие версии.
func (s *Store) Swap(next *HotSettings) ([]Warning, error) {
	w, err := next.Validate()
	if err != nil {
		return nil, err
	}
	if cur := s.p.Load(); cur != nil && next.Version <= cur.Version {
		return nil, fmt.Errorf("hot settings: stale version %d <= %d", next.Version, cur.Version)
	}
	s.p.Store(next)
	return w, nil
}
