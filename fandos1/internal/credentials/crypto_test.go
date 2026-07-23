package credentials

import (
	"bytes"
	"crypto/rand"
	"os"
	"testing"
)

func key(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		t.Fatal(err)
	}
	return k
}

func TestRoundTrip(t *testing.T) {
	mk := key(t)
	aad := []byte("tenant:default|exchange:okx|kind:trade")
	secret := []byte("super-secret-api-key-passphrase")
	b, err := Encrypt(mk, secret, aad)
	if err != nil {
		t.Fatal(err)
	}
	pt, err := Decrypt(mk, b, aad)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(pt, secret) {
		t.Fatal("round-trip mismatch")
	}
}

func TestWrongAAD(t *testing.T) {
	mk := key(t)
	b, _ := Encrypt(mk, []byte("s"), []byte("exchange:binance|kind:trade"))
	if _, err := Decrypt(mk, b, []byte("exchange:bybit|kind:trade")); err == nil {
		t.Fatal("ожидалась ошибка: blob привязан к другому контексту")
	}
}

func TestWrongMasterKey(t *testing.T) {
	b, _ := Encrypt(key(t), []byte("s"), []byte("a"))
	if _, err := Decrypt(key(t), b, []byte("a")); err == nil {
		t.Fatal("ожидалась ошибка при чужом master key")
	}
}

func TestTamper(t *testing.T) {
	mk := key(t)
	b, _ := Encrypt(mk, []byte("s"), []byte("a"))
	b.Ciphertext[len(b.Ciphertext)-1] ^= 0xFF
	if _, err := Decrypt(mk, b, []byte("a")); err == nil {
		t.Fatal("AEAD обязан детектировать подмену")
	}
}

func TestUniqueCiphertexts(t *testing.T) {
	mk := key(t)
	b1, _ := Encrypt(mk, []byte("s"), []byte("a"))
	b2, _ := Encrypt(mk, []byte("s"), []byte("a"))
	if bytes.Equal(b1.Ciphertext, b2.Ciphertext) {
		t.Fatal("одинаковый plaintext не должен давать одинаковый ciphertext (random DEK/nonce)")
	}
}

func TestLoadMasterKeyInvalid(t *testing.T) {
	// Не base64
	t.Setenv("BAD_KEY", "!!!notbase64!!!")
	if _, err := LoadMasterKey("BAD_KEY"); err == nil {
		t.Fatal("expected error for non-base64 key")
	}
	// Короткий ключ
	t.Setenv("SHORT_KEY", "AAAA")
	if _, err := LoadMasterKey("SHORT_KEY"); err == nil {
		t.Fatal("expected error for short key")
	}
}

func TestLoadMasterKeyMissing(t *testing.T) {
	const name = "MASTER_KEY_DEFINITELY_MISSING_X9Q"
	os.Unsetenv(name)
	if _, err := LoadMasterKey(name); err == nil {
		t.Fatal("expected error for missing env")
	}
}
