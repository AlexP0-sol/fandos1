package kucoin

import (
	"testing"
)

// TestSignerKnownVector проверяет KC-API-SIGN и KC-API-PASSPHRASE
// на известных векторах, сгенерированных независимо Python-скриптом:
//
//	import hmac, hashlib, base64
//	secret = b'test-secret'
//	passphrase = 'test-passphrase'
//	timestamp = '1700000000000'
//	method_post = 'POST'
//	endpoint = '/api/v1/orders'
//	body = '{"side":"buy","symbol":"XBTUSDTM"}'
//	str_to_sign = timestamp + method_post + endpoint + body
//	sig = base64.b64encode(hmac.new(secret, str_to_sign.encode(), hashlib.sha256).digest())
//	# => "jz4kEWc+fDgNVKbv5tknji4y09ITM6HGoLj+GEVY0F4="
//	pp = base64.b64encode(hmac.new(secret, passphrase.encode(), hashlib.sha256).digest())
//	# => "UbgWiL7WdjQOVBl1OLuMgUbTl9VlKFsjFbLedtCDPrY="
//
// GET vector:
//
//	endpoint_with_query = '/api/v1/ticker?symbol=XBTUSDTM'
//	str_to_sign2 = timestamp + 'GET' + endpoint_with_query
//	sig2 = base64.b64encode(hmac.new(secret, str_to_sign2.encode(), hashlib.sha256).digest())
//	# => "aTO2wJIRfCj9tSzq+7UDFLX1Lb3OiPTt273fokzVBVQ="
func TestSignerKnownVector(t *testing.T) {
	const (
		apiKey     = "test-api-key"
		secret     = "test-secret"
		passphrase = "test-passphrase"
		ts         = int64(1700000000000)
	)

	signer := NewSigner(apiKey, []byte(secret), passphrase)

	// POST vector
	method := "POST"
	endpoint := "/api/v1/orders"
	body := `{"side":"buy","symbol":"XBTUSDTM"}`
	strToSign := StrToSignPOST(ts, method, endpoint, body)
	wantStrToSign := "1700000000000POST/api/v1/orders" + body
	if strToSign != wantStrToSign {
		t.Errorf("POST strToSign:\n  got  %q\n  want %q", strToSign, wantStrToSign)
	}

	gotSign := signer.Sign(strToSign)
	wantSign := "jz4kEWc+fDgNVKbv5tknji4y09ITM6HGoLj+GEVY0F4="
	if gotSign != wantSign {
		t.Errorf("POST KC-API-SIGN:\n  got  %q\n  want %q", gotSign, wantSign)
	}

	// Passphrase vector (HMAC-signed, не plain)
	// signedPassphrase доступна через AuthHeaders
	headers := signer.AuthHeaders(ts, strToSign)
	wantPassphrase := "UbgWiL7WdjQOVBl1OLuMgUbTl9VlKFsjFbLedtCDPrY="
	if headers["KC-API-PASSPHRASE"] != wantPassphrase {
		t.Errorf("KC-API-PASSPHRASE:\n  got  %q\n  want %q", headers["KC-API-PASSPHRASE"], wantPassphrase)
	}
	if headers["KC-API-KEY"] != apiKey {
		t.Errorf("KC-API-KEY = %q, want %q", headers["KC-API-KEY"], apiKey)
	}
	if headers["KC-API-KEY-VERSION"] != "2" {
		t.Errorf("KC-API-KEY-VERSION = %q, want \"2\"", headers["KC-API-KEY-VERSION"])
	}
	if headers["KC-API-SIGN"] != wantSign {
		t.Errorf("KC-API-SIGN (via AuthHeaders):\n  got  %q\n  want %q", headers["KC-API-SIGN"], wantSign)
	}
	if headers["KC-API-TIMESTAMP"] != "1700000000000" {
		t.Errorf("KC-API-TIMESTAMP = %q, want \"1700000000000\"", headers["KC-API-TIMESTAMP"])
	}

	// GET vector
	endpointWithQuery := "/api/v1/ticker?symbol=XBTUSDTM"
	strToSignGET := StrToSignGET(ts, "GET", endpointWithQuery)
	wantStrToSignGET := "1700000000000GET/api/v1/ticker?symbol=XBTUSDTM"
	if strToSignGET != wantStrToSignGET {
		t.Errorf("GET strToSign:\n  got  %q\n  want %q", strToSignGET, wantStrToSignGET)
	}
	gotSignGET := signer.Sign(strToSignGET)
	wantSignGET := "aTO2wJIRfCj9tSzq+7UDFLX1Lb3OiPTt273fokzVBVQ="
	if gotSignGET != wantSignGET {
		t.Errorf("GET KC-API-SIGN:\n  got  %q\n  want %q", gotSignGET, wantSignGET)
	}
}

// TestBuildSortedQuery проверяет детерминированность сортировки параметров.
func TestBuildSortedQuery(t *testing.T) {
	q := BuildSortedQuery(map[string]string{
		"symbol": "XBTUSDTM",
		"limit":  "20",
	})
	want := "limit=20&symbol=XBTUSDTM"
	if q != want {
		t.Errorf("BuildSortedQuery = %q, want %q", q, want)
	}

	empty := BuildSortedQuery(map[string]string{})
	if empty != "" {
		t.Errorf("BuildSortedQuery(empty) = %q, want \"\"", empty)
	}
}
