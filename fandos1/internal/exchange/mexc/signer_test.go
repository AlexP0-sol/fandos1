package mexc

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

// TestSignGET_KnownVector — проверяет подпись GET-запроса по известному вектору.
//
// Вектор сгенерирован независимо на Python:
//
//	import hmac, hashlib
//	string_to_sign = "TEST_KEY" + "1700000000000" + "page_num=1&symbol=BTC_USDT"
//	sig = hmac.new(b"TEST_SECRET", string_to_sign.encode(), hashlib.sha256).hexdigest()
//	# => "4543b04893162ffc11805de3f04596e7d3f7545ed193b6fca31c840945ff659e"
func TestSignGET_KnownVector(t *testing.T) {
	s := NewSigner("TEST_KEY", []byte("TEST_SECRET"))
	ts, sig := s.SignGET(1700000000000, "page_num=1&symbol=BTC_USDT")

	if ts != "1700000000000" {
		t.Errorf("ts = %q, want 1700000000000", ts)
	}
	want := "4543b04893162ffc11805de3f04596e7d3f7545ed193b6fca31c840945ff659e"
	if sig != want {
		t.Errorf("signature = %s\nwant       %s", sig, want)
	}
}

// TestSignPOST_KnownVector — проверяет подпись POST-запроса по известному вектору.
//
// Вектор сгенерирован независимо на Python:
//
//	body = '{"symbol":"BTC_USDT","vol":1,"side":1,"type":5,"openType":2}'
//	string_to_sign = "TEST_KEY" + "1700000000000" + body
//	sig = hmac.new(b"TEST_SECRET", string_to_sign.encode(), hashlib.sha256).hexdigest()
//	# => "004efa0c1e29c228b3f1a6bd06dc07cf8fca4ad1f13a59868e8710f31e631858"
func TestSignPOST_KnownVector(t *testing.T) {
	s := NewSigner("TEST_KEY", []byte("TEST_SECRET"))
	body := `{"symbol":"BTC_USDT","vol":1,"side":1,"type":5,"openType":2}`
	ts, sig := s.SignPOST(1700000000000, body)

	if ts != "1700000000000" {
		t.Errorf("ts = %q, want 1700000000000", ts)
	}
	want := "004efa0c1e29c228b3f1a6bd06dc07cf8fca4ad1f13a59868e8710f31e631858"
	if sig != want {
		t.Errorf("signature = %s\nwant       %s", sig, want)
	}
}

// TestSignDeterministic — одни и те же входные данные дают одну и ту же подпись.
func TestSignDeterministic(t *testing.T) {
	s := NewSigner("K", []byte("s"))
	_, a := s.SignGET(1700000000000, "x=1")
	_, b := s.SignGET(1700000000000, "x=1")
	if a != b {
		t.Error("signing must be deterministic")
	}
}

// TestSignDifferentSecrets — разные секреты дают разные подписи.
func TestSignDifferentSecrets(t *testing.T) {
	a := NewSigner("K", []byte("sa"))
	b := NewSigner("K", []byte("sb"))
	_, sa := a.SignGET(1, "x=1")
	_, sb := b.SignGET(1, "x=1")
	if sa == sb {
		t.Error("different secrets must give different signatures")
	}
}

// TestAuthHeaders — заголовки аутентификации содержат обязательные поля.
func TestAuthHeaders(t *testing.T) {
	s := NewSigner("MYKEY", []byte("s"))
	h := s.AuthHeaders("1700000000000", "sig123")
	want := map[string]string{
		"ApiKey":       "MYKEY",
		"Request-Time": "1700000000000",
		"Signature":    "sig123",
		"Content-Type": "application/json",
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

// TestBuildSortedQuery — параметры сортируются детерминированно.
func TestBuildSortedQuery(t *testing.T) {
	params := map[string]string{
		"symbol":   "BTC_USDT",
		"page_num": "1",
		"age":      "10",
	}
	q := BuildSortedQuery(params)
	want := "age=10&page_num=1&symbol=BTC_USDT"
	if q != want {
		t.Errorf("query = %q, want %q", q, want)
	}
}

// TestBuildSortedQueryEmpty — пустые параметры дают пустую строку.
func TestBuildSortedQueryEmpty(t *testing.T) {
	if q := BuildSortedQuery(map[string]string{}); q != "" {
		t.Errorf("empty params should give empty query, got %q", q)
	}
}

// TestSignGET_InternalVerify — независимая проверка HMAC через стандартную библиотеку.
func TestSignGET_InternalVerify(t *testing.T) {
	apiKey := "MYAPIKEY"
	secret := []byte("MYSECRET")
	ts := int64(1650000000000)
	paramStr := "category=futures&symbol=BTC_USDT"

	s := NewSigner(apiKey, secret)
	tsStr, sig := s.SignGET(ts, paramStr)

	// Независимая верификация.
	expected := apiKey + tsStr + paramStr
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(expected))
	wantSig := hex.EncodeToString(mac.Sum(nil))

	if sig != wantSig {
		t.Errorf("sig = %s, want %s", sig, wantSig)
	}
}

// TestZero — после Zero подпись меняется.
func TestZero(t *testing.T) {
	s := NewSigner("K", []byte("secret"))
	_, before := s.SignGET(1, "x=1")
	s.Zero()
	_, after := s.SignGET(1, "x=1")
	if before == after {
		t.Error("signature must differ after Zero()")
	}
}

// TestAPIKeyAccessor — APIKey() возвращает ключ.
func TestAPIKeyAccessor(t *testing.T) {
	s := NewSigner("MYKEY", []byte("s"))
	if s.APIKey() != "MYKEY" {
		t.Errorf("APIKey = %q", s.APIKey())
	}
}
