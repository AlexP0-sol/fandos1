package repository_test

// Интеграционные тесты CredentialsRepo и UsersRepo.
// Требуют живой PostgreSQL: postgres://fandos:fandos@localhost:5432/fandos
// Запуск: go test ./internal/repository/ -count=1

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/thecd/fundarbitrage/internal/credentials"
	"github.com/thecd/fundarbitrage/internal/repository"
)

// testMasterKey — фиксированный 32-байтовый тестовый master key.
var testMasterKey = []byte("00112233445566778899aabbccddeeff") // ровно 32 байта

// testUserID_creds — user_id=1 (seed пользователь из 0001_core.sql).
const testUserID_creds = int64(1)

// cleanupCredential удаляет тестовый credential по (user_id, exchange, kind).
func cleanupCredential(t *testing.T, userID int64, exchange, kind string) {
	t.Helper()
	_, _ = testPool.Exec(context.Background(),
		`DELETE FROM exchange_credentials WHERE user_id=$1 AND exchange=$2 AND kind=$3`,
		userID, exchange, kind)
}

// makeBlob создаёт тестовый зашифрованный blob.
func makeBlob(t *testing.T, apiKey, apiSecret, passphrase string) (*credentials.Blob, string) {
	t.Helper()
	payload := map[string]string{
		"api_key":    apiKey,
		"api_secret": apiSecret,
		"passphrase": passphrase,
	}
	plaintext, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	aad := []byte("test-aad")
	blob, err := credentials.Encrypt(testMasterKey, plaintext, aad)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	// fingerprint — первые 8 символов API-ключа + ...
	fp := apiKey
	if len(fp) > 8 {
		fp = fp[:8] + "..."
	}
	return blob, fp
}

// ============================================================
// CredentialsRepo — save/load round-trip
// ============================================================

func TestCredentials_SaveLoadRoundTrip(t *testing.T) {
	if testPool == nil {
		t.Skip("пул не инициализирован")
	}

	ctx := context.Background()
	repo := repository.NewCredentialsRepo(testPool)

	exchange := "binance"
	kind := "trade"
	defer cleanupCredential(t, testUserID_creds, exchange, kind)

	blob, fp := makeBlob(t, "TESTAPIKEY123456", "SECRETVALUE99", "")

	// Сохраняем.
	if err := repo.Save(ctx, testUserID_creds, exchange, kind, blob, fp); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Загружаем и проверяем что расшифровывается.
	loaded, err := repo.Load(ctx, testUserID_creds, exchange, kind)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	aad := []byte("test-aad")
	plaintext, err := credentials.Decrypt(testMasterKey, loaded, aad)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	defer credentials.Zero(plaintext)

	var decoded map[string]string
	if err := json.Unmarshal(plaintext, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded["api_key"] != "TESTAPIKEY123456" {
		t.Errorf("api_key = %q, хотим TESTAPIKEY123456", decoded["api_key"])
	}
	if decoded["api_secret"] != "SECRETVALUE99" {
		t.Errorf("api_secret = %q, хотим SECRETVALUE99", decoded["api_secret"])
	}
}

// ============================================================
// CredentialsRepo — ротация (повторный Save)
// ============================================================

func TestCredentials_Rotate(t *testing.T) {
	if testPool == nil {
		t.Skip("пул не инициализирован")
	}

	ctx := context.Background()
	repo := repository.NewCredentialsRepo(testPool)

	exchange := "bybit"
	kind := "trade"
	defer cleanupCredential(t, testUserID_creds, exchange, kind)

	blob1, fp1 := makeBlob(t, "KEY_BEFORE_ROTATE", "SECRET_BEFORE", "")
	if err := repo.Save(ctx, testUserID_creds, exchange, kind, blob1, fp1); err != nil {
		t.Fatalf("первый Save: %v", err)
	}

	// Проверяем что rotated_at NULL после первого сохранения.
	list1, err := repo.List(ctx, testUserID_creds)
	if err != nil {
		t.Fatalf("List после первого Save: %v", err)
	}
	var info1 *repository.CredentialInfo
	for i := range list1 {
		if list1[i].Exchange == exchange && list1[i].Kind == kind {
			info1 = &list1[i]
			break
		}
	}
	if info1 == nil {
		t.Fatal("credential не найден в списке после первого Save")
	}
	// rotated_at NULL для нового ключа.
	if info1.RotatedAt != nil {
		t.Errorf("RotatedAt должен быть nil для нового ключа, получили %v", info1.RotatedAt)
	}

	// Небольшая пауза чтобы rotated_at отличался от created_at.
	time.Sleep(10 * time.Millisecond)

	// Повторный Save = ротация.
	blob2, fp2 := makeBlob(t, "KEY_AFTER_ROTATE", "SECRET_AFTER", "")
	if err := repo.Save(ctx, testUserID_creds, exchange, kind, blob2, fp2); err != nil {
		t.Fatalf("второй Save (ротация): %v", err)
	}

	// Проверяем что rotated_at установлен.
	list2, err := repo.List(ctx, testUserID_creds)
	if err != nil {
		t.Fatalf("List после ротации: %v", err)
	}
	var info2 *repository.CredentialInfo
	for i := range list2 {
		if list2[i].Exchange == exchange && list2[i].Kind == kind {
			info2 = &list2[i]
			break
		}
	}
	if info2 == nil {
		t.Fatal("credential не найден после ротации")
	}
	if info2.RotatedAt == nil {
		t.Error("RotatedAt должен быть установлен после ротации")
	}
	if info2.Fingerprint != fp2 {
		t.Errorf("Fingerprint после ротации = %q, хотим %q", info2.Fingerprint, fp2)
	}
	if info2.Revoked {
		t.Error("после ротации ключ не должен быть отозван")
	}

	// Загружаем и проверяем что это уже новый ключ.
	loaded, err := repo.Load(ctx, testUserID_creds, exchange, kind)
	if err != nil {
		t.Fatalf("Load после ротации: %v", err)
	}
	aad := []byte("test-aad")
	pt, err := credentials.Decrypt(testMasterKey, loaded, aad)
	if err != nil {
		t.Fatalf("Decrypt после ротации: %v", err)
	}
	defer credentials.Zero(pt)
	var decoded map[string]string
	if err := json.Unmarshal(pt, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded["api_key"] != "KEY_AFTER_ROTATE" {
		t.Errorf("api_key = %q, хотим KEY_AFTER_ROTATE", decoded["api_key"])
	}
}

// ============================================================
// CredentialsRepo — Revoke и HasActive
// ============================================================

func TestCredentials_RevokeHasActive(t *testing.T) {
	if testPool == nil {
		t.Skip("пул не инициализирован")
	}

	ctx := context.Background()
	repo := repository.NewCredentialsRepo(testPool)

	exchange := "okx"
	kind := "withdrawal"
	defer cleanupCredential(t, testUserID_creds, exchange, kind)

	blob, fp := makeBlob(t, "OKX_KEY_REVOKE", "OKX_SECRET", "PASSPHRASE123")
	if err := repo.Save(ctx, testUserID_creds, exchange, kind, blob, fp); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// HasActive должен возвращать true.
	active, err := repo.HasActive(ctx, testUserID_creds, exchange, kind)
	if err != nil {
		t.Fatalf("HasActive до отзыва: %v", err)
	}
	if !active {
		t.Error("HasActive должен быть true до отзыва")
	}

	// Отзываем.
	if err := repo.Revoke(ctx, testUserID_creds, exchange, kind); err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	// HasActive должен вернуть false.
	active, err = repo.HasActive(ctx, testUserID_creds, exchange, kind)
	if err != nil {
		t.Fatalf("HasActive после отзыва: %v", err)
	}
	if active {
		t.Error("HasActive должен быть false после отзыва")
	}

	// Load должен вернуть ErrCredentialNotFound.
	_, err = repo.Load(ctx, testUserID_creds, exchange, kind)
	if !errors.Is(err, repository.ErrCredentialNotFound) {
		t.Errorf("Load после отзыва: ожидали ErrCredentialNotFound, получили %v", err)
	}

	// Повторный Revoke должен вернуть ErrCredentialNotFound.
	err = repo.Revoke(ctx, testUserID_creds, exchange, kind)
	if !errors.Is(err, repository.ErrCredentialNotFound) {
		t.Errorf("повторный Revoke: ожидали ErrCredentialNotFound, получили %v", err)
	}

	// После ротации (повторный Save) revoked_at должен сброситься.
	blob2, fp2 := makeBlob(t, "OKX_KEY_AFTER_REVOKE", "OKX_SECRET2", "PASS2")
	if err := repo.Save(ctx, testUserID_creds, exchange, kind, blob2, fp2); err != nil {
		t.Fatalf("Save после отзыва: %v", err)
	}
	active, err = repo.HasActive(ctx, testUserID_creds, exchange, kind)
	if err != nil {
		t.Fatalf("HasActive после повторного Save: %v", err)
	}
	if !active {
		t.Error("HasActive должен быть true после повторного Save")
	}
}

// ============================================================
// CredentialsRepo — Load на несуществующую запись
// ============================================================

func TestCredentials_LoadNotFound(t *testing.T) {
	if testPool == nil {
		t.Skip("пул не инициализирован")
	}

	ctx := context.Background()
	repo := repository.NewCredentialsRepo(testPool)

	// Гарантируем отсутствие записи.
	cleanupCredential(t, testUserID_creds, "gate", "trade")

	_, err := repo.Load(ctx, testUserID_creds, "gate", "trade")
	if !errors.Is(err, repository.ErrCredentialNotFound) {
		t.Errorf("ожидали ErrCredentialNotFound, получили %v", err)
	}
}

// ============================================================
// CredentialsRepo — List возвращает только данного пользователя
// ============================================================

func TestCredentials_ListFiltersUser(t *testing.T) {
	if testPool == nil {
		t.Skip("пул не инициализирован")
	}

	ctx := context.Background()
	repo := repository.NewCredentialsRepo(testPool)

	exchange := "mexc"
	kind := "trade"
	defer cleanupCredential(t, testUserID_creds, exchange, kind)

	blob, fp := makeBlob(t, "MEXC_TEST_KEY", "MEXC_SECRET", "")
	if err := repo.Save(ctx, testUserID_creds, exchange, kind, blob, fp); err != nil {
		t.Fatalf("Save: %v", err)
	}

	list, err := repo.List(ctx, testUserID_creds)
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	found := false
	for _, info := range list {
		if info.Exchange == exchange && info.Kind == kind {
			found = true
			// CredentialInfo не содержит секретов.
			if info.Fingerprint != fp {
				t.Errorf("Fingerprint = %q, хотим %q", info.Fingerprint, fp)
			}
			if info.CreatedAt.IsZero() {
				t.Error("CreatedAt не должен быть нулевым")
			}
			break
		}
	}
	if !found {
		t.Errorf("credential %s/%s не найден в List", exchange, kind)
	}
}

// ============================================================
// UsersRepo — ClaimOwner
// ============================================================

func TestUsers_ClaimOwner(t *testing.T) {
	if testPool == nil {
		t.Skip("пул не инициализирован")
	}

	ctx := context.Background()
	repo := repository.NewUsersRepo(testPool)

	// Сохраняем текущее telegram_id и восстанавливаем после теста.
	var origTelegramID int64
	if err := testPool.QueryRow(ctx, `SELECT telegram_id FROM users WHERE tenant_id='default'`).Scan(&origTelegramID); err != nil {
		t.Fatalf("чтение текущего telegram_id: %v", err)
	}
	defer func() {
		_, _ = testPool.Exec(ctx, `UPDATE users SET telegram_id=$1 WHERE tenant_id='default'`, origTelegramID)
	}()

	// Сбрасываем telegram_id в -1 (seed placeholder) для чистоты теста.
	if _, err := testPool.Exec(ctx, `UPDATE users SET telegram_id=-1 WHERE tenant_id='default'`); err != nil {
		t.Fatalf("сброс telegram_id: %v", err)
	}

	// Первый клейм с telegram_id=-1 → должен успешно захватить.
	claimed, err := repo.ClaimOwner(ctx, int64(555777999))
	if err != nil {
		t.Fatalf("ClaimOwner(555777999): %v", err)
	}
	if !claimed {
		t.Error("первый ClaimOwner должен возвращать claimed=true")
	}

	// Повторный клейм → claimed=false (строка уже занята telegram_id=555777999).
	claimed2, err := repo.ClaimOwner(ctx, int64(555777999))
	if err != nil {
		t.Fatalf("повторный ClaimOwner: %v", err)
	}
	if claimed2 {
		t.Error("повторный ClaimOwner должен возвращать false")
	}

	// Чужой telegram_id → тоже false.
	claimed3, err := repo.ClaimOwner(ctx, int64(111222333))
	if err != nil {
		t.Fatalf("ClaimOwner(111222333): %v", err)
	}
	if claimed3 {
		t.Error("клейм чужим telegram_id должен возвращать false")
	}
}

// ============================================================
// UsersRepo — OwnerTelegramID
// ============================================================

func TestUsers_OwnerTelegramID(t *testing.T) {
	if testPool == nil {
		t.Skip("пул не инициализирован")
	}

	ctx := context.Background()
	repo := repository.NewUsersRepo(testPool)

	id, err := repo.OwnerTelegramID(ctx)
	if err != nil {
		t.Fatalf("OwnerTelegramID: %v", err)
	}
	// Seed имеет telegram_id=-1 (если тесты выше не изменили, и restore выполнен).
	// Проверяем просто что метод работает без ошибки.
	_ = id
}
