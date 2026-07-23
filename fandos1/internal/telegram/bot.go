// Package telegram реализует Telegram Bot API клиент, notifier, session manager
// и HTTP API для Mini App (стадия 9 промпта v2).
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// ============================================================
// Config — конфигурация бота
// ============================================================

// Config — настройки бота. BaseURL переопределяется в тестах для fake-сервера.
type Config struct {
	Token      string
	HTTPClient *http.Client
	BaseURL    string // по умолчанию https://api.telegram.org
	WebAppDir  string // путь к директории с webapp (для GET /)
}

func (c *Config) baseURL() string {
	if c.BaseURL != "" {
		return strings.TrimRight(c.BaseURL, "/")
	}
	return "https://api.telegram.org"
}

func (c *Config) httpClient() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return &http.Client{Timeout: 70 * time.Second}
}

// ============================================================
// Bot — сырой клиент Telegram Bot API (net/http, без внешних зависимостей)
// ============================================================

// Bot — клиент Telegram Bot API.
type Bot struct {
	cfg    Config
	offset atomic.Int64
}

// NewBot создаёт нового бота.
func NewBot(cfg Config) *Bot {
	return &Bot{cfg: cfg}
}

// apiURL возвращает URL метода API.
func (b *Bot) apiURL(method string) string {
	// Токен не логируется (раздел 13.3).
	return fmt.Sprintf("%s/bot%s/%s", b.cfg.baseURL(), b.cfg.Token, method)
}

// call выполняет POST-запрос к Telegram Bot API и десериализует ответ.
func (b *Bot) call(ctx context.Context, method string, body interface{}, out interface{}) error {
	data, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("telegram: marshal %s: %w", method, err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.apiURL(method), bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("telegram: build request %s: %w", method, err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := b.cfg.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("telegram: %s: %w", method, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("telegram: read response %s: %w", method, err)
	}

	// Telegram всегда возвращает {"ok":bool,...}.
	var apiResp struct {
		OK          bool            `json:"ok"`
		Description string          `json:"description"`
		Result      json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(respBody, &apiResp); err != nil {
		return fmt.Errorf("telegram: parse response %s: %w", method, err)
	}
	if !apiResp.OK {
		return fmt.Errorf("telegram: %s API error: %s", method, apiResp.Description)
	}
	if out != nil && apiResp.Result != nil {
		if err := json.Unmarshal(apiResp.Result, out); err != nil {
			return fmt.Errorf("telegram: parse result %s: %w", method, err)
		}
	}
	return nil
}

// ============================================================
// SendMessage — отправка сообщения
// ============================================================

// SendMessageOpts — опциональные параметры SendMessage.
type SendMessageOpts struct {
	ParseMode             string // "MarkdownV2" | "HTML" | ""
	DisableNotification   bool
	DisableWebPagePreview bool
}

// sendMessageRequest — тело запроса sendMessage.
type sendMessageRequest struct {
	ChatID                int64  `json:"chat_id"`
	Text                  string `json:"text"`
	ParseMode             string `json:"parse_mode,omitempty"`
	DisableNotification   bool   `json:"disable_notification,omitempty"`
	DisableWebPagePreview bool   `json:"disable_web_page_preview,omitempty"`
}

// SendMessage отправляет текстовое сообщение. Не включает секреты в логи.
func (b *Bot) SendMessage(ctx context.Context, chatID int64, text string, opts SendMessageOpts) error {
	req := sendMessageRequest{
		ChatID:                chatID,
		Text:                  text,
		ParseMode:             opts.ParseMode,
		DisableNotification:   opts.DisableNotification,
		DisableWebPagePreview: opts.DisableWebPagePreview,
	}
	if err := b.call(ctx, "sendMessage", req, nil); err != nil {
		// Не логируем text — может содержать чувствительные данные.
		return fmt.Errorf("telegram: SendMessage chatID=%d: %w", chatID, err)
	}
	return nil
}

// EscapeMarkdownV2 экранирует специальные символы MarkdownV2.
// Список символов из документации Telegram Bot API.
func EscapeMarkdownV2(s string) string {
	// Символы, требующие экранирования в MarkdownV2.
	const specials = `\_*[]()~` + "`" + `>#+-=|{}.!`
	var b strings.Builder
	b.Grow(len(s) + 16)
	for _, r := range s {
		if strings.ContainsRune(specials, r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// ============================================================
// GetUpdates — long-polling
// ============================================================

// Update — Telegram Update объект (минимальные поля для обработки).
type Update struct {
	UpdateID int64   `json:"update_id"`
	Message  Message `json:"message"`
}

// Message — входящее сообщение (минимальные поля).
type Message struct {
	MessageID int64  `json:"message_id"`
	Text      string `json:"text"`
	From      User   `json:"from"`
	Chat      Chat   `json:"chat"`
}

// User — Telegram пользователь.
type User struct {
	ID        int64  `json:"id"`
	FirstName string `json:"first_name"`
	Username  string `json:"username"`
}

// Chat — Telegram чат.
type Chat struct {
	ID int64 `json:"id"`
}

// getUpdatesRequest — параметры getUpdates.
type getUpdatesRequest struct {
	Offset  int64 `json:"offset,omitempty"`
	Timeout int   `json:"timeout"`
	Limit   int   `json:"limit,omitempty"`
}

// UpdateHandler — callback для обработки обновлений.
type UpdateHandler func(ctx context.Context, u Update)

// PollUpdates запускает long-polling loop getUpdates.
// Блокирует до отмены ctx. timeout — таймаут long-poll в секундах (Telegram рекомендует 50).
func (b *Bot) PollUpdates(ctx context.Context, timeout int, handler UpdateHandler) error {
	slog.Info("telegram: запуск long-polling", "timeout_s", timeout)
	for {
		select {
		case <-ctx.Done():
			slog.Info("telegram: long-polling остановлен по ctx")
			return nil
		default:
		}

		updates, err := b.getUpdates(ctx, timeout)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			slog.Error("telegram: getUpdates ошибка", "err", err)
			// Небольшая пауза при ошибке, чтобы не спамить в Telegram при сбое сети.
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(5 * time.Second):
			}
			continue
		}

		for _, u := range updates {
			// Обновляем offset до следующего update_id.
			next := u.UpdateID + 1
			for {
				old := b.offset.Load()
				if next <= old {
					break
				}
				if b.offset.CompareAndSwap(old, next) {
					break
				}
			}
			handler(ctx, u)
		}
	}
}

// getUpdates выполняет один запрос getUpdates и возвращает список обновлений.
func (b *Bot) getUpdates(ctx context.Context, timeout int) ([]Update, error) {
	req := getUpdatesRequest{
		Offset:  b.offset.Load(),
		Timeout: timeout,
		Limit:   100,
	}

	// Контекст с запасом сверх timeout, чтобы не прерывать HTTP-запрос раньше времени.
	callCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout+20)*time.Second)
	defer cancel()

	var updates []Update
	if err := b.call(callCtx, "getUpdates", req, &updates); err != nil {
		return nil, err
	}
	return updates, nil
}
