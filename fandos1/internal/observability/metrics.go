package observability

// Файл реализует dependency-free Prometheus text-format registry (раздел 17.1).
// Формат exposition 0.0.4 (text/plain): counter, gauge, histogram.
// Все операции потокобезопасны (sync/atomic + sync.Mutex).

import (
	"fmt"
	"io"
	"math"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

// ============================================================
// Registry — центральный реестр метрик
// ============================================================

// Registry хранит все зарегистрированные метрики и экспортирует их через HTTP.
// Потокобезопасен для конкурентной регистрации и обновления метрик.
type Registry struct {
	mu       sync.Mutex
	families []*metricFamily
}

// NewRegistry создаёт пустой реестр.
func NewRegistry() *Registry {
	return &Registry{}
}

// Handler возвращает http.Handler, экспортирующий метрики в Prometheus text format 0.0.4.
func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		r.mu.Lock()
		families := make([]*metricFamily, len(r.families))
		copy(families, r.families)
		r.mu.Unlock()
		for _, f := range families {
			f.writeTo(w)
		}
	})
}

// ============================================================
// metricFamily — семейство метрик одного типа
// ============================================================

type metricKind int

const (
	kindCounter   metricKind = iota
	kindGauge                // значение float64, устанавливается атомарно
	kindHistogram            // наблюдения с bucket'ами
)

// metricFamily — группа метрик с общим именем и help.
type metricFamily struct {
	name    string
	help    string
	kind    metricKind
	buckets []float64 // только для histogram

	mu      sync.Mutex
	metrics map[string]*metric // ключ — строка label-сета
}

// metric — одна time series (конкретный набор label-значений).
type metric struct {
	labels map[string]string

	// counter
	counterVal atomic.Uint64

	// gauge (int64 bits для float64 атомарного хранения)
	gaugeVal atomic.Int64

	// histogram
	histMu      sync.Mutex
	histBuckets []atomic.Uint64 // len = len(buckets)+1 (последний — +Inf)
	histSum     atomic.Uint64   // bits float64
	histCount   atomic.Uint64
}

// ============================================================
// Counter
// ============================================================

// Counter — монотонно возрастающий счётчик (раздел 17.1).
type Counter struct {
	family *metricFamily
}

// Counter регистрирует или возвращает существующее семейство счётчиков.
// name — имя без суффиксов; labels — имена label'ов.
func (r *Registry) Counter(name, help string, labels ...string) *Counter {
	f := r.getOrCreate(name, help, kindCounter, nil)
	_ = labels // labels объявлены для документации; фактически используются при Inc
	return &Counter{family: f}
}

// Inc увеличивает счётчик на 1 для заданного набора label-значений.
// Порядок values должен совпадать с порядком labels при регистрации.
func (c *Counter) Inc(labelValues ...string) {
	m := c.family.getOrCreateMetric(labelValues)
	m.counterVal.Add(1)
}

// Add увеличивает счётчик на delta (delta >= 0).
func (c *Counter) Add(delta uint64, labelValues ...string) {
	m := c.family.getOrCreateMetric(labelValues)
	m.counterVal.Add(delta)
}

// ============================================================
// Gauge
// ============================================================

// Gauge — метрика с произвольным float64-значением (раздел 17.1).
type Gauge struct {
	family *metricFamily
}

// Gauge регистрирует или возвращает существующее семейство gauge-метрик.
func (r *Registry) Gauge(name, help string, labels ...string) *Gauge {
	f := r.getOrCreate(name, help, kindGauge, nil)
	_ = labels
	return &Gauge{family: f}
}

// Set устанавливает значение gauge для заданного набора label-значений.
func (g *Gauge) Set(value float64, labelValues ...string) {
	m := g.family.getOrCreateMetric(labelValues)
	m.gaugeVal.Store(int64(math.Float64bits(value)))
}

// ============================================================
// Histogram
// ============================================================

// Histogram — метрика с наблюдениями и bucket'ами (раздел 17.1).
type Histogram struct {
	family *metricFamily
}

// Histogram регистрирует семейство histogram-метрик с явными bucket'ами.
// buckets должны быть строго возрастающими; +Inf добавляется автоматически.
func (r *Registry) Histogram(name, help string, buckets []float64, labels ...string) *Histogram {
	sorted := make([]float64, len(buckets))
	copy(sorted, buckets)
	sort.Float64s(sorted)
	f := r.getOrCreate(name, help, kindHistogram, sorted)
	_ = labels
	return &Histogram{family: f}
}

// Observe добавляет одно наблюдение value в histogram.
func (h *Histogram) Observe(value float64, labelValues ...string) {
	m := h.family.getOrCreateMetric(labelValues)
	m.histMu.Lock()
	buckets := h.family.buckets
	for i, b := range buckets {
		if value <= b {
			m.histBuckets[i].Add(1)
		}
	}
	// +Inf bucket — всегда включает всё
	m.histBuckets[len(buckets)].Add(1)
	m.histMu.Unlock()
	// sum и count без мьютекса (не требуют атомарной согласованности с bucket'ами)
	addFloat64Atomic(&m.histSum, value)
	m.histCount.Add(1)
}

// ============================================================
// Вспомогательные методы
// ============================================================

func (r *Registry) getOrCreate(name, help string, kind metricKind, buckets []float64) *metricFamily {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, f := range r.families {
		if f.name == name {
			return f
		}
	}
	f := &metricFamily{
		name:    name,
		help:    help,
		kind:    kind,
		buckets: buckets,
		metrics: make(map[string]*metric),
	}
	r.families = append(r.families, f)
	return f
}

func (f *metricFamily) getOrCreateMetric(labelValues []string) *metric {
	key := strings.Join(labelValues, "\x00")
	f.mu.Lock()
	m, ok := f.metrics[key]
	if !ok {
		m = &metric{
			labels: parseLabelValues(labelValues),
		}
		if f.kind == kindHistogram {
			m.histBuckets = make([]atomic.Uint64, len(f.buckets)+1)
		}
		f.metrics[key] = m
	}
	f.mu.Unlock()
	return m
}

// parseLabelValues разбирает плоский список key=value (key0, val0, key1, val1, ...).
// Для метрик без label'ов возвращает nil.
func parseLabelValues(kv []string) map[string]string {
	if len(kv) == 0 {
		return nil
	}
	m := make(map[string]string, len(kv)/2)
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return m
}

// addFloat64Atomic складывает float64 в atomic.Uint64 через CAS.
func addFloat64Atomic(a *atomic.Uint64, delta float64) {
	for {
		old := a.Load()
		newVal := math.Float64bits(math.Float64frombits(old) + delta)
		if a.CompareAndSwap(old, newVal) {
			return
		}
	}
}

// ============================================================
// Экспорт в Prometheus text format 0.0.4
// ============================================================

func (f *metricFamily) writeTo(w io.Writer) {
	kindStr := ""
	switch f.kind {
	case kindCounter:
		kindStr = "counter"
	case kindGauge:
		kindStr = "gauge"
	case kindHistogram:
		kindStr = "histogram"
	}
	fmt.Fprintf(w, "# HELP %s %s\n", f.name, f.help)
	fmt.Fprintf(w, "# TYPE %s %s\n", f.name, kindStr)

	f.mu.Lock()
	keys := make([]string, 0, len(f.metrics))
	for k := range f.metrics {
		keys = append(keys, k)
	}
	f.mu.Unlock()
	sort.Strings(keys)

	for _, key := range keys {
		f.mu.Lock()
		m := f.metrics[key]
		f.mu.Unlock()

		switch f.kind {
		case kindCounter:
			val := m.counterVal.Load()
			fmt.Fprintf(w, "%s%s %d\n", f.name, formatLabels(m.labels), val)
		case kindGauge:
			bits := m.gaugeVal.Load()
			val := math.Float64frombits(uint64(bits))
			fmt.Fprintf(w, "%s%s %g\n", f.name, formatLabels(m.labels), val)
		case kindHistogram:
			count := m.histCount.Load()
			sumBits := m.histSum.Load()
			sum := math.Float64frombits(sumBits)
			// Bucket'ы уже кумулятивны: Observe добавляет наблюдение во все
			// bucket'ы с le >= value. Поэтому выводим напрямую без суммирования.
			for i, b := range f.buckets {
				le := formatFloat(b)
				lbls := mergeLabels(m.labels, "le", le)
				fmt.Fprintf(w, "%s_bucket%s %d\n", f.name, formatLabels(lbls), m.histBuckets[i].Load())
			}
			// +Inf bucket: count == total наблюдений.
			lblsInf := mergeLabels(m.labels, "le", "+Inf")
			fmt.Fprintf(w, "%s_bucket%s %d\n", f.name, formatLabels(lblsInf), count)
			fmt.Fprintf(w, "%s_sum%s %g\n", f.name, formatLabels(m.labels), sum)
			fmt.Fprintf(w, "%s_count%s %d\n", f.name, formatLabels(m.labels), count)
		}
	}
}

// formatLabels формирует строку {k="v",...} для Prometheus.
// Значения экранируются: \n → \\n, " → \\", \ → \\\\.
func formatLabels(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var sb strings.Builder
	sb.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(k)
		sb.WriteString(`="`)
		sb.WriteString(escapeLabelValue(labels[k]))
		sb.WriteByte('"')
	}
	sb.WriteByte('}')
	return sb.String()
}

// escapeLabelValue экранирует спецсимволы в значении label (Prometheus spec).
func escapeLabelValue(s string) string {
	var sb strings.Builder
	for _, c := range s {
		switch c {
		case '"':
			sb.WriteString(`\"`)
		case '\\':
			sb.WriteString(`\\`)
		case '\n':
			sb.WriteString(`\n`)
		default:
			sb.WriteRune(c)
		}
	}
	return sb.String()
}

// mergeLabels создаёт копию labels с дополнительной парой key=value.
func mergeLabels(labels map[string]string, key, value string) map[string]string {
	out := make(map[string]string, len(labels)+1)
	for k, v := range labels {
		out[k] = v
	}
	out[key] = value
	return out
}

// formatFloat форматирует float64 в читаемую строку для le label.
func formatFloat(f float64) string {
	if f == math.Inf(1) {
		return "+Inf"
	}
	// Используем %g для краткости (убирает лишние нули).
	return fmt.Sprintf("%g", f)
}

// ============================================================
// Предзарегистрированные метрики (раздел 17.1)
// ============================================================

// DefaultBuckets — стандартные bucket'ы для миллисекундных гистограмм задержек.
// Покрывают диапазон 0–10000 мс с плотными точками в «нормальной» зоне.
var DefaultBuckets = []float64{1, 5, 10, 25, 50, 100, 250, 500, 1000, 2500, 5000, 10000}

// AppMetrics — набор предзарегистрированных метрик приложения (раздел 17.1).
// Создаётся через NewAppMetrics; поля используются напрямую без строковых имён.
type AppMetrics struct {
	// Гистограммы задержек (раздел 17.1)

	// WsMessageLagMs — задержка WS-сообщения от биржи до обработки (мс).
	WsMessageLagMs *Histogram
	// OrderAckLatencyMs — время от отправки ордера до получения ACK (мс).
	OrderAckLatencyMs *Histogram
	// ExecutionLegSkewMs — разница времён исполнения двух leg'ов (мс).
	ExecutionLegSkewMs *Histogram

	// Gauge-метрики состояния (раздел 17.1)

	// DeltaMismatchDurationMs — длительность текущего delta mismatch (мс).
	DeltaMismatchDurationMs *Gauge
	// ScannerEligibleCandidates — число пар, прошедших предфильтр сканера.
	ScannerEligibleCandidates *Gauge
	// OutboxUnprocessed — число необработанных записей в outbox.
	OutboxUnprocessed *Gauge
	// ClockOffsetMs — текущий offset clock (NTP) в мс.
	ClockOffsetMs *Gauge

	// Счётчики событий (раздел 17.1)

	// CircuitBreakerTripsTotal — число срабатываний circuit breaker.
	CircuitBreakerTripsTotal *Counter
	// OrdersPlacedTotal — число выставленных ордеров; labels: exchange, mode.
	OrdersPlacedTotal *Counter
	// ReconnectsTotal — число реконнектов WS; label: exchange.
	ReconnectsTotal *Counter
}

// NewAppMetrics создаёт и регистрирует все предзаданные метрики в реестре r.
func NewAppMetrics(r *Registry) *AppMetrics {
	return &AppMetrics{
		WsMessageLagMs: r.Histogram(
			"ws_message_lag_ms",
			"Задержка WS-сообщения от биржи до обработки (мс)",
			DefaultBuckets,
			"exchange",
		),
		OrderAckLatencyMs: r.Histogram(
			"order_ack_latency_ms",
			"Время от отправки ордера до получения ACK (мс)",
			DefaultBuckets,
			"exchange",
		),
		ExecutionLegSkewMs: r.Histogram(
			"execution_leg_skew_ms",
			"Разница времён исполнения между leg'ами пары (мс)",
			DefaultBuckets,
		),
		DeltaMismatchDurationMs: r.Gauge(
			"delta_mismatch_duration_ms",
			"Длительность текущего delta mismatch (мс)",
			"position_id",
		),
		ScannerEligibleCandidates: r.Gauge(
			"scanner_eligible_candidates",
			"Число пар, прошедших предфильтр сканера",
		),
		OutboxUnprocessed: r.Gauge(
			"outbox_unprocessed",
			"Число необработанных записей в outbox",
		),
		ClockOffsetMs: r.Gauge(
			"clock_offset_ms",
			"Текущий clock offset по NTP (мс); alert при превышении MAX_CLOCK_OFFSET_MS",
		),
		CircuitBreakerTripsTotal: r.Counter(
			"circuit_breaker_trips_total",
			"Число срабатываний circuit breaker",
			"breaker",
		),
		OrdersPlacedTotal: r.Counter(
			"orders_placed_total",
			"Число выставленных ордеров",
			"exchange", "mode",
		),
		ReconnectsTotal: r.Counter(
			"reconnects_total",
			"Число реконнектов WebSocket-соединения к бирже",
			"exchange",
		),
	}
}

// ============================================================
// Вспомогательные экспортируемые функции для слоя выше
// ============================================================

// SortedKeys возвращает отсортированные ключи map'ы (удобно для детерминированного вывода).
func SortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
