// livewiring.go — фабрика реальных биржевых адаптеров для live-режима (раздел 8, 13).
// Читает и расшифровывает trade-ключи из БД, строит per-биржа HTTP-клиент
// (с rate-limit и retry-safe политикой httpclient) и конкретный адаптер.
//
// Каждый пакет-адаптер объявляет собственные структурно-идентичные типы
// HTTPRequest/HTTPDoer — поэтому здесь по одному тонкому мосту на биржу,
// транслирующему запрос в общий httpclient. Бизнес-логики тут нет.
package app

import (
	"context"
	"fmt"
	"time"

	"github.com/thecd/fundarbitrage/internal/domain"
	"github.com/thecd/fundarbitrage/internal/exchange"
	"github.com/thecd/fundarbitrage/internal/exchange/binance"
	"github.com/thecd/fundarbitrage/internal/exchange/bitget"
	"github.com/thecd/fundarbitrage/internal/exchange/bybit"
	"github.com/thecd/fundarbitrage/internal/exchange/gate"
	"github.com/thecd/fundarbitrage/internal/exchange/httpclient"
	"github.com/thecd/fundarbitrage/internal/exchange/kucoin"
	"github.com/thecd/fundarbitrage/internal/exchange/mexc"
	"github.com/thecd/fundarbitrage/internal/exchange/okx"
)

// ownerUserID — single-tenant v1 (ADR-0001): владелец = user_id 1.
const ownerUserID = 1

// liveRateLimit — консервативные общие лимиты (per-биржа тонкая настройка — backlog).
var liveRateLimit = httpclient.RateLimitConfig{RequestsPerSecond: 8, Burst: 16}

// newHTTP создаёт httpclient для конкретного base URL.
func newHTTP(baseURL string) (*httpclient.HttpClient, error) {
	return httpclient.New(httpclient.Config{
		BaseURL:    baseURL,
		Timeout:    10 * time.Second,
		MaxRetries: 3,
		RateLimit:  liveRateLimit,
	})
}

// BuildLiveAdapters строит адаптеры для всех бирж, по которым в БД есть активный
// trade-ключ. Биржи без ключей пропускаются (арбитраж возможен между любыми
// двумя из настроенных). Пустой результат — ошибка: торговать нечем.
func BuildLiveAdapters(ctx context.Context, boot *Bootstrap) (map[domain.ExchangeID]exchange.ExchangeAdapter, error) {
	out := make(map[domain.ExchangeID]exchange.ExchangeAdapter)
	var built []string
	for _, ex := range domain.SupportedExchanges() {
		creds, err := LoadDecrypted(ctx, boot, ownerUserID, ex, "trade")
		if err != nil {
			boot.Log.Info("live: trade-ключ не настроен, биржа пропущена", "exchange", string(ex))
			continue
		}
		adapter, berr := buildAdapter(ex, creds)
		if berr != nil {
			boot.Log.Warn("live: не удалось построить адаптер", "exchange", string(ex), "err", berr.Error())
			continue
		}
		out[ex] = adapter
		built = append(built, string(ex))
	}
	if len(out) < 2 {
		return nil, fmt.Errorf("live: нужно ≥2 бирж с trade-ключами для арбитража, настроено %d (%v)", len(out), built)
	}
	boot.Log.Info("live adapters built", "exchanges", built)
	return out, nil
}

// buildAdapter — конструктор адаптера конкретной биржи из расшифрованных ключей.
func buildAdapter(ex domain.ExchangeID, c CredentialPlaintext) (exchange.ExchangeAdapter, error) {
	switch ex {
	case domain.ExchangeBinance:
		h, err := newHTTP("https://fapi.binance.com")
		if err != nil {
			return nil, err
		}
		sapi, err := newHTTP("https://api.binance.com")
		if err != nil {
			return nil, err
		}
		return binance.New(binance.Config{
			RESTBaseURL: "https://fapi.binance.com",
			SAPIBaseURL: "https://api.binance.com",
			APIKey:      c.Key,
			Signer:      binance.NewSigner([]byte(c.Secret)),
			HTTPDoer:    binanceBridge{fapi: h, sapi: sapi},
		}), nil

	case domain.ExchangeBybit:
		h, err := newHTTP("https://api.bybit.com")
		if err != nil {
			return nil, err
		}
		return bybit.New(bybit.Config{
			RESTBaseURL: "https://api.bybit.com",
			Signer:      bybit.NewSigner(c.Key, []byte(c.Secret)),
			HTTPDoer:    bybitBridge{h: h},
		}), nil

	case domain.ExchangeOKX:
		h, err := newHTTP("https://www.okx.com")
		if err != nil {
			return nil, err
		}
		return okx.New(okx.Config{
			RESTBaseURL: "https://www.okx.com",
			APIKey:      c.Key, APISecret: c.Secret, Passphrase: c.Passphrase,
			HTTPDoer: okxBridge{h: h},
		})

	case domain.ExchangeBitget:
		h, err := newHTTP("https://api.bitget.com")
		if err != nil {
			return nil, err
		}
		return bitget.New(bitget.Config{
			RESTBaseURL: "https://api.bitget.com",
			APIKey:      c.Key, APISecret: c.Secret, Passphrase: c.Passphrase,
			HTTPDoer: bitgetBridge{h: h},
		})

	case domain.ExchangeKuCoin:
		h, err := newHTTP("https://api-futures.kucoin.com")
		if err != nil {
			return nil, err
		}
		spot, err := newHTTP("https://api.kucoin.com")
		if err != nil {
			return nil, err
		}
		return kucoin.New(kucoin.Config{
			RESTBaseURL: "https://api-futures.kucoin.com",
			SpotBaseURL: "https://api.kucoin.com",
			APIKey:      c.Key, APISecret: c.Secret, Passphrase: c.Passphrase,
			HTTPDoer: kucoinBridge{futures: h, spot: spot},
		})

	case domain.ExchangeMEXC:
		h, err := newHTTP("https://contract.mexc.com")
		if err != nil {
			return nil, err
		}
		spot, err := newHTTP("https://api.mexc.com")
		if err != nil {
			return nil, err
		}
		return mexc.New(mexc.Config{
			RESTBaseURL: "https://contract.mexc.com",
			SpotBaseURL: "https://api.mexc.com",
			APIKey:      c.Key, APISecret: c.Secret,
			HTTPDoer: mexcBridge{contract: h, spot: spot},
		})

	case domain.ExchangeGate:
		h, err := newHTTP("https://api.gateio.ws")
		if err != nil {
			return nil, err
		}
		return gate.New(gate.Config{
			RESTBaseURL: "https://api.gateio.ws",
			APIKey:      c.Key, APISecret: c.Secret,
			HTTPDoer: gateBridge{h: h},
		})
	}
	return nil, fmt.Errorf("app: no adapter constructor for exchange %s", ex)
}

// ============================================================
// Мосты HTTPDoer → httpclient (по одному на пакет; типы номинально различны).
// ============================================================

// sapiMarker — префикс пути SAPI у binance-адаптера (второй host).
const sapiMarker = "|SAPI|"

type binanceBridge struct{ fapi, sapi *httpclient.HttpClient }

func (b binanceBridge) Do(ctx context.Context, r binance.HTTPRequest) (int, []byte, error) {
	client, path := b.fapi, r.Path
	if len(r.Path) >= len(sapiMarker) && r.Path[:len(sapiMarker)] == sapiMarker {
		client, path = b.sapi, r.Path[len(sapiMarker):]
	}
	return client.Do(ctx, httpclient.Request{
		Method: r.Method, Path: path, Query: r.Query,
		Body: r.Body, Headers: r.Headers, Safe: r.Safe,
	})
}

type bybitBridge struct{ h *httpclient.HttpClient }

func (b bybitBridge) Do(ctx context.Context, r bybit.HTTPRequest) (int, []byte, error) {
	return b.h.Do(ctx, httpclient.Request{
		Method: r.Method, Path: r.Path, Query: r.Query,
		Body: r.Body, Headers: r.Headers, Safe: r.Safe,
	})
}

type okxBridge struct{ h *httpclient.HttpClient }

func (b okxBridge) Do(ctx context.Context, r okx.HTTPRequest) (int, []byte, error) {
	return b.h.Do(ctx, httpclient.Request{
		Method: r.Method, Path: r.Path, Query: r.Query,
		Body: r.Body, Headers: r.Headers, Safe: r.Safe,
	})
}

type bitgetBridge struct{ h *httpclient.HttpClient }

func (b bitgetBridge) Do(ctx context.Context, r bitget.HTTPRequest) (int, []byte, error) {
	return b.h.Do(ctx, httpclient.Request{
		Method: r.Method, Path: r.Path, Query: r.Query,
		Body: r.Body, Headers: r.Headers, Safe: r.Safe,
	})
}

// kucoinBridge маршрутизирует spot-пути (withdraw/deposit) на api.kucoin.com.
type kucoinBridge struct{ futures, spot *httpclient.HttpClient }

func (b kucoinBridge) Do(ctx context.Context, r kucoin.HTTPRequest) (int, []byte, error) {
	client, path := b.futures, r.Path
	if len(r.Path) >= len(sapiMarker) && r.Path[:len(sapiMarker)] == sapiMarker {
		client, path = b.spot, r.Path[len(sapiMarker):]
	}
	return client.Do(ctx, httpclient.Request{
		Method: r.Method, Path: path, Query: r.Query,
		Body: r.Body, Headers: r.Headers, Safe: r.Safe,
	})
}

// mexcBridge маршрутизирует spot-пути на api.mexc.com.
type mexcBridge struct{ contract, spot *httpclient.HttpClient }

func (b mexcBridge) Do(ctx context.Context, r mexc.HTTPRequest) (int, []byte, error) {
	client, path := b.contract, r.Path
	if len(r.Path) >= len(sapiMarker) && r.Path[:len(sapiMarker)] == sapiMarker {
		client, path = b.spot, r.Path[len(sapiMarker):]
	}
	return client.Do(ctx, httpclient.Request{
		Method: r.Method, Path: path, Query: r.Query,
		Body: r.Body, Headers: r.Headers, Safe: r.Safe,
	})
}

type gateBridge struct{ h *httpclient.HttpClient }

func (b gateBridge) Do(ctx context.Context, r gate.HTTPRequest) (int, []byte, error) {
	return b.h.Do(ctx, httpclient.Request{
		Method: r.Method, Path: r.Path, Query: r.Query,
		Body: r.Body, Headers: r.Headers, Safe: r.Safe,
	})
}
