package binance

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

// TestSignKnownVector — детерминированный test vector: одна и та же (secret, payload) даёт
// одну и ту же подпись. Это гарантирует, что изменения алгоритма будут пойманы.
func TestSignKnownVector(t *testing.T) {
	secret := []byte("test-secret-123")
	payload := "symbol=BTCUSDT&timestamp=1650000000000"
	s := NewSigner(secret)
	got := s.Sign(payload)

	// Независимо считаем HMAC-SHA256 вручную для сверки.
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(payload))
	want := hex.EncodeToString(mac.Sum(nil))
	if got != want {
		t.Errorf("Sign = %s, want %s", got, want)
	}
}

// TestSignQuery — собирает полную query string с timestamp, recvWindow, signature.
func TestSignQuery(t *testing.T) {
	s := NewSigner([]byte("k"))
	out := s.SignQuery("symbol=BTCUSDT", 1650000000000, 5000)
	if !strings.Contains(out, "symbol=BTCUSDT") {
		t.Error("missing original query")
	}
	if !strings.Contains(out, "timestamp=1650000000000") {
		t.Error("missing timestamp")
	}
	if !strings.Contains(out, "recvWindow=5000") {
		t.Error("missing recvWindow")
	}
	if !strings.HasPrefix(out[strings.Index(out, "&signature="):], "&signature=") {
		t.Error("missing signature suffix")
	}
	// signature должен быть hex строкой 64 символа.
	idx := strings.Index(out, "signature=") + len("signature=")
	if len(out[idx:]) != 64 {
		t.Errorf("signature len = %d, want 64", len(out[idx:]))
	}
}

// TestSignQueryEmptyBase — пустой base query: timestamp первый параметр.
func TestSignQueryEmptyBase(t *testing.T) {
	s := NewSigner([]byte("k"))
	out := s.SignQuery("", 1700000000000, 5000)
	if !strings.HasPrefix(out, "timestamp=1700000000000") {
		t.Errorf("empty base should start with timestamp, got %q", out)
	}
}

// TestSignDeterministic — одна и та же пара (secret, payload) → одна подпись.
func TestSignDeterministic(t *testing.T) {
	s := NewSigner([]byte("abc"))
	first := s.Sign("payload")
	second := s.Sign("payload")
	if first != second {
		t.Error("signing must be deterministic for same input")
	}
}

// TestSignDifferentSecrets — разные секреты → разные подписи.
func TestSignDifferentSecrets(t *testing.T) {
	a := NewSigner([]byte("secret-a"))
	b := NewSigner([]byte("secret-b"))
	payload := "test"
	if a.Sign(payload) == b.Sign(payload) {
		t.Error("different secrets must produce different signatures")
	}
}

// TestVerifyConstantTime — проверка round-trip и constant-time сравнения.
func TestVerifyConstantTime(t *testing.T) {
	secret := []byte("s")
	payload := "test"
	sig := NewSigner(secret).Sign(payload)
	if !VerifyConstantTime(secret, payload, sig) {
		t.Error("verify should match for correct signature")
	}
	if VerifyConstantTime(secret, "other", sig) {
		t.Error("verify should fail for wrong payload")
	}
	if VerifyConstantTime([]byte("other-secret"), payload, sig) {
		t.Error("verify should fail for wrong secret")
	}
	if VerifyConstantTime(secret, payload, "not-hex!") {
		t.Error("verify should fail for invalid hex")
	}
}

// TestZero — после Zero подпись меняется (secret обнулён).
func TestZero(t *testing.T) {
	s := NewSigner([]byte("secret"))
	before := s.Sign("payload")
	s.Zero()
	after := s.Sign("payload")
	if before == after {
		t.Error("signature must differ after Zero()")
	}
}

// TestNowMs — миллисекунды положительные и растут.
func TestNowMs(t *testing.T) {
	t1 := NowMs()
	t2 := NowMs()
	if t1 <= 0 || t2 < t1 {
		t.Errorf("NowMs not monotonic positive: %d, %d", t1, t2)
	}
}
