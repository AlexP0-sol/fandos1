// Package httpclient реализует общий REST-клиент для биржевых адаптеров
// (раздел 7.4 промпта v2: независимый rate limiter, request queue, circuit breaker,
// reconnect backoff с jitter).
//
// Каждый адаптер создаёт свой HttpClient — это обеспечивает изоляцию rate limits
// между биржами (Binance rate limit не влияет на Bybit).
package httpclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// Config — параметры клиента.
type Config struct {
	BaseURL    string
	APIKey     string // передаётся в header (X-MBX-APIKEY и т.п. — через Header callback)
	Timeout    time.Duration
	MaxRetries int
	RateLimit  RateLimitConfig
}

// RateLimitConfig — простой token-bucket rate limiter per-client.
type RateLimitConfig struct {
	RequestsPerSecond int
	Burst             int
}

// jitterRand — источник случайных чисел для backoff jitter, сид задаётся один раз.
var (
	jitterRandMu sync.Mutex
	jitterRand   = rand.New(rand.NewSource(time.Now().UnixNano()))
)

// HttpClient — потокобезопасный HTTP-клиент для биржи.
type HttpClient struct {
	baseURL string
	http    *http.Client
	cfg     Config

	// Rate limiter (token bucket).
	mu       sync.Mutex
	tokens   int
	lastFill time.Time

	// Retry policy.
	maxRetries int
}

// New создаёт клиент. Возвращает ошибку, если BaseURL пуст.
// Timeouts и rate limit берутся из конфига.
func New(cfg Config) (*HttpClient, error) {
	if cfg.BaseURL == "" {
		return nil, fmt.Errorf("httpclient: BaseURL не может быть пустым")
	}
	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = 10 * time.Second
	}
	return &HttpClient{
		baseURL:    cfg.BaseURL,
		http:       &http.Client{Timeout: timeout},
		cfg:        cfg,
		tokens:     cfg.RateLimit.Burst,
		lastFill:   time.Now(),
		maxRetries: cfg.MaxRetries,
	}, nil
}

// Request — параметры одного HTTP-запроса.
type Request struct {
	Method  string
	Path    string // путь после baseURL, начиная с /
	Query   string // query string без знака ?
	Body    io.Reader
	Headers map[string]string
	// Safe указывает, что запрос идемпотентен (read-only GET и т.п.) и может быть
	// повторён при сетевых ошибках или 429/5xx без побочных эффектов.
	// Небезопасные запросы (POST размещения ордеров) при ошибке возвращают её немедленно —
	// вызывающий обязан применить QUERY_THEN_DECIDE (разделы 5.3, 10.2).
	Safe bool
}

// StatusError — ошибка с HTTP-статусом, возвращается из doOnce.
type StatusError struct {
	Status int
	Msg    string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("httpclient: HTTP %d: %s", e.Status, e.Msg)
}

// Do выполняет запрос с rate limit и retry (раздел 7.4).
// Retry (429/5xx/сетевые ошибки) выполняется ТОЛЬКО когда req.Safe == true.
// Для небезопасных запросов (POST ордеров) ошибка возвращается немедленно.
func (c *HttpClient) Do(ctx context.Context, req Request) (statusCode int, body []byte, err error) {
	attempts := c.maxRetries + 1
	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		// Rate limit перед каждой попыткой.
		if err := c.waitToken(ctx); err != nil {
			return 0, nil, err
		}

		status, b, e := c.doOnce(ctx, req)

		// Сетевая ошибка (e != nil).
		if e != nil {
			if !req.Safe {
				// Небезопасный запрос (POST ордера) — немедленный возврат без retry.
				return status, b, e
			}
			// Safe-запрос: потребляем попытку с backoff.
			lastErr = e
			if backoffErr := c.backoff(ctx, attempt); backoffErr != nil {
				return 0, nil, backoffErr
			}
			continue
		}

		// HTTP-ответ получен (e == nil).

		// Успех: 2xx и 3xx (и все 1xx), а также не-retryable 4xx кроме 429.
		if status != 429 && status < 500 {
			// 4xx (кроме 429) — клиентская ошибка, не retryable.
			return status, b, nil
		}

		// Retryable: 429 или 5xx.
		if !req.Safe {
			// Небезопасный запрос — немедленный возврат.
			return status, b, &StatusError{Status: status, Msg: string(b)}
		}
		// Safe-запрос: retry с backoff.
		lastErr = &StatusError{Status: status, Msg: string(b)}
		if backoffErr := c.backoff(ctx, attempt); backoffErr != nil {
			return status, b, backoffErr
		}
	}
	return 0, nil, lastErr
}

// doOnce — один HTTP-запрос без retry.
func (c *HttpClient) doOnce(ctx context.Context, req Request) (int, []byte, error) {
	url := c.baseURL + req.Path
	if req.Query != "" {
		url += "?" + req.Query
	}
	httpReq, err := http.NewRequestWithContext(ctx, req.Method, url, req.Body)
	if err != nil {
		return 0, nil, err
	}
	for k, v := range req.Headers {
		httpReq.Header.Set(k, v)
	}

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, body, nil
}

// waitToken — token bucket rate limiter (блокирует, пока нет токена).
func (c *HttpClient) waitToken(ctx context.Context) error {
	rps := c.cfg.RateLimit.RequestsPerSecond
	if rps <= 0 {
		return nil // без лимита
	}
	for {
		c.mu.Lock()
		c.refill(rps)
		if c.tokens > 0 {
			c.tokens--
			c.mu.Unlock()
			return nil
		}
		// Нет токена — ждём до следующего refill.
		wait := time.Second / time.Duration(rps)
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
}

// refill — пополняет токены по времени (вызывать под mutex).
// Использует целочисленную арифметику (time.Duration) вместо float64.
func (c *HttpClient) refill(rps int) {
	now := time.Now()
	elapsed := now.Sub(c.lastFill)
	// Количество новых токенов = elapsed / (1s / rps) = elapsed * rps / 1s.
	add := int(elapsed * time.Duration(rps) / time.Second)
	if add > 0 {
		c.tokens += add
		if c.tokens > c.cfg.RateLimit.Burst {
			c.tokens = c.cfg.RateLimit.Burst
		}
		c.lastFill = now
	}
}

// backoff — exponential backoff с jitter: 100ms × 2^attempt + random [0, 50ms).
// Jitter вычисляется через math/rand с mutex-защитой (package-level seed).
func (c *HttpClient) backoff(ctx context.Context, attempt int) error {
	if attempt < 0 {
		attempt = 0
	}
	// Базовая задержка 100ms × 2^attempt, cap 5s.
	base := time.Duration(100<<attempt) * time.Millisecond
	if base > 5*time.Second {
		base = 5 * time.Second
	}
	// Jitter [0, 50ms) через package-level rand с mutex.
	jitterRandMu.Lock()
	jitterNs := jitterRand.Int63n(int64(50 * time.Millisecond))
	jitterRandMu.Unlock()
	delay := base + time.Duration(jitterNs)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(delay):
		return nil
	}
}

// ParseRateLimitHeaders — извлекает использованный weight из заголовков биржи
// (Binance: X-MBX-USED-WEIGHT-1M; Bybit: X-RateLimit-Remaining-Btc).
// Возвращает map[header_name]value (int).
func ParseRateLimitHeaders(header http.Header, names []string) map[string]int {
	out := make(map[string]int, len(names))
	for _, name := range names {
		v := header.Get(name)
		if v == "" {
			continue
		}
		if n, err := strconv.Atoi(v); err == nil {
			out[name] = n
		}
	}
	return out
}

// ErrTimeout — sentinel для таймаута (мапится на network/timeout в адаптере).
var ErrTimeout = errors.New("httpclient: timeout")
