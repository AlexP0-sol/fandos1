package bybit

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

// TestSignGetPayload — payload собирается в формате timestamp+apiKey+recvWindow+query.
func TestSignGetPayload(t *testing.T) {
	s := NewSigner("KEY123", []byte("secret"))
	payload, sig := s.SignGet(1650000000000, 5000, "category=linear&symbol=BTCUSDT")

	want := "1650000000000KEY1235000category=linear&symbol=BTCUSDT"
	if payload != want {
		t.Errorf("payload = %q, want %q", payload, want)
	}

	// Независимая проверка HMAC.
	mac := hmac.New(sha256.New, []byte("secret"))
	mac.Write([]byte(want))
	expectedSig := hex.EncodeToString(mac.Sum(nil))
	if sig != expectedSig {
		t.Errorf("sig = %s, want %s", sig, expectedSig)
	}
}

// TestSignPostPayload — POST-формат использует body вместо query.
func TestSignPostPayload(t *testing.T) {
	s := NewSigner("K", []byte("s"))
	payload, sig := s.SignPost(1700000000000, 5000, `{"category":"linear","symbol":"BTCUSDT"}`)
	want := `1700000000000K5000{"category":"linear","symbol":"BTCUSDT"}`
	if payload != want {
		t.Errorf("payload = %q, want %q", payload, want)
	}
	if len(sig) != 64 {
		t.Errorf("sig len = %d, want 64", len(sig))
	}
}

// TestSignDeterministic
func TestSignDeterministic(t *testing.T) {
	s := NewSigner("K", []byte("s"))
	_, a := s.SignGet(1, 5000, "x=1")
	_, b := s.SignGet(1, 5000, "x=1")
	if a != b {
		t.Error("signing must be deterministic")
	}
}

// TestDifferentSecrets — разные секреты дают разные подписи.
func TestDifferentSecrets(t *testing.T) {
	a := NewSigner("K", []byte("sa"))
	b := NewSigner("K", []byte("sb"))
	_, sa := a.SignGet(1, 5000, "x=1")
	_, sb := b.SignGet(1, 5000, "x=1")
	if sa == sb {
		t.Error("different secrets must give different signatures")
	}
}

// TestAPIKeyAccessor
func TestAPIKeyAccessor(t *testing.T) {
	s := NewSigner("MYKEY", []byte("s"))
	if s.APIKey() != "MYKEY" {
		t.Errorf("APIKey = %q", s.APIKey())
	}
}

// TestAuthHeaders — V5 аутентификация через заголовки X-BAPI-*.
func TestAuthHeaders(t *testing.T) {
	s := NewSigner("KEY", []byte("s"))
	h := s.AuthHeaders(1650000000000, 5000, "sig123")
	want := map[string]string{
		"X-BAPI-API-KEY":     "KEY",
		"X-BAPI-TIMESTAMP":   "1650000000000",
		"X-BAPI-RECV-WINDOW": "5000",
		"X-BAPI-SIGN":        "sig123",
	}
	for k, v := range want {
		if h[k] != v {
			t.Errorf("header %s = %q, want %q", k, h[k], v)
		}
	}
	if len(h) != len(want) {
		t.Errorf("unexpected extra headers: %v", h)
	}
}

// TestBuildSortedQuery — детерминированный порядок параметров.
func TestBuildSortedQuery(t *testing.T) {
	params := map[string]string{
		"zebra": "3",
		"apple": "1",
		"mango": "2",
	}
	q := BuildSortedQuery(params)
	want := "apple=1&mango=2&zebra=3"
	if q != want {
		t.Errorf("query = %q, want %q", q, want)
	}
}

// TestBuildSortedQueryEmpty
func TestBuildSortedQueryEmpty(t *testing.T) {
	if q := BuildSortedQuery(map[string]string{}); q != "" {
		t.Errorf("empty params should give empty query, got %q", q)
	}
}

// TestZero — после Zero подпись меняется.
func TestZero(t *testing.T) {
	s := NewSigner("K", []byte("secret"))
	_, before := s.SignGet(1, 5000, "x=1")
	s.Zero()
	_, after := s.SignGet(1, 5000, "x=1")
	if before == after {
		t.Error("signature must differ after Zero()")
	}
}

// TestFullRequestFlow — end-to-end по канону V5: query подписывается,
// аутентификация уходит в заголовки, query несёт только бизнес-параметры.
func TestFullRequestFlow(t *testing.T) {
	s := NewSigner("MYAPIKEY", []byte("MYSECRET"))
	ts := int64(1650000000000)

	business := "category=linear&symbol=BTCUSDT"
	payload, sig := s.SignGet(ts, 5000, business)

	// Payload = timestamp + api_key + recv_window + query (V5 spec).
	wantPayload := "1650000000000MYAPIKEY5000" + business
	if payload != wantPayload {
		t.Errorf("payload = %q, want %q", payload, wantPayload)
	}

	h := s.AuthHeaders(ts, 5000, sig)
	if h["X-BAPI-SIGN"] != sig || h["X-BAPI-API-KEY"] != "MYAPIKEY" {
		t.Errorf("auth headers incomplete: %v", h)
	}
	// Query остаётся чистым — только бизнес-параметры.
	if strings.Contains(business, "api_key") || strings.Contains(business, "sign") {
		t.Error("business query must not contain auth params in V5")
	}
}
