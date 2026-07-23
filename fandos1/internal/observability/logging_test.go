package observability

// Тесты логирования: redacting handler, correlation helpers, уровни.

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

// TestRedactingHandler_SecretsAreRedacted проверяет, что чувствительные поля
// заменяются [REDACTED] в JSON-выводе.
func TestRedactingHandler_SecretsAreRedacted(t *testing.T) {
	sensitiveKeys := []string{
		"api_key", "API_KEY", "ApiKey",
		"secret", "Secret", "SECRET",
		"token", "Token", "TOKEN",
		"passphrase", "Passphrase",
		"authorization", "Authorization",
	}
	for _, key := range sensitiveKeys {
		t.Run(key, func(t *testing.T) {
			var buf bytes.Buffer
			log := NewLogger(&buf, slog.LevelDebug, true)
			log.Info("тест", slog.String(key, "super-secret-value"))
			var record map[string]any
			if err := json.Unmarshal(buf.Bytes(), &record); err != nil {
				t.Fatalf("не JSON: %v (вывод: %s)", err, buf.String())
			}
			val, ok := record[key]
			if !ok {
				t.Fatalf("ключ %q не найден в записи: %s", key, buf.String())
			}
			if val != redactedValue {
				t.Errorf("ключ %q: ожидалось %q, получено %q", key, redactedValue, val)
			}
		})
	}
}

// TestRedactingHandler_SafeFieldsNotRedacted проверяет, что обычные поля
// проходят без изменений.
func TestRedactingHandler_SafeFieldsNotRedacted(t *testing.T) {
	var buf bytes.Buffer
	log := NewLogger(&buf, slog.LevelDebug, true)
	log.Info("тест",
		slog.String("position_id", "pos-123"),
		slog.String("exchange", "binance"),
		slog.Int("count", 42),
	)
	var record map[string]any
	if err := json.Unmarshal(buf.Bytes(), &record); err != nil {
		t.Fatalf("не JSON: %v", err)
	}
	if record["position_id"] != "pos-123" {
		t.Errorf("position_id должен быть pos-123, получено %v", record["position_id"])
	}
	if record["exchange"] != "binance" {
		t.Errorf("exchange должен быть binance, получено %v", record["exchange"])
	}
}

// TestWithHelpers проверяет, что correlation helpers добавляют нужные поля.
func TestWithHelpers(t *testing.T) {
	var buf bytes.Buffer
	log := NewLogger(&buf, slog.LevelDebug, true)
	log = WithPositionID(log, "pos-1")
	log = WithLegID(log, "leg-a")
	log = WithExchange(log, "bybit")
	log = WithClientOrderID(log, "order-xyz")
	log = WithTransferPlanID(log, "plan-99")
	log.Info("корреляция")

	var record map[string]any
	if err := json.Unmarshal(buf.Bytes(), &record); err != nil {
		t.Fatalf("не JSON: %v", err)
	}
	fields := map[string]string{
		"position_id":      "pos-1",
		"leg_id":           "leg-a",
		"exchange":         "bybit",
		"client_order_id":  "order-xyz",
		"transfer_plan_id": "plan-99",
	}
	for k, want := range fields {
		if got, ok := record[k]; !ok || got != want {
			t.Errorf("поле %q: ожидалось %q, получено %v (ok=%v)", k, want, got, ok)
		}
	}
}

// TestNewLogger_TextFormat проверяет, что text-format logger не паникует и выводит сообщение.
func TestNewLogger_TextFormat(t *testing.T) {
	var buf bytes.Buffer
	log := NewLogger(&buf, slog.LevelInfo, false)
	log.Info("text mode test")
	if !strings.Contains(buf.String(), "text mode test") {
		t.Errorf("ожидалось сообщение в выводе: %s", buf.String())
	}
}

// TestLevelFilter проверяет, что DEBUG-сообщения фильтруются при уровне INFO.
func TestLevelFilter(t *testing.T) {
	var buf bytes.Buffer
	log := NewLogger(&buf, slog.LevelInfo, true)
	log.Debug("скрытое сообщение")
	if buf.Len() > 0 {
		t.Errorf("DEBUG-сообщение не должно проходить через INFO-логгер: %s", buf.String())
	}
	log.Info("видимое сообщение")
	if buf.Len() == 0 {
		t.Error("INFO-сообщение должно появиться в выводе")
	}
}

// TestRedactingHandler_GroupAttributes проверяет redaction в группах атрибутов.
func TestRedactingHandler_GroupAttributes(t *testing.T) {
	var buf bytes.Buffer
	log := NewLogger(&buf, slog.LevelDebug, true)
	log.Info("группа", slog.Group("creds",
		slog.String("api_key", "hidden"),
		slog.String("user", "alice"),
	))
	// Проверяем, что в сыром выводе нет значения "hidden"
	if strings.Contains(buf.String(), "hidden") {
		t.Errorf("секрет api_key утёк в лог: %s", buf.String())
	}
}
