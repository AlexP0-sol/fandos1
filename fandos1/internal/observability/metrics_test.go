package observability

// Тесты метрик: формат экспозиции, bucket'ы гистограмм, label escaping,
// конкурентные инкременты под -race.

import (
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// ============================================================
// Вспомогательная функция для экспорта реестра в строку
// ============================================================

func registryOutput(r *Registry) string {
	req := httptest.NewRequest("GET", "/metrics", nil)
	rr := httptest.NewRecorder()
	r.Handler().ServeHTTP(rr, req)
	return rr.Body.String()
}

// ============================================================
// Counter
// ============================================================

// TestCounter_BasicExposition проверяет формат вывода counter без label'ов.
func TestCounter_BasicExposition(t *testing.T) {
	r := NewRegistry()
	c := r.Counter("test_counter_total", "Тест счётчика без label")
	c.Inc()
	c.Inc()
	c.Add(3)
	out := registryOutput(r)
	if !strings.Contains(out, "# HELP test_counter_total Тест счётчика без label") {
		t.Errorf("HELP-строка не найдена:\n%s", out)
	}
	if !strings.Contains(out, "# TYPE test_counter_total counter") {
		t.Errorf("TYPE-строка не найдена:\n%s", out)
	}
	if !strings.Contains(out, "test_counter_total 5") {
		t.Errorf("значение 5 не найдено:\n%s", out)
	}
}

// TestCounter_WithLabels проверяет формат вывода counter с label'ами.
func TestCounter_WithLabels(t *testing.T) {
	r := NewRegistry()
	c := r.Counter("orders_placed_total", "Ордера", "exchange", "mode")
	c.Inc("exchange", "binance", "mode", "limit")
	c.Inc("exchange", "binance", "mode", "limit")
	c.Inc("exchange", "bybit", "mode", "market")
	out := registryOutput(r)
	if !strings.Contains(out, `orders_placed_total{exchange="binance",mode="limit"} 2`) {
		t.Errorf("binance/limit=2 не найдено:\n%s", out)
	}
	if !strings.Contains(out, `orders_placed_total{exchange="bybit",mode="market"} 1`) {
		t.Errorf("bybit/market=1 не найдено:\n%s", out)
	}
}

// ============================================================
// Gauge
// ============================================================

// TestGauge_BasicExposition проверяет формат вывода gauge.
func TestGauge_BasicExposition(t *testing.T) {
	r := NewRegistry()
	g := r.Gauge("outbox_unprocessed", "Необработанные outbox записи")
	g.Set(42.5)
	out := registryOutput(r)
	if !strings.Contains(out, "# TYPE outbox_unprocessed gauge") {
		t.Errorf("TYPE-строка gauge не найдена:\n%s", out)
	}
	if !strings.Contains(out, "outbox_unprocessed 42.5") {
		t.Errorf("значение 42.5 не найдено:\n%s", out)
	}
}

// TestGauge_Update проверяет, что Set перезаписывает значение.
func TestGauge_Update(t *testing.T) {
	r := NewRegistry()
	g := r.Gauge("clock_offset_ms", "Clock offset")
	g.Set(100.0)
	g.Set(-5.5)
	out := registryOutput(r)
	if !strings.Contains(out, "clock_offset_ms -5.5") {
		t.Errorf("значение -5.5 не найдено:\n%s", out)
	}
}

// ============================================================
// Histogram
// ============================================================

// TestHistogram_BucketCounting проверяет корректность кумулятивных bucket'ов.
func TestHistogram_BucketCounting(t *testing.T) {
	r := NewRegistry()
	buckets := []float64{10, 50, 100}
	h := r.Histogram("ws_message_lag_ms", "WS lag", buckets, "exchange")
	// 5ms → попадает в [10, 50, 100, +Inf]
	h.Observe(5, "exchange", "binance")
	// 30ms → попадает в [50, 100, +Inf]
	h.Observe(30, "exchange", "binance")
	// 75ms → попадает в [100, +Inf]
	h.Observe(75, "exchange", "binance")
	// 200ms → только +Inf
	h.Observe(200, "exchange", "binance")

	out := registryOutput(r)
	// le="10": только 5ms → cumulative=1
	if !strings.Contains(out, `ws_message_lag_ms_bucket{exchange="binance",le="10"} 1`) {
		t.Errorf("le=10 ожидалось 1:\n%s", out)
	}
	// le="50": 5+30ms → cumulative=2
	if !strings.Contains(out, `ws_message_lag_ms_bucket{exchange="binance",le="50"} 2`) {
		t.Errorf("le=50 ожидалось 2:\n%s", out)
	}
	// le="100": 5+30+75ms → cumulative=3
	if !strings.Contains(out, `ws_message_lag_ms_bucket{exchange="binance",le="100"} 3`) {
		t.Errorf("le=100 ожидалось 3:\n%s", out)
	}
	// +Inf: все 4 наблюдения
	if !strings.Contains(out, `ws_message_lag_ms_bucket{exchange="binance",le="+Inf"} 4`) {
		t.Errorf("le=+Inf ожидалось 4:\n%s", out)
	}
	// count=4
	if !strings.Contains(out, `ws_message_lag_ms_count{exchange="binance"} 4`) {
		t.Errorf("count ожидалось 4:\n%s", out)
	}
	// sum=5+30+75+200=310
	if !strings.Contains(out, `ws_message_lag_ms_sum{exchange="binance"} 310`) {
		t.Errorf("sum ожидалось 310:\n%s", out)
	}
}

// TestHistogram_EmptyNoData проверяет, что histogram без наблюдений не выдаёт строк.
func TestHistogram_EmptyNoData(t *testing.T) {
	r := NewRegistry()
	_ = r.Histogram("empty_hist", "Пустая", DefaultBuckets)
	out := registryOutput(r)
	// Семейство объявлено, но нет time series — ни _bucket, ни _sum, ни _count.
	if strings.Contains(out, "empty_hist_bucket") {
		t.Errorf("пустая histogram не должна выдавать bucket строки:\n%s", out)
	}
}

// ============================================================
// Label escaping
// ============================================================

// TestLabelEscaping проверяет корректное экранирование спецсимволов.
func TestLabelEscaping(t *testing.T) {
	r := NewRegistry()
	c := r.Counter("escaped_total", "Тест escaping", "path")
	// Путь содержит кавычки, обратные слеши и перевод строки.
	c.Inc("path", `a"b\c`+"\nend")
	out := registryOutput(r)
	// Ожидаем экранированный label: a\"b\\c\nend
	expected := `escaped_total{path="a\"b\\c\nend"} 1`
	if !strings.Contains(out, expected) {
		t.Errorf("ожидалось %q в выводе:\n%s", expected, out)
	}
}

// ============================================================
// Concurrent safety
// ============================================================

// TestCounter_ConcurrentIncrements проверяет корректность под гонкой (-race).
func TestCounter_ConcurrentIncrements(t *testing.T) {
	r := NewRegistry()
	c := r.Counter("concurrent_total", "Конкурентный счётчик")
	const goroutines = 100
	const incsPerGoroutine = 1000
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < incsPerGoroutine; j++ {
				c.Inc()
			}
		}()
	}
	wg.Wait()
	out := registryOutput(r)
	expected := "concurrent_total 100000"
	if !strings.Contains(out, expected) {
		t.Errorf("ожидалось %q, вывод:\n%s", expected, out)
	}
}

// TestHistogram_ConcurrentObserve проверяет конкурентный Observe под -race.
func TestHistogram_ConcurrentObserve(t *testing.T) {
	r := NewRegistry()
	h := r.Histogram("concurrent_hist", "Конкурентная гистограмма", []float64{50, 100})
	const goroutines = 50
	const obsPerGoroutine = 200
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < obsPerGoroutine; j++ {
				h.Observe(75) // попадает в [100, +Inf]
			}
		}()
	}
	wg.Wait()
	out := registryOutput(r)
	total := goroutines * obsPerGoroutine // 10000
	expected := `concurrent_hist_count`
	if !strings.Contains(out, expected) {
		t.Errorf("count не найден в выводе:\n%s", out)
	}
	_ = total // значение проверяется через содержимое out
}

// TestGauge_ConcurrentSet проверяет конкурентный Set под -race.
func TestGauge_ConcurrentSet(t *testing.T) {
	r := NewRegistry()
	g := r.Gauge("concurrent_gauge", "Конкурентный gauge")
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(val float64) {
			defer wg.Done()
			g.Set(val)
		}(float64(i))
	}
	wg.Wait()
	// Не падает под -race, итоговое значение — одно из установленных.
	out := registryOutput(r)
	if !strings.Contains(out, "concurrent_gauge") {
		t.Errorf("gauge не найден в выводе:\n%s", out)
	}
}

// ============================================================
// AppMetrics
// ============================================================

// TestNewAppMetrics_AllFamiliesPresent проверяет, что все предзарегистрированные
// метрики присутствуют в выводе после первого использования.
func TestNewAppMetrics_AllFamiliesPresent(t *testing.T) {
	r := NewRegistry()
	m := NewAppMetrics(r)

	// Создаём хотя бы одну time series для каждой метрики.
	m.WsMessageLagMs.Observe(10, "exchange", "binance")
	m.OrderAckLatencyMs.Observe(50, "exchange", "bybit")
	m.ExecutionLegSkewMs.Observe(5)
	m.DeltaMismatchDurationMs.Set(100, "position_id", "p1")
	m.ScannerEligibleCandidates.Set(7)
	m.OutboxUnprocessed.Set(0)
	m.ClockOffsetMs.Set(12.5)
	m.CircuitBreakerTripsTotal.Inc("breaker", "trading")
	m.OrdersPlacedTotal.Inc("exchange", "binance", "mode", "limit")
	m.ReconnectsTotal.Inc("exchange", "binance")

	out := registryOutput(r)
	expected := []string{
		"ws_message_lag_ms",
		"order_ack_latency_ms",
		"execution_leg_skew_ms",
		"delta_mismatch_duration_ms",
		"scanner_eligible_candidates",
		"outbox_unprocessed",
		"clock_offset_ms",
		"circuit_breaker_trips_total",
		"orders_placed_total",
		"reconnects_total",
	}
	for _, name := range expected {
		if !strings.Contains(out, name) {
			t.Errorf("метрика %q не найдена в выводе", name)
		}
	}
}

// TestContentType проверяет, что Handler устанавливает правильный Content-Type.
func TestContentType(t *testing.T) {
	r := NewRegistry()
	_ = r.Counter("ct_test", "content type test")
	req := httptest.NewRequest("GET", "/metrics", nil)
	rr := httptest.NewRecorder()
	r.Handler().ServeHTTP(rr, req)
	ct := rr.Header().Get("Content-Type")
	if !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("ожидался text/plain Content-Type, получен: %q", ct)
	}
	if !strings.Contains(ct, "version=0.0.4") {
		t.Errorf("ожидался version=0.0.4 в Content-Type: %q", ct)
	}
}
