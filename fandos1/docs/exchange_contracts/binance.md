# Binance USDT-M Futures — контракт адаптера

> **ВАЖНО (раздел 2 промпта v2):** актуальность документации должна быть проверена
> на день реализации. Этот файл — базовая структура; перед production-rollback
> сверить с https://developers.binance.com/docs/derivatives/usds-margined-futures/general-info.

## Дата проверки
Заготовка — 2026-07. Требует верификации перед интеграцией.

## Версия API
USDT-M Futures (fapi). Публичные: v1/v2. Приватные: v1/v2.

## Base URL
- Production: `https://fapi.binance.com`
- Testnet: `https://testnet.binancefuture.com`
- WS Production: `wss://fstream.binance.com`
- WS Testnet: `wss://stream.binancefuture.com`

## Тип инструмента
LINEAR USDT-M perpetual (`instrumentType = LINEAR_USDT_PERPETUAL`).

## Формат symbol
`<ASSET>USDT`, например `BTCUSDT`, `ETHUSDT`. Concatenation работает для Binance,
но в реестре всё равно хранится явно (раздел 6.1).

## Подпись REST
- HMAC-SHA256 от query string (включая `timestamp` и `recvWindow`).
- Signature передаётся как `signature` query-параметр.
- `X-MBX-APIKEY` header = API key.

## Timestamp / recvWindow
- `timestamp` — миллисекунды UTC.
- `recvWindow` — по умолчанию 5000 мс, max 60000.

## Учётные данные
API Key, API Secret. Passphrase НЕ требуется.

## Права ключа
- Read (account).
- Futures Trading (для order endpoints).
- Withdraw — для withdrawal key (отдельный ключ).
- Binance не поддерживает IP whitelist на отдельные ключи напрямую (через sub-account).

## Публичные REST endpoints
- `GET /fapi/v1/exchangeInfo` — реестр инструментов.
- `GET /fapi/v1/ticker/24hr?symbol=` — 24h ticker (volume).
- `GET /fapi/v1/ticker/bookTicker?symbol=` — best bid/ask.
- `GET /fapi/v1/premiumIndex?symbol=` — mark price, funding rate, next funding time.
- `GET /fapi/v1/klines` — свечи (не используется в v1).
- `GET /fapi/v1/depth?symbol=&limit=` — order book (5/10/20/50/100/500/1000).

## Публичные WS streams
- `<symbol>@ticker` — 24h ticker.
- `<symbol>@bookTicker` — best bid/ask.
- `<symbol>@markPrice` — mark price + funding (обновление 1s или 3s).
- `<symbol>@depth20@100ms` — partial book depth.
- `<symbol>@depth@100ms` — diff depth (incremental).

## Приватные REST endpoints
- `GET /fapi/v2/balance` — USDT-M balance.
- `GET /fapi/v2/positionRisk` — позиции.
- `GET /fapi/v1/openOrders?symbol=` — открытые ордера.
- `POST /fapi/v1/order` — разместить (newClientOrderId, side, type, quantity, price, reduceOnly, timeInForce).
- `DELETE /fapi/v1/order?orderId=&symbol=` — отменить.
- `GET /fapi/v1/order?orderId=&symbol=` — query.
- `POST /fapi/v1/leverage` — set leverage.
- `POST /fapi/v1/marginType` — set margin mode.

## Приватные WS
- Listen key: `POST /fapi/v1/listenKey` (без подписи).
- Stream: `wss://fstream.binance.com/ws/<listenKey>`.
- Events: `ORDER_TRADE_UPDATE`, `ACCOUNT_UPDATE`, `MARGIN_CALL`, `listenKeyExpired`.

## Funding
- Binance USDT-M: интервал 1h, 4h или 8h в зависимости от инструмента (проверять в exchangeInfo).
- Конвенция (раздел 3.2): rate > 0 → long платит short (единообразно с системой).

## ADL
- `GET /fapi/v1/adlQuantile?symbol=` — ADL queue position per symbol.

## Position mode
- One-way или Hedge mode: `POST /fapi/v1/positionSide/dual`.
- В v1 — требуемый режим настраивается через SetPositionMode (раздел 5.3).

## Rate limits
- IP-based и UID-based веса (request weight, order count).
- Лимиты по 1m/5m/1h/1d — фиксировать в адаптере через rate limiter (раздел 7.4).
- Проверять `X-MBX-USED-WEIGHT-1M` и `X-MBX-USED-WEIGHT-1S` headers.

## clientOrderId
- До 36 символов; алфавит совместим с нашей схемой (заглавные буквы, цифры, двоеточие/дефис).
- **ВНИМАНИЕ:** проверить, что Binance принимает ':' в newClientOrderId — если нет,
  использовать только дефис как разделитель в Format().

## Особенности
- Margin modes: ISOLATED / CROSSED (переключается через /marginType).
- reduce-only: параметр `reduceOnly=true` в POST /order (только one-way mode).
- Funding payment приходит в `ACCOUNT_UPDATE` и в ledger; можно сверить через
  `/fapi/v1/income?incomeType=FUNDING_FEE`.

## Известные ограничения / TODO для production
- [ ] Сверить форматы JSON ответов с актуальной версией API.
- [ ] Проверить ':' в newClientOrderId; при необходимости изменить Format() разделитель.
- [ ] Уточнить funding interval per-symbol (1h/4h/8h) — брать из `exchangeInfo`.
- [ ] IP whitelist: Binance требует IP-привязку через account settings.
- [ ] Withdrawal через Futures API не доступен — нужен Spot API endpoint /sapi/v1/capital/withdraw/apply.
