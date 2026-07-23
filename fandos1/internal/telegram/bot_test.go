package telegram

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// makeFakeTelegramServer создаёт fake Telegram сервер для тестов.
// processedUpdates — счётчик обработанных апдейтов.
// cancel — отмена контекста бота после одного batch апдейтов.
func makeFakeTelegramServer(t *testing.T, updates []Update) (*httptest.Server, func()) {
	t.Helper()

	var served atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Обрабатываем только getUpdates и sendMessage.
		switch {
		case r.URL.Path == "/botTEST_TOKEN/getUpdates":
			call := served.Add(1)
			w.Header().Set("Content-Type", "application/json")
			if call == 1 {
				// Первый запрос — возвращаем один апдейт.
				data, _ := json.Marshal(map[string]interface{}{
					"ok":     true,
					"result": updates,
				})
				w.Write(data)
			} else {
				// Последующие запросы — пустой результат (polling завершён).
				data, _ := json.Marshal(map[string]interface{}{
					"ok":     true,
					"result": []Update{},
				})
				w.Write(data)
			}

		case r.URL.Path == "/botTEST_TOKEN/sendMessage":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"ok":     true,
				"result": map[string]interface{}{"message_id": 1},
			})

		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))

	return srv, srv.Close
}

// TestBot_PollUpdates_OneUpdateThenStop — long-polling обрабатывает один апдейт затем останавливается по ctx.
func TestBot_PollUpdates_OneUpdateThenStop(t *testing.T) {
	// Ожидаемый апдейт.
	want := Update{
		UpdateID: 100,
		Message: Message{
			MessageID: 1,
			Text:      "hello",
			From:      User{ID: 279058397, Username: "testuser"},
			Chat:      Chat{ID: 279058397},
		},
	}

	srv, cleanup := makeFakeTelegramServer(t, []Update{want})
	defer cleanup()

	bot := NewBot(Config{
		Token:      "TEST_TOKEN",
		BaseURL:    srv.URL,
		HTTPClient: &http.Client{Timeout: 10 * time.Second},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	var received []Update
	done := make(chan struct{})

	go func() {
		defer close(done)
		_ = bot.PollUpdates(ctx, 1, func(_ context.Context, u Update) {
			received = append(received, u)
			// После первого апдейта — останавливаем polling.
			cancel()
		})
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("polling не завершился в течение 5 секунд")
	}

	if len(received) != 1 {
		t.Fatalf("получено %d апдейтов, хотим 1", len(received))
	}
	if received[0].UpdateID != want.UpdateID {
		t.Fatalf("update_id = %d, хотим %d", received[0].UpdateID, want.UpdateID)
	}
	if received[0].Message.Text != want.Message.Text {
		t.Fatalf("text = %q, хотим %q", received[0].Message.Text, want.Message.Text)
	}

	// Проверяем что offset обновился до next = 101.
	if got := bot.offset.Load(); got != 101 {
		t.Fatalf("offset = %d, хотим 101", got)
	}
}

// TestBot_SendMessage — отправка сообщения через fake Telegram API.
func TestBot_SendMessage(t *testing.T) {
	srv, cleanup := makeFakeTelegramServer(t, nil)
	defer cleanup()

	bot := NewBot(Config{
		Token:      "TEST_TOKEN",
		BaseURL:    srv.URL,
		HTTPClient: &http.Client{Timeout: 5 * time.Second},
	})

	err := bot.SendMessage(context.Background(), 12345, "Test *message*", SendMessageOpts{
		ParseMode: "MarkdownV2",
	})
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
}

// TestEscapeMarkdownV2 — все специальные символы экранируются.
func TestEscapeMarkdownV2(t *testing.T) {
	cases := map[string]string{
		"hello":         "hello",
		"1+2=3":         "1\\+2\\=3",
		"price: $100.5": "price: $100\\.5",
		"_bold_ *text*": "\\_bold\\_ \\*text\\*",
		"[link](url)":   "\\[link\\]\\(url\\)",
		"100%":          "100%",
		"a|b":           "a\\|b",
	}
	for input, want := range cases {
		got := EscapeMarkdownV2(input)
		if got != want {
			t.Errorf("EscapeMarkdownV2(%q) = %q, хотим %q", input, got, want)
		}
	}
}
