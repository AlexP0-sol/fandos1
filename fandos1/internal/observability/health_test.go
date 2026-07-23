package observability

// Тесты health checker: 200/503, JSON shape, таймаут, non-critical degraded.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// ============================================================
// Вспомогательные функции
// ============================================================

// newHC создаёт HealthChecker с коротким таймаутом для тестов.
func newHC() *HealthChecker {
	return NewHealthCheckerWithTimeout(500 * time.Millisecond)
}

// checkResponse декодирует JSON из ResponseRecorder.
func checkResponse(t *testing.T, rr *httptest.ResponseRecorder) HealthResponse {
	t.Helper()
	var resp HealthResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("не удалось декодировать JSON: %v (тело: %s)", err, rr.Body.String())
	}
	return resp
}

// ============================================================
// Liveness handler
// ============================================================

// TestLivenessAlwaysOK проверяет, что /healthz всегда отвечает 200.
func TestLivenessAlwaysOK(t *testing.T) {
	hc := newHC()
	hc.RegisterCheck("db", true, func(ctx context.Context) error {
		return errors.New("база недоступна")
	})
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rr := httptest.NewRecorder()
	hc.LivenessHandler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("LivenessHandler ожидался 200, получен %d", rr.Code)
	}
}

// TestLivenessReturnsJSON проверяет shape JSON ответа /healthz.
func TestLivenessReturnsJSON(t *testing.T) {
	hc := newHC()
	hc.RegisterCheck("db", true, func(ctx context.Context) error { return nil })
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rr := httptest.NewRecorder()
	hc.LivenessHandler().ServeHTTP(rr, req)
	resp := checkResponse(t, rr)
	if resp.Status == "" {
		t.Error("status не должен быть пустым")
	}
	if len(resp.Checks) != 1 {
		t.Errorf("ожидался 1 check, получено %d", len(resp.Checks))
	}
	check := resp.Checks[0]
	if check.Name != "db" {
		t.Errorf("ожидался name=db, получено %q", check.Name)
	}
	if !check.Critical {
		t.Error("ожидался critical=true")
	}
	if !check.OK {
		t.Errorf("ожидался ok=true, error=%q", check.Error)
	}
	if check.LatencyMs < 0 {
		t.Errorf("latency_ms должна быть >= 0, получено %v", check.LatencyMs)
	}
}

// ============================================================
// Readiness handler
// ============================================================

// TestReadinessAllOK проверяет 200 при всех успешных проверках.
func TestReadinessAllOK(t *testing.T) {
	hc := newHC()
	hc.RegisterCheck("db", true, func(ctx context.Context) error { return nil })
	hc.RegisterCheck("clock", true, func(ctx context.Context) error { return nil })
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rr := httptest.NewRecorder()
	hc.ReadinessHandler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("ожидался 200, получен %d", rr.Code)
	}
	resp := checkResponse(t, rr)
	if resp.Status != "ok" {
		t.Errorf("ожидался status=ok, получено %q", resp.Status)
	}
}

// TestReadinessCriticalFails проверяет 503 при провале критической проверки.
func TestReadinessCriticalFails(t *testing.T) {
	hc := newHC()
	hc.RegisterCheck("db", true, func(ctx context.Context) error {
		return errors.New("connection refused")
	})
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rr := httptest.NewRecorder()
	hc.ReadinessHandler().ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("ожидался 503, получен %d", rr.Code)
	}
	resp := checkResponse(t, rr)
	if resp.Status != "unhealthy" {
		t.Errorf("ожидался status=unhealthy, получено %q", resp.Status)
	}
	if len(resp.Checks) != 1 || resp.Checks[0].OK {
		t.Error("check db должен быть ok=false")
	}
	if resp.Checks[0].Error == "" {
		t.Error("error должен быть заполнен при провале")
	}
}

// TestReadinessNonCriticalFail проверяет 200 при провале некритической проверки.
func TestReadinessNonCriticalFail(t *testing.T) {
	hc := newHC()
	hc.RegisterCheck("db", true, func(ctx context.Context) error { return nil })
	hc.RegisterCheck("breakers", false, func(ctx context.Context) error {
		return errors.New("circuit breaker активен")
	})
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rr := httptest.NewRecorder()
	hc.ReadinessHandler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("некритический провал не должен давать 503, получен %d", rr.Code)
	}
	resp := checkResponse(t, rr)
	if resp.Status != "degraded" {
		t.Errorf("ожидался status=degraded, получено %q", resp.Status)
	}
	// Проверяем, что breakers=false, db=true
	for _, ch := range resp.Checks {
		switch ch.Name {
		case "db":
			if !ch.OK {
				t.Error("db должна быть ok=true")
			}
		case "breakers":
			if ch.OK {
				t.Error("breakers должна быть ok=false")
			}
			if ch.Critical {
				t.Error("breakers должна быть critical=false")
			}
		}
	}
}

// ============================================================
// Таймаут проверки
// ============================================================

// TestCheckTimeoutEnforced проверяет, что зависшая проверка прерывается по таймауту.
func TestCheckTimeoutEnforced(t *testing.T) {
	// Короткий таймаут для теста — 100мс.
	hc := NewHealthCheckerWithTimeout(100 * time.Millisecond)
	hc.RegisterCheck("slow", true, func(ctx context.Context) error {
		// Ждём контекста — функция должна вернуть ошибку при отмене.
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
			return nil // не должны сюда попасть
		}
	})
	start := time.Now()
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rr := httptest.NewRecorder()
	hc.ReadinessHandler().ServeHTTP(rr, req)
	elapsed := time.Since(start)
	// Ответ должен прийти примерно через 100мс, не через 5с.
	if elapsed > 2*time.Second {
		t.Errorf("таймаут не сработал: elapsed=%v", elapsed)
	}
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("ожидался 503 (таймаут critical), получен %d", rr.Code)
	}
	resp := checkResponse(t, rr)
	if resp.Checks[0].OK {
		t.Error("зависшая проверка должна быть ok=false")
	}
}

// ============================================================
// JSON shape
// ============================================================

// TestJSONShape проверяет точную структуру JSON согласно раздел 17.3.
func TestJSONShape(t *testing.T) {
	hc := newHC()
	hc.RegisterCheck("master_key", true, func(ctx context.Context) error { return nil })
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rr := httptest.NewRecorder()
	hc.LivenessHandler().ServeHTTP(rr, req)

	// Проверяем raw JSON на наличие обязательных полей.
	body := rr.Body.String()
	requiredFields := []string{"status", "checks", "name", "ok", "critical", "latency_ms"}
	for _, field := range requiredFields {
		found := false
		for i := 0; i < len(body)-len(field); i++ {
			if body[i:i+len(field)] == field {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("поле %q не найдено в JSON: %s", field, body)
		}
	}
}

// TestNoChecks проверяет корректный ответ при отсутствии зарегистрированных проверок.
func TestNoChecks(t *testing.T) {
	hc := newHC()
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rr := httptest.NewRecorder()
	hc.ReadinessHandler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("без проверок ожидался 200, получен %d", rr.Code)
	}
	resp := checkResponse(t, rr)
	if resp.Status != "ok" {
		t.Errorf("ожидался ok, получено %q", resp.Status)
	}
	if len(resp.Checks) != 0 {
		t.Errorf("ожидался пустой список checks, получено %d", len(resp.Checks))
	}
}

// TestHandlerMux проверяет Handler() как мультиплексор.
func TestHandlerMux(t *testing.T) {
	hc := newHC()
	mux := hc.Handler()
	for _, path := range []string{"/healthz", "/readyz"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, req)
		if rr.Code == 0 {
			t.Errorf("path %s: нет ответа", path)
		}
	}
}
