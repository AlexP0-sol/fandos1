package observability

// Файл реализует health check framework (раздел 17.3 промпта v2).
// /healthz — liveness: процесс жив → всегда 200 с JSON всех проверок.
// /readyz  — readiness: любая CRITICAL проверка провалена → 503 + JSON.

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

// ============================================================
// Типы и структуры
// ============================================================

// CheckFunc — функция проверки здоровья одного компонента.
// Должна завершиться в пределах timeout (управляется контекстом).
type CheckFunc func(ctx context.Context) error

// CheckResult — результат одной проверки (раздел 17.3: JSON shape).
type CheckResult struct {
	Name      string  `json:"name"`
	OK        bool    `json:"ok"`
	Critical  bool    `json:"critical"`
	Error     string  `json:"error,omitempty"` // заполняется только при OK=false
	LatencyMs float64 `json:"latency_ms"`
}

// HealthResponse — ответ /healthz и /readyz (раздел 17.3: JSON shape).
type HealthResponse struct {
	// Status: "ok" или "degraded" (не critical) или "unhealthy" (critical fail).
	Status string        `json:"status"`
	Checks []CheckResult `json:"checks"`
}

// checkEntry — внутренняя запись зарегистрированной проверки.
type checkEntry struct {
	name     string
	critical bool
	fn       CheckFunc
}

// ============================================================
// HealthChecker
// ============================================================

// defaultCheckTimeout — таймаут по умолчанию для каждой проверки (раздел 17.3).
const defaultCheckTimeout = 2 * time.Second

// HealthChecker управляет набором health checks и предоставляет HTTP handlers.
// Потокобезопасен для конкурентной регистрации и запросов.
type HealthChecker struct {
	mu           sync.RWMutex
	checks       []checkEntry
	checkTimeout time.Duration
}

// NewHealthChecker создаёт HealthChecker с таймаутом 2s по умолчанию.
func NewHealthChecker() *HealthChecker {
	return &HealthChecker{checkTimeout: defaultCheckTimeout}
}

// NewHealthCheckerWithTimeout создаёт HealthChecker с кастомным таймаутом per-check.
func NewHealthCheckerWithTimeout(timeout time.Duration) *HealthChecker {
	return &HealthChecker{checkTimeout: timeout}
}

// RegisterCheck регистрирует проверку с именем name.
// critical=true означает, что провал этой проверки переводит /readyz в 503.
// Проверки выполняются в порядке регистрации при каждом запросе.
//
// Примеры проверок (раздел 17.3) — регистрируются в app-пакете:
//   - "db"          critical=true  — доступность базы данных
//   - "master_key"  critical=true  — возможность расшифровки ключей
//   - "clock"       critical=true  — clock offset в допуске
//   - "adapters"    critical=true  — exchange adapters healthy
//   - "private_ws"  critical=true  — private WS для открытых позиций живы
//   - "incidents"   critical=true  — нет блокирующего recovery incident
//   - "breakers"    critical=false — circuit breakers не активны
func (hc *HealthChecker) RegisterCheck(name string, critical bool, fn CheckFunc) {
	hc.mu.Lock()
	defer hc.mu.Unlock()
	hc.checks = append(hc.checks, checkEntry{
		name:     name,
		critical: critical,
		fn:       fn,
	})
}

// runAll выполняет все зарегистрированные проверки параллельно с таймаутом.
func (hc *HealthChecker) runAll(ctx context.Context) []CheckResult {
	hc.mu.RLock()
	entries := make([]checkEntry, len(hc.checks))
	copy(entries, hc.checks)
	hc.mu.RUnlock()

	results := make([]CheckResult, len(entries))
	var wg sync.WaitGroup
	for i, entry := range entries {
		wg.Add(1)
		go func(idx int, e checkEntry) {
			defer wg.Done()
			// Каждая проверка получает собственный контекст с таймаутом.
			checkCtx, cancel := context.WithTimeout(ctx, hc.checkTimeout)
			defer cancel()
			start := time.Now()
			err := e.fn(checkCtx)
			latencyMs := float64(time.Since(start).Microseconds()) / 1000.0
			r := CheckResult{
				Name:      e.name,
				Critical:  e.critical,
				LatencyMs: latencyMs,
			}
			if err != nil {
				r.OK = false
				r.Error = err.Error()
			} else {
				r.OK = true
			}
			results[idx] = r
		}(i, entry)
	}
	wg.Wait()
	return results
}

// buildResponse формирует HealthResponse и признак hasCriticalFailure.
func buildResponse(results []CheckResult) (HealthResponse, bool) {
	hasCriticalFailure := false
	hasAnyFailure := false
	for _, r := range results {
		if !r.OK {
			hasAnyFailure = true
			if r.Critical {
				hasCriticalFailure = true
			}
		}
	}
	status := "ok"
	if hasCriticalFailure {
		status = "unhealthy"
	} else if hasAnyFailure {
		status = "degraded"
	}
	return HealthResponse{Status: status, Checks: results}, hasCriticalFailure
}

// ============================================================
// HTTP Handlers
// ============================================================

// Handler возвращает мультиплексор с /healthz и /readyz маршрутами.
// Можно использовать вместо регистрации на внешнем mux:
//
//	http.Handle("/healthz", hc.LivenessHandler())
//	http.Handle("/readyz",  hc.ReadinessHandler())
func (hc *HealthChecker) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/healthz", hc.LivenessHandler())
	mux.Handle("/readyz", hc.ReadinessHandler())
	return mux
}

// LivenessHandler возвращает handler для /healthz.
// Всегда отвечает 200 OK — «процесс жив» (liveness probe).
// Возвращает JSON со статусом всех проверок для операционного удобства.
func (hc *HealthChecker) LivenessHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		results := hc.runAll(r.Context())
		resp, _ := buildResponse(results)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(resp)
	})
}

// ReadinessHandler возвращает handler для /readyz.
// 200 OK если все CRITICAL проверки успешны.
// 503 Service Unavailable если хотя бы одна CRITICAL провалена.
// Используется Kubernetes/Docker readiness probe и load balancer'ами.
func (hc *HealthChecker) ReadinessHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		results := hc.runAll(r.Context())
		resp, hasCriticalFailure := buildResponse(results)
		w.Header().Set("Content-Type", "application/json")
		if hasCriticalFailure {
			w.WriteHeader(http.StatusServiceUnavailable)
		} else {
			w.WriteHeader(http.StatusOK)
		}
		_ = json.NewEncoder(w).Encode(resp)
	})
}
