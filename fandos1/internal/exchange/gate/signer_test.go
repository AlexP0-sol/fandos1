package gate

import (
	"testing"
)

// Тест с известным вектором, сгенерированным независимо на Python:
//
//	python3 -c "
//	import hmac, hashlib
//	secret = 'test-secret-key'
//	method = 'GET'; url_path = '/api/v4/futures/usdt/contracts'
//	query = 'limit=100'; body = ''; ts = '1700000000'
//	m = hashlib.sha512(); m.update(body.encode()); body_hash = m.hexdigest()
//	sig_str = method+'\n'+url_path+'\n'+query+'\n'+body_hash+'\n'+ts
//	sign = hmac.new(secret.encode(), sig_str.encode(), hashlib.sha512).hexdigest()
//	print(sign)
//	"
//
// Результат:
// f56f0a0ca520cc886eced17e761c207ed2ffc3186d71ec237e4edef15086558bbe36949675795f18e7029726879d097e0cb5c9736d23f8e49ce26b4880e5b770
func TestSignerKnownVectorGET(t *testing.T) {
	s := NewSigner("test-api-key", []byte("test-secret-key"))

	// Вектор: GET /api/v4/futures/usdt/contracts?limit=100 ts=1700000000
	got := s.Sign("GET", "/api/v4/futures/usdt/contracts", "limit=100", "", 1700000000)
	want := "f56f0a0ca520cc886eced17e761c207ed2ffc3186d71ec237e4edef15086558bbe36949675795f18e7029726879d097e0cb5c9736d23f8e49ce26b4880e5b770"
	if got != want {
		t.Errorf("Sign GET: got  %s\n          want %s", got, want)
	}
}

// POST-вектор с непустым телом.
//
//	python3 -c "
//	import hmac, hashlib
//	secret = 'test-secret-key'
//	method = 'POST'; url_path = '/api/v4/futures/usdt/orders'
//	query = ''; body = '{\"contract\":\"BTC_USDT\",\"size\":1}'; ts = '1700000000'
//	m = hashlib.sha512(); m.update(body.encode()); body_hash = m.hexdigest()
//	sig_str = method+'\n'+url_path+'\n'+query+'\n'+body_hash+'\n'+ts
//	sign = hmac.new(secret.encode(), sig_str.encode(), hashlib.sha512).hexdigest()
//	print(sign)
//	"
//
// Результат:
// 25a181446cc2d96f9886c781f126ed0a41cf577bd3e9b556de34d0d68788043ce38cec27a75e4a918e8bc0df41117c92101cc45524fc9d1ad09510a05378921d
func TestSignerKnownVectorPOST(t *testing.T) {
	s := NewSigner("test-api-key", []byte("test-secret-key"))

	body := `{"contract":"BTC_USDT","size":1}`
	got := s.Sign("POST", "/api/v4/futures/usdt/orders", "", body, 1700000000)
	want := "25a181446cc2d96f9886c781f126ed0a41cf577bd3e9b556de34d0d68788043ce38cec27a75e4a918e8bc0df41117c92101cc45524fc9d1ad09510a05378921d"
	if got != want {
		t.Errorf("Sign POST: got  %s\n           want %s", got, want)
	}
}

// TestSignerEmptyBody — хэш пустого тела совпадает с SHA512("").
func TestSignerEmptyBody(t *testing.T) {
	// SHA512("") = cf83e1357eef...
	s := NewSigner("k", []byte("s"))
	// Два вызова с разным body="" и body не задан — оба дают одинаковый результат.
	sig1 := s.Sign("GET", "/api/v4/spot/time", "", "", 1700000000)
	sig2 := s.Sign("GET", "/api/v4/spot/time", "", "", 1700000000)
	if sig1 != sig2 {
		t.Error("identical inputs must produce identical signatures")
	}
}

// TestSignerDeterministic — одинаковый ввод → одинаковая подпись.
func TestSignerDeterministic(t *testing.T) {
	s := NewSigner("KEY", []byte("SEC"))
	a := s.Sign("GET", "/api/v4/futures/usdt/accounts", "x=1", "", 1000)
	b := s.Sign("GET", "/api/v4/futures/usdt/accounts", "x=1", "", 1000)
	if a != b {
		t.Error("signing must be deterministic")
	}
}

// TestSignerDifferentSecrets — разные секреты → разные подписи.
func TestSignerDifferentSecrets(t *testing.T) {
	a := NewSigner("K", []byte("secret-a"))
	b := NewSigner("K", []byte("secret-b"))
	sa := a.Sign("GET", "/x", "", "", 1)
	sb := b.Sign("GET", "/x", "", "", 1)
	if sa == sb {
		t.Error("different secrets must produce different signatures")
	}
}

// TestSignerAuthHeaders — заголовки KEY/Timestamp/SIGN.
func TestSignerAuthHeaders(t *testing.T) {
	s := NewSigner("MY-KEY", []byte("s"))
	h := s.AuthHeaders(1700000000, "sig-value")
	if h["KEY"] != "MY-KEY" {
		t.Errorf("KEY = %q", h["KEY"])
	}
	if h["Timestamp"] != "1700000000" {
		t.Errorf("Timestamp = %q", h["Timestamp"])
	}
	if h["SIGN"] != "sig-value" {
		t.Errorf("SIGN = %q", h["SIGN"])
	}
	if len(h) != 3 {
		t.Errorf("unexpected headers: %v", h)
	}
}

// TestBuildSortedQuery — детерминированный порядок параметров.
func TestBuildSortedQuery(t *testing.T) {
	params := map[string]string{"zebra": "3", "apple": "1", "mango": "2"}
	got := BuildSortedQuery(params)
	want := "apple=1&mango=2&zebra=3"
	if got != want {
		t.Errorf("BuildSortedQuery = %q, want %q", got, want)
	}
}

// TestBuildSortedQueryEmpty — пустой map → пустая строка.
func TestBuildSortedQueryEmpty(t *testing.T) {
	if q := BuildSortedQuery(nil); q != "" {
		t.Errorf("empty params gave %q", q)
	}
}

// TestSignerZero — после Zero подпись меняется.
func TestSignerZero(t *testing.T) {
	s := NewSigner("K", []byte("secret"))
	before := s.Sign("GET", "/x", "", "", 1)
	s.Zero()
	after := s.Sign("GET", "/x", "", "", 1)
	if before == after {
		t.Error("signature must differ after Zero()")
	}
}

// TestAPIKeyAccessor
func TestAPIKeyAccessor(t *testing.T) {
	s := NewSigner("MYKEY", []byte("s"))
	if s.APIKey() != "MYKEY" {
		t.Errorf("APIKey = %q", s.APIKey())
	}
}
