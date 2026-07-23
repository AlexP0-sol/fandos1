package httpclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// mustNew — вспомогательный конструктор для тестов: паникует при ошибке.
func mustNew(cfg Config) *HttpClient {
	c, err := New(cfg)
	if err != nil {
		panic(err)
	}
	return c
}

// TestNewEmptyBaseURL — New возвращает ошибку при пустом BaseURL.
func TestNewEmptyBaseURL(t *testing.T) {
	_, err := New(Config{BaseURL: ""})
	if err == nil {
		t.Fatal("ожидалась ошибка при пустом BaseURL")
	}
}

// TestNewValidBaseURL — New не возвращает ошибку при заполненном BaseURL.
func TestNewValidBaseURL(t *testing.T) {
	_, err := New(Config{BaseURL: "http://example.com"})
	if err != nil {
		t.Fatalf("неожиданная ошибка: %v", err)
	}
}

// TestDoSuccess — 200 OK возвращает тело.
func TestDoSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	c := mustNew(Config{BaseURL: srv.URL, Timeout: 2 * time.Second})
	status, body, err := c.Do(context.Background(), Request{Method: "GET", Path: "/test"})
	if err != nil {
		t.Fatal(err)
	}
	if status != 200 {
		t.Errorf("status = %d, want 200", status)
	}
	if string(body) != `{"ok":true}` {
		t.Errorf("body = %q", string(body))
	}
}

// TestDoRetriesOn5xx — Safe=true, 5xx триггерит retry.
func TestDoRetriesOn5xx(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if n < 3 {
			w.WriteHeader(500)
			return
		}
		w.WriteHeader(200)
		w.Write([]byte("ok"))
	}))
	defer srv.Close()

	c := mustNew(Config{BaseURL: srv.URL, Timeout: 2 * time.Second, MaxRetries: 3})
	_, _, err := c.Do(context.Background(), Request{Method: "GET", Path: "/", Safe: true})
	if err != nil {
		t.Fatalf("expected eventual success, got %v", err)
	}
	if calls.Load() < 3 {
		t.Errorf("expected ≥3 calls (retry), got %d", calls.Load())
	}
}

// TestDoNoRetryOn4xx — 4xx не retry даже при Safe=true.
func TestDoNoRetryOn4xx(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(400)
		w.Write([]byte(`{"err":"bad"}`))
	}))
	defer srv.Close()

	c := mustNew(Config{BaseURL: srv.URL, Timeout: 2 * time.Second, MaxRetries: 3})
	status, _, err := c.Do(context.Background(), Request{Method: "GET", Path: "/", Safe: true})
	if err != nil {
		t.Fatal(err)
	}
	if status != 400 {
		t.Errorf("status = %d, want 400", status)
	}
	if calls.Load() != 1 {
		t.Errorf("4xx should not retry, calls = %d", calls.Load())
	}
}

// TestDoRetriesOn429 — Safe=true, rate limit от сервера триггерит retry.
func TestDoRetriesOn429(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if n == 1 {
			w.WriteHeader(429)
			return
		}
		w.WriteHeader(200)
		w.Write([]byte("ok"))
	}))
	defer srv.Close()

	c := mustNew(Config{BaseURL: srv.URL, Timeout: 2 * time.Second, MaxRetries: 3})
	_, _, err := c.Do(context.Background(), Request{Method: "GET", Path: "/", Safe: true})
	if err != nil {
		t.Fatalf("expected retry success, got %v", err)
	}
	if calls.Load() < 2 {
		t.Errorf("expected ≥2 calls, got %d", calls.Load())
	}
}

// TestPostNotRetried — Safe=false, один 500 → ровно 1 вызов, ошибка возвращается.
func TestPostNotRetried(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(500)
		w.Write([]byte(`{"err":"server error"}`))
	}))
	defer srv.Close()

	c := mustNew(Config{BaseURL: srv.URL, Timeout: 2 * time.Second, MaxRetries: 3})
	status, _, err := c.Do(context.Background(), Request{Method: "POST", Path: "/order", Safe: false})
	// Небезопасный запрос возвращает StatusError немедленно.
	if err == nil {
		t.Fatal("ожидалась ошибка для 500 при Safe=false")
	}
	if status != 500 {
		t.Errorf("status = %d, want 500", status)
	}
	if calls.Load() != 1 {
		t.Errorf("POST не должен retry, calls = %d, want 1", calls.Load())
	}
}

// TestNetworkErrorRetriedForSafe — Safe=true, сетевая ошибка потребляет попытки retry.
func TestNetworkErrorRetriedForSafe(t *testing.T) {
	var calls atomic.Int64
	// Сервер принимает соединение, но сразу закрывает его (имитация network error).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if n < 3 {
			// Принудительно закрыть соединение — вызовет network error у клиента.
			hj, ok := w.(http.Hijacker)
			if ok {
				conn, _, _ := hj.Hijack()
				if conn != nil {
					conn.Close()
				}
				return
			}
			// Fallback: 500.
			w.WriteHeader(500)
			return
		}
		w.WriteHeader(200)
		w.Write([]byte("ok"))
	}))
	defer srv.Close()

	c := mustNew(Config{BaseURL: srv.URL, Timeout: 2 * time.Second, MaxRetries: 5})
	_, _, err := c.Do(context.Background(), Request{Method: "GET", Path: "/", Safe: true})
	// Может завершиться успехом или ошибкой в зависимости от реализации hijack;
	// главное — было больше 1 вызова.
	_ = err
	if calls.Load() < 2 {
		t.Errorf("Safe=true сетевые ошибки должны повторяться, calls = %d", calls.Load())
	}
}

// TestConcurrentDo — параллельные Do под -race не ломают состояние.
func TestConcurrentDo(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte("ok"))
	}))
	defer srv.Close()

	c := mustNew(Config{
		BaseURL:   srv.URL,
		Timeout:   2 * time.Second,
		RateLimit: RateLimitConfig{RequestsPerSecond: 100, Burst: 10},
	})

	done := make(chan struct{})
	for i := 0; i < 20; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			c.Do(context.Background(), Request{Method: "GET", Path: "/"})
		}()
	}
	for i := 0; i < 20; i++ {
		<-done
	}
}

// TestDoContextCancelled — ctx.Done прерывает запрос.
func TestDoContextCancelled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	c := mustNew(Config{BaseURL: srv.URL, Timeout: 5 * time.Second})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, _, err := c.Do(ctx, Request{Method: "GET", Path: "/"})
	if err == nil {
		t.Error("expected ctx cancellation error")
	}
}

// TestRateLimit — token bucket ограничивает частоту.
func TestRateLimit(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	c := mustNew(Config{
		BaseURL: srv.URL, Timeout: 2 * time.Second,
		RateLimit: RateLimitConfig{RequestsPerSecond: 10, Burst: 2},
	})
	// Посылаем 5 быстрых запросов; rate limiter должен их замедлить.
	start := time.Now()
	for i := 0; i < 5; i++ {
		_, _, _ = c.Do(context.Background(), Request{Method: "GET", Path: "/"})
	}
	elapsed := time.Since(start)
	if calls.Load() != 5 {
		t.Errorf("calls = %d, want 5", calls.Load())
	}
	// При rps=10, 5 запросов (burst=2 + 3 через rate) занимают ≥ ~300ms.
	if elapsed < 200*time.Millisecond {
		t.Errorf("rate limiter too fast: %v (expected ≥ 200ms)", elapsed)
	}
}

// TestHeadersPassed — headers из Request передаются серверу.
func TestHeadersPassed(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.WriteHeader(200)
	}))
	defer srv.Close()

	c := mustNew(Config{BaseURL: srv.URL, Timeout: 2 * time.Second})
	_, _, err := c.Do(context.Background(), Request{
		Method:  "GET",
		Path:    "/",
		Headers: map[string]string{"X-Custom": "value", "X-MBX-APIKEY": "key123"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Get("X-Custom") != "value" {
		t.Errorf("X-Custom = %q", got.Get("X-Custom"))
	}
	if got.Get("X-MBX-APIKEY") != "key123" {
		t.Errorf("API key header missing")
	}
}

// TestQueryString — query конкатенируется в URL.
func TestQueryString(t *testing.T) {
	var gotURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURL = r.URL.String()
		w.WriteHeader(200)
	}))
	defer srv.Close()

	c := mustNew(Config{BaseURL: srv.URL, Timeout: 2 * time.Second})
	_, _, err := c.Do(context.Background(), Request{
		Method: "GET",
		Path:   "/test",
		Query:  "symbol=BTCUSDT&limit=10",
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotURL != "/test?symbol=BTCUSDT&limit=10" {
		t.Errorf("URL = %q", gotURL)
	}
}

// TestParseRateLimitHeaders — извлечение X-MBX-USED-WEIGHT.
func TestParseRateLimitHeaders(t *testing.T) {
	h := http.Header{}
	h.Set("X-MBX-USED-WEIGHT-1M", "1200")
	h.Set("X-RateLimit-Remaining", "50")
	h.Set("X-Empty", "")
	out := ParseRateLimitHeaders(h, []string{"X-MBX-USED-WEIGHT-1M", "X-RateLimit-Remaining", "X-Empty", "X-Missing"})
	if out["X-MBX-USED-WEIGHT-1M"] != 1200 {
		t.Errorf("weight = %d, want 1200", out["X-MBX-USED-WEIGHT-1M"])
	}
	if out["X-RateLimit-Remaining"] != 50 {
		t.Errorf("remaining = %d, want 50", out["X-RateLimit-Remaining"])
	}
	if _, ok := out["X-Empty"]; ok {
		t.Error("empty header should be skipped")
	}
	if _, ok := out["X-Missing"]; ok {
		t.Error("missing header should be skipped")
	}
}
