# Bybit Linear USDT Perpetual — контракт адаптера

> **ВАЖНО (раздел 2 промпта v2):** сверить с актуальной документацией
> https://bybit-exchange.github.io/docs/v5/intro перед production-интеграцией.

## Дата проверки
Заготовка — 2026-07. Требует верификации.

## Версия API
V5 API (унифицированный linear/inverse/option).

## Base URL
- Production: `https://api.bybit.com`
- Testnet: `https://api-testnet.bybit.com`
- WS Public Production: `wss://stream.bybit.com/v5/public/linear`
- WS Private Production: `wss://stream.bybit.com/v5/private`

## Тип инструмента
LINEAR perpetual, settlement USDT (`category=linear`).

## Формат symbol
`<ASSET>USDT`, например `BTCUSDT`, `ETHUSDT`.

## Подпись REST
- Параметры: `api_key`, `timestamp` (ms), `recv_window` (ms).
- payload = `timestamp + api_key + recv_window + <query_string>`.
- signature = HMAC-SHA256(secret, payload), hex-encoded.
- Передаётся как `sign` query-параметр.

## Timestamp / recv_window
- `timestamp` — миллисекунды UTC.
- `recv_window` — по умолчанию 5000 мс.

## Учётные данные
API Key, API Secret. Passphrase НЕ требуется (в отличие от OKX/Bitget/KuCoin).

## Права ключа
- Унифицированный ключ с уровнями прав: ReadTrade, Trade, Withdraw.
- Для v1: Read + Trade; отдельный ключ с Withdraw для ребалансировки.
- IP whitelist поддерживается в UI Bybit.

## Публичные REST endpoints
- `GET /v5/market/instruments-info?category=linear` — реестр.
- `GET /v5/market/tickers?category=linear&symbol=` — 24h ticker + mark + funding.
- `GET /v5/market/funding/history?category=linear&symbol=` — история funding.
- `GET /v5/market/orderbook?category=linear&symbol=&limit=` — order book.

## Публичные WS streams
- `tickers.<symbol>` — 24h ticker + funding.
- `orderbook.<depth>.<symbol>` — partial book (50, 200).
- `publicTrade.<symbol>` — сделки.

## Приватные REST endpoints
- `GET /v5/account/wallet-balance?accountType=UNIFIED` — баланс.
- `GET /v5/position/list?category=linear&settleCoin=USDT` — позиции.
- `GET /v5/order/realtime?category=linear` — открытые ордера.
- `POST /v5/order/create` — разместить (orderLinkId=clientOrderId).
- `POST /v5/order/cancel` — отменить.
- `GET /v5/order/realtime?orderLinkId=` — query.
- `POST /v5/account/set-leverage` — set leverage.
- `POST /v5/account/set-margin-mode` — ISOLATED/CROSS.

## Приватные WS
- Auth: `{"op":"auth","args":[api_key, expires_ms, signature]}`.
- Topics: `order`, `position`, `execution`, `wallet`, `position`.

## Funding
- Bybit V5 Linear: интервал 8h по умолчанию, но некоторые инструменты 1h/4h.
- `nextFundingTime` в миллисекундах UTC.
- fundingRate × notional, positive → long платит short.

## ADL
- `GET /v5/account/adl-list?category=linear` — индикатор ADL per-symbol.

## Position mode
- One-way / Hedge через `POST /v5/account/position-mode`.

## Rate limits
- V5 использует weight-based лимиты (не count-based).
- Проверять заголовки `X-RateLimit-Status`, `X-RateLimit-Remaining-Btc` и т.п.

## clientOrderId
- Параметр `orderLinkId`: до 36 символов, alphanumeric + `_` `-`.
- **ВАЖНО:** проверить, принимает ли Bybit ':' — если нет, адаптировать Format().

## Особенности
- Account types: UNIFIED (рекомендуется), CONTRACT, SPOT.
- Margin modes: ISOLATED/CROSS per-coin.
- reduceOnly в V5: `reduceOnly=true` в POST /order/create.

## TODO для production
- [ ] Сверить JSON-схемы ответов с V5.
- [ ] Уточнить ':' в orderLinkId (по V5 doc обычно запрещён, использовать '-').
- [ ] Проверить weight-таблицу rate limits.
- [ ] Auth WS через expires + signature.
