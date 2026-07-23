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

// TestAuthParamsQuery — обязательные параметры собираются в строку.
func TestAuthParamsQuery(t *testing.T) {
	s := NewSigner("KEY", []byte("s"))
	q := s.AuthParamsQuery(1650000000000, 5000)
	want := "api_key=KEY&timestamp=1650000000000&recv_window=5000"
	if q != want {
		t.Errorf("auth params = %q, want %q", q, want)
	}
}

// TestAppendSignEmptyQuery — пустой query → только sign.
func TestAppendSignEmptyQuery(t *testing.T) {
	got := AppendSign("", "abc")
	if got != "sign=abc" {
		t.Errorf("got %q", got)
	}
}

// TestAppendSignNonEmpty
func TestAppendSignNonEmpty(t *testing.T) {
	got := AppendSign("api_key=K&timestamp=1", "abc")
	if !strings.HasSuffix(got, "&sign=abc") {
		t.Errorf("missing sign suffix: %q", got)
	}
}

// TestBuildSortedQuery — детерминированный порядок параметров.
func TestBuildSortedQuery(t *testing.T) {
	params := map[string]string{
		"zebra":   "3",
		"apple":   "1",
		"mango":   "2",
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

// TestFullRequestFlow — end-to-end: сборка query + подпись.
func TestFullRequestFlow(t *testing.T) {
	s := NewSigner("MYAPIKEY", []byte("MYSECRET"))
	ts := int64(1650000000000)

	// Строим query с обязательными + бизнес-параметрами.
	business := "category=linear&symbol=BTCUSDT"
	auth := s.AuthParamsQuery(ts, 5000)
	fullQuery := business + "&" + auth

	// Подписываем (payload использует business-query без auth, как требует V5).
	_, sig := s.SignGet(ts, 5000, business)
	signed := AppendSign(fullQuery, sig)

	if !strings.Contains(signed, "api_key=MYAPIKEY") {
		t.Error("missing api_key")
	}
	if !strings.Contains(signed, "timestamp=1650000000000") {
		t.Error("missing timestamp")
	}
	if !strings.Contains(signed, "symbol=BTCUSDT") {
		t.Error("missing business param")
	}
	if !strings.HasSuffix(signed, "&sign="+sig) {
		t.Error("signature suffix missing")
	}
}
