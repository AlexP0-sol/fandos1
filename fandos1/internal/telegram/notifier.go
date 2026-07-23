package telegram

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// ============================================================
// NotificationKind — вид уведомления (раздел 14.5)
// ============================================================

// NotificationKind — семантика события.
type NotificationKind string

const (
	KindBotStart            NotificationKind = "bot_start"
	KindBotStop             NotificationKind = "bot_stop"
	KindExchangeDisconnect  NotificationKind = "exchange_disconnect"
	KindExchangeReconnect   NotificationKind = "exchange_reconnect"
	KindPositionOpen        NotificationKind = "position_open"
	KindPositionPartial     NotificationKind = "position_partial"
	KindPositionHedged      NotificationKind = "position_hedged"
	KindFundingReceived     NotificationKind = "funding_received"
	KindPositionClose       NotificationKind = "position_close"
	KindADLDetected         NotificationKind = "adl_detected"
	KindSafeHalt            NotificationKind = "safe_halt"
	KindRebalanceLocked     NotificationKind = "rebalance_locked"
	KindCircuitBreaker      NotificationKind = "circuit_breaker"
	KindClockSkew           NotificationKind = "clock_skew"
	KindWithdrawError       NotificationKind = "withdraw_error"
	KindRiskWarning         NotificationKind = "risk_warning"
	KindMaxDailyLoss        NotificationKind = "max_daily_loss"
	KindManualOperation     NotificationKind = "manual_operation"
	KindSecondFactorRequest NotificationKind = "second_factor_request"
)

// NotificationSeverity — уровень важности уведомления.
type NotificationSeverity string

const (
	SeverityInfo     NotificationSeverity = "info"
	SeverityWarning  NotificationSeverity = "warning"
	SeverityCritical NotificationSeverity = "critical"
)

// ============================================================
// NotificationSink — интерфейс хранилища уведомлений
// ============================================================

// NotificationRecord — запись уведомления для персистентного хранилища.
type NotificationRecord struct {
	DedupKey string
	Kind     NotificationKind
	Severity NotificationSeverity
	Text     string
	SentAt   time.Time
}

// NotificationSink — интерфейс для персистентного хранения уведомлений.
// Реализация через репозиторий подключается позже.
type NotificationSink interface {
	// Save сохраняет запись уведомления.
	Save(ctx context.Context, rec NotificationRecord) error
}

// noopSink — заглушка NotificationSink (используется если sink не задан).
type noopSink struct{}

func (noopSink) Save(_ context.Context, _ NotificationRecord) error { return nil }

// ============================================================
// Notifier — отправка уведомлений с rate limiting по dedup-ключу
// ============================================================

// NotifierConfig — конфигурация Notifier.
type NotifierConfig struct {
	// ChatID — Telegram chat ID получателя уведомлений.
	ChatID int64
	// MinInterval — минимальный интервал между уведомлениями с одинаковым dedup-ключом.
	// 0 = без ограничений (только для тестов).
	MinInterval time.Duration
}

// dedupEntry — запись в таблице дедупликации.
type dedupEntry struct {
	lastSent time.Time
}

// Notifier — отправляет уведомления через Bot с per-ключевым rate limiting.
type Notifier struct {
	bot  *Bot
	cfg  NotifierConfig
	sink NotificationSink

	mu    sync.Mutex
	dedup map[string]*dedupEntry
}

// NewNotifier создаёт Notifier.
// sink может быть nil — в этом случае используется заглушка.
func NewNotifier(bot *Bot, cfg NotifierConfig, sink NotificationSink) *Notifier {
	if sink == nil {
		sink = noopSink{}
	}
	return &Notifier{
		bot:   bot,
		cfg:   cfg,
		sink:  sink,
		dedup: make(map[string]*dedupEntry),
	}
}

// Notify отправляет уведомление если dedup-ключ не блокирует.
// dedupKey — строка-ключ для rate limiting (например "safe_halt" или "exchange:binance:disconnect").
// Не включает секреты в текст уведомления или логи.
func (n *Notifier) Notify(ctx context.Context, dedupKey string, kind NotificationKind, severity NotificationSeverity, text string) error {
	now := time.Now()

	// Проверяем rate limit.
	if n.cfg.MinInterval > 0 {
		n.mu.Lock()
		entry, ok := n.dedup[dedupKey]
		if ok && now.Sub(entry.lastSent) < n.cfg.MinInterval {
			n.mu.Unlock()
			slog.Debug("telegram: notifier: пропуск дубликата", "key", dedupKey, "kind", string(kind))
			return nil
		}
		if !ok {
			entry = &dedupEntry{}
			n.dedup[dedupKey] = entry
		}
		entry.lastSent = now
		n.mu.Unlock()
	}

	// Формируем текст с severity-префиксом.
	prefix := severityPrefix(severity)
	fullText := prefix + text

	// Отправляем в Telegram.
	opts := SendMessageOpts{DisableWebPagePreview: true}
	if err := n.bot.SendMessage(ctx, n.cfg.ChatID, fullText, opts); err != nil {
		// Логируем ошибку без текста сообщения (может содержать данные).
		slog.Error("telegram: notifier: ошибка отправки", "key", dedupKey, "kind", string(kind), "err", err)
		return fmt.Errorf("notifier: %w", err)
	}

	// Сохраняем в sink (персистентное хранилище).
	rec := NotificationRecord{
		DedupKey: dedupKey,
		Kind:     kind,
		Severity: severity,
		Text:     text,
		SentAt:   now,
	}
	if err := n.sink.Save(ctx, rec); err != nil {
		// Ошибка sink не должна блокировать основной поток.
		slog.Error("telegram: notifier: ошибка сохранения в sink", "key", dedupKey, "err", err)
	}

	return nil
}

// severityPrefix — эмодзи-префикс по уровню важности.
func severityPrefix(s NotificationSeverity) string {
	switch s {
	case SeverityCritical:
		return "🚨 "
	case SeverityWarning:
		return "⚠️ "
	default:
		return "ℹ️ "
	}
}
