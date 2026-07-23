// Package observability реализует логирование, метрики и health checks
// (раздел 17 промпта v2) — без сторонних зависимостей (кроме стандартной библиотеки).
package observability

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"slices"
)

// ============================================================
// NewLogger — конструктор логгера (раздел 17.2)
// ============================================================

// NewLogger создаёт *slog.Logger с JSON- или text-handler в зависимости от json.
// level контролирует минимальный уровень вывода.
// Использовать json=true в production, json=false для локальной отладки.
func NewLogger(w io.Writer, level slog.Level, json bool) *slog.Logger {
	opts := &slog.HandlerOptions{Level: level}
	var inner slog.Handler
	if json {
		inner = slog.NewJSONHandler(w, opts)
	} else {
		inner = slog.NewTextHandler(w, opts)
	}
	// Оборачиваем в RedactingHandler: секреты не попадают в логи (раздел 17.2).
	return slog.New(NewRedactingHandler(inner))
}

// ============================================================
// Типизированные хелперы correlation fields (раздел 17.2)
// ============================================================

// WithPositionID добавляет correlation field position_id к логгеру.
func WithPositionID(log *slog.Logger, positionID string) *slog.Logger {
	return log.With(slog.String("position_id", positionID))
}

// WithLegID добавляет correlation field leg_id к логгеру.
func WithLegID(log *slog.Logger, legID string) *slog.Logger {
	return log.With(slog.String("leg_id", legID))
}

// WithExchange добавляет correlation field exchange к логгеру.
func WithExchange(log *slog.Logger, exchange string) *slog.Logger {
	return log.With(slog.String("exchange", exchange))
}

// WithClientOrderID добавляет correlation field client_order_id к логгеру.
func WithClientOrderID(log *slog.Logger, clientOrderID string) *slog.Logger {
	return log.With(slog.String("client_order_id", clientOrderID))
}

// WithTransferPlanID добавляет correlation field transfer_plan_id к логгеру.
func WithTransferPlanID(log *slog.Logger, transferPlanID string) *slog.Logger {
	return log.With(slog.String("transfer_plan_id", transferPlanID))
}

// ============================================================
// RedactingHandler — защита секретов в логах (раздел 17.2)
// ============================================================

// sensitiveKeyRE совпадает с именами полей, содержащими чувствительные данные.
// Используется case-insensitive: api_key, API_KEY, Secret, token, passphrase,
// authorization и любые их комбинации с суффиксами/префиксами.
var sensitiveKeyRE = regexp.MustCompile(`(?i)(api[_-]?key|secret|token|passphrase|authorization)`)

const redactedValue = "[REDACTED]"

// RedactingHandler оборачивает slog.Handler и заменяет значения чувствительных
// ключей строкой [REDACTED]. Является defense-in-depth слоем: даже если
// вышестоящий код случайно добавит секрет в атрибут — в лог попадёт [REDACTED].
type RedactingHandler struct {
	inner slog.Handler
}

// NewRedactingHandler создаёт RedactingHandler поверх inner handler.
func NewRedactingHandler(inner slog.Handler) *RedactingHandler {
	return &RedactingHandler{inner: inner}
}

// Enabled делегирует к inner handler.
func (h *RedactingHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

// Handle редактирует чувствительные атрибуты перед делегированием к inner.
func (h *RedactingHandler) Handle(ctx context.Context, r slog.Record) error {
	// Строим новую запись с теми же полями, но с отредактированными значениями.
	newRecord := slog.NewRecord(r.Time, r.Level, r.Message, r.PC)
	r.Attrs(func(a slog.Attr) bool {
		newRecord.AddAttrs(redactAttr(a))
		return true
	})
	return h.inner.Handle(ctx, newRecord)
}

// WithAttrs редактирует статические атрибуты и делегирует к inner handler.
func (h *RedactingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	redacted := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		redacted[i] = redactAttr(a)
	}
	return &RedactingHandler{inner: h.inner.WithAttrs(redacted)}
}

// WithGroup делегирует к inner handler (группы не несут чувствительных данных сами по себе).
func (h *RedactingHandler) WithGroup(name string) slog.Handler {
	return &RedactingHandler{inner: h.inner.WithGroup(name)}
}

// redactAttr рекурсивно проверяет атрибут и его дочерние элементы.
// При совпадении ключа с sensitiveKeyRE заменяет значение на [REDACTED].
func redactAttr(a slog.Attr) slog.Attr {
	// Для группы — рекурсивно обрабатываем вложенные атрибуты.
	if a.Value.Kind() == slog.KindGroup {
		children := a.Value.Group()
		redactedChildren := make([]any, 0, len(children))
		for _, child := range children {
			redactedChildren = append(redactedChildren, redactAttr(child))
		}
		return slog.Group(a.Key, redactedChildren...)
	}
	// Проверяем совпадение ключа с паттерном чувствительных полей.
	if sensitiveKeyRE.MatchString(a.Key) {
		return slog.String(a.Key, redactedValue)
	}
	return a
}

// ============================================================
// Уровни логирования (раздел 17.2)
// ============================================================

// Дополнительный уровень CRITICAL поверх стандартных slog уровней.
// slog не имеет встроенного CRITICAL, используем LevelError+4 по соглашению.
const LevelCritical = slog.LevelError + 4

// LogLevelFromString разбирает строку в slog.Level (из ColdConfig.LogLevel).
// Допустимые значения: debug, info, warn, error, critical (регистр не важен).
func LogLevelFromString(s string) (slog.Level, error) {
	// Нормализуем через ToLower без импорта strings (используем slices для поиска).
	type entry struct {
		name  string
		level slog.Level
	}
	levels := []entry{
		{"debug", slog.LevelDebug},
		{"info", slog.LevelInfo},
		{"warn", slog.LevelWarn},
		{"warning", slog.LevelWarn},
		{"error", slog.LevelError},
		{"critical", LevelCritical},
	}
	// Приводим s к нижнему регистру вручную (ASCII only).
	lower := make([]byte, len(s))
	for i := range s {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 32
		}
		lower[i] = c
	}
	idx := slices.IndexFunc(levels, func(e entry) bool { return e.name == string(lower) })
	if idx < 0 {
		return slog.LevelInfo, fmt.Errorf("observability: неизвестный уровень логирования %q", s)
	}
	return levels[idx].level, nil
}
