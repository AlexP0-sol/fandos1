package bitget

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"testing"
)

// TestSignGETKnownVector проверяет подпись GET-запроса по известному вектору.
//
// VERIFIED: Bitget V2 подпись — base64(HMAC-SHA256(secret, timestamp+METHOD+path+"?"+query))
//
// Вектор из официальной документации Bitget (classic/quickStart/intro):
//
//	timestamp = 1684814440729
//	method    = GET
//	path      = /api/v2/mix/account/account
//	query     = marginCoin=usdt&symbol=btcusdt
//	preHash   = 1684814440729GET/api/v2/mix/account/account?marginCoin=usdt&symbol=btcusdt
//
// Для пустого secretKey любой deterministicHMAC с нашим вычислением должен совпасть.
// Проверяем детерминированность и строй preHash.
func TestSignGETKnownVector(t *testing.T) {
	secret := []byte("TestSecret123")
	signer := NewSigner("MYKEY", secret, "MyPass")

	ts := int64(1684814440729)
	path := "/api/v2/mix/account/account"
	query := "marginCoin=usdt&symbol=btcusdt"

	preHash, sig := signer.SignGET(ts, path, query)

	// Ожидаемый preHash
	wantPreHash := "1684814440729GET/api/v2/mix/account/account?marginCoin=usdt&symbol=btcusdt"
	if preHash != wantPreHash {
		t.Errorf("preHash = %q, want %q", preHash, wantPreHash)
	}

	// Независимая проверка HMAC
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(wantPreHash))
	expectedSig := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	if sig != expectedSig {
		t.Errorf("sig = %q, want %q", sig, expectedSig)
	}
}

// TestSignPOSTKnownVector проверяет POST-подпись по известному вектору.
//
// VERIFIED: Bitget V2 POST:
//
//	preHash = timestamp + POST + requestPath + body
//	(без "?" для POST, т.к. body передаётся отдельно)
//
// Вектор из официальной документации:
//
//	timestamp = 16273667805456
//	method    = POST
//	path      = /api/v2/mix/order/place-order
//	body      = {"productType":"usdt-futures","symbol":"BTCUSDT",...}
func TestSignPOSTKnownVector(t *testing.T) {
	secret := []byte("TestSecret456")
	signer := NewSigner("KEY", secret, "pass")

	ts := int64(16273667805456)
	path := "/api/v2/mix/order/place-order"
	body := `{"productType":"usdt-futures","symbol":"BTCUSDT","size":"8","marginMode":"crossed","side":"buy","orderType":"limit","clientOid":"channel#123456"}`

	preHash, sig := signer.SignPOST(ts, path, body)

	wantPreHash := "16273667805456POST/api/v2/mix/order/place-order" + body
	if preHash != wantPreHash {
		t.Errorf("preHash = %q, want %q", preHash, wantPreHash)
	}

	// Независимая проверка
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(wantPreHash))
	expectedSig := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	if sig != expectedSig {
		t.Errorf("sig = %q, want %q", sig, expectedSig)
	}
}

// TestSignDeterministic — одинаковые входы → одинаковая подпись.
func TestSignDeterministic(t *testing.T) {
	s := NewSigner("K", []byte("secret"), "pass")
	_, a := s.SignGET(1, "/path", "x=1")
	_, b := s.SignGET(1, "/path", "x=1")
	if a != b {
		t.Error("signing must be deterministic")
	}
}

// TestSignDifferentSecrets — разные секреты дают разные подписи.
func TestSignDifferentSecrets(t *testing.T) {
	a := NewSigner("K", []byte("secretA"), "pass")
	b := NewSigner("K", []byte("secretB"), "pass")
	_, sa := a.SignGET(1, "/path", "x=1")
	_, sb := b.SignGET(1, "/path", "x=1")
	if sa == sb {
		t.Error("different secrets must give different signatures")
	}
}

// TestSignGETNoQuery — если query пустой, "?" не добавляется.
func TestSignGETNoQuery(t *testing.T) {
	s := NewSigner("K", []byte("sec"), "pass")
	preHash, _ := s.SignGET(1000, "/api/v2/public/time", "")
	want := "1000GET/api/v2/public/time"
	if preHash != want {
		t.Errorf("preHash without query = %q, want %q", preHash, want)
	}
}

// TestAuthHeaders — проверяем присутствие всех обязательных заголовков.
func TestAuthHeaders(t *testing.T) {
	s := NewSigner("APIKEY123", []byte("sec"), "MyPassphrase")
	h := s.AuthHeaders(1700000000000, "sig123")

	required := map[string]string{
		"ACCESS-KEY":        "APIKEY123",
		"ACCESS-SIGN":       "sig123",
		"ACCESS-TIMESTAMP":  "1700000000000",
		"ACCESS-PASSPHRASE": "MyPassphrase",
		"Content-Type":      "application/json",
		"locale":            "en-US",
	}
	for k, want := range required {
		if h[k] != want {
			t.Errorf("header %s = %q, want %q", k, h[k], want)
		}
	}
	if len(h) != len(required) {
		t.Errorf("unexpected extra headers: got %d, want %d", len(h), len(required))
	}
}

// TestAPIKeyAndPassphrase — accessor-ы возвращают корректные значения.
func TestAPIKeyAndPassphrase(t *testing.T) {
	s := NewSigner("MYAPIKEY", []byte("sec"), "MYPASS")
	if s.APIKey() != "MYAPIKEY" {
		t.Errorf("APIKey() = %q", s.APIKey())
	}
	if s.Passphrase() != "MYPASS" {
		t.Errorf("Passphrase() = %q", s.Passphrase())
	}
}

// TestZero — после Zero подпись меняется.
func TestZero(t *testing.T) {
	s := NewSigner("K", []byte("secret"), "pass")
	_, before := s.SignGET(1, "/x", "")
	s.Zero()
	_, after := s.SignGET(1, "/x", "")
	if before == after {
		t.Error("signature must differ after Zero()")
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

// TestBuildSortedQueryEmpty — пустые параметры → пустая строка.
func TestBuildSortedQueryEmpty(t *testing.T) {
	if q := BuildSortedQuery(map[string]string{}); q != "" {
		t.Errorf("empty params should give empty query, got %q", q)
	}
}

// TestSignatureIsBase64 — убеждаемся, что подпись base64, а не hex.
func TestSignatureIsBase64(t *testing.T) {
	s := NewSigner("K", []byte("secret"), "pass")
	_, sig := s.SignGET(1000, "/path", "")
	// base64 стандартный — символы A-Z, a-z, 0-9, +, /, = (padding).
	// Длина для SHA256 (32 bytes) = ceil(32/3)*4 = 44 символа.
	if len(sig) != 44 {
		t.Errorf("signature length = %d, want 44 (base64 of SHA256)", len(sig))
	}
}
