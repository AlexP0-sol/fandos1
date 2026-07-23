# KuCoin Futures — контракт адаптера

> **ВАЖНО:** все endpoint-ы ОБЯЗАТЕЛЬНО сверить с актуальной официальной документацией
> https://www.kucoin.com/docs-new перед production-интеграцией.

## Дата проверки
2026-07 (ExaSearch по официальной документации KuCoin).

## Версия API
Futures REST API v1; аутентификация KC-API v2 (KC-API-KEY-VERSION: 2).

## Base URL
- Futures Production: `https://api-futures.kucoin.com`
- Spot Production (для withdraw/deposit/networks): `https://api.kucoin.com`
- WS Futures: `wss://ws-api-futures.kucoin.com`

## Тип инструмента
Linear USDT perpetual (isInverse=false, settleCurrency=USDT).

## Формат symbol
`<ASSET>USDTM`, например `XBTUSDTM`, `ETHUSDTM`.

**ВАЖНО:** KuCoin называет Bitcoin `XBT` в Futures символах (не `BTC`).

## XBT/BTC маппинг (КРИТИЧНО)

KuCoin Futures использует `XBT` как baseCurrency для Bitcoin-контрактов:
- `XBTUSDTM` — Bitcoin USDT perpetual (baseCurrency: `XBT`)

При парсинге инструментов адаптер нормализует:
- `XBT` → `BTC` в поле `CanonicalBaseAsset`
- При конструировании exchange-символа: `BTC` → `XBT` (если потребуется)

## Лоты (КРИТИЧНО)

KuCoin Futures оперирует **целыми числами лотов**, а не дробными базовыми количествами.

- `size` в ордерах = количество лотов (integer, не float)
- `currentQty` в позициях = лоты (signed integer: >0 long, <0 short)
- `multiplier` = размер 1 лота в базовой монете (например 0.001 BTC для XBTUSDTM)

**Конверсии (адаптер):**
```
BaseQty → лоты: lots = floor(BaseQty / multiplier)  // округление вниз
Лоты → BaseQty: baseQty = abs(lots) * multiplier
```

`multiplier` кешируется из `GET /api/v1/contracts/active` при вызове `GetInstruments`.
При первом вызове `PlaceOrder` без предварительного `GetInstruments` — ошибка.

## Подпись REST (KC-API v2) — VERIFIED

**Строка для подписи:**
```
str = timestamp_ms + METHOD + endpoint_with_query + body
```

- Для GET/DELETE: `endpoint_with_query = /api/v1/ticker?symbol=XBTUSDTM`, body = ""
- Для POST: `endpoint = /api/v1/orders`, body = JSON-строка (без query в endpoint)

**KC-API-SIGN:** `base64(HMAC-SHA256(secret, str))`

**KC-API-PASSPHRASE (v2!):** `base64(HMAC-SHA256(secret, passphrase))`
Не plain passphrase! Это отличие v2 от v1.

**Обязательные заголовки:**
```
KC-API-KEY:         <api_key>
KC-API-SIGN:        <base64(HMAC-SHA256(secret, str))>
KC-API-TIMESTAMP:   <milliseconds UTC>
KC-API-PASSPHRASE:  <base64(HMAC-SHA256(secret, passphrase))>
KC-API-KEY-VERSION: 2
Content-Type:       application/json  (для POST)
```

## Конверт ответа — VERIFIED

```json
{"code":"200000","data":{...}}
```

`code != "200000"` → ошибка. Маппинг кодов:

| Код    | Sentinel | Статус |
|--------|----------|--------|
| 429000 | ErrRateLimited | VERIFIED |
| 400003 | ErrUnauthorized (key/IP) | VERIFIED |
| 400004 | ErrUnauthorized (passphrase) | VERIFIED |
| 400005 | ErrUnauthorized (signature) | VERIFIED |
| 100001 | ErrInvalidSymbol (при "symbol" в msg) | TODO:VERIFY |
| 300003 | ErrInsufficientMargin | TODO:VERIFY |
| 100004 | ErrOrderNotFound | TODO:VERIFY |
| 200004 | ErrInsufficientMargin (insufficient balance) | TODO:VERIFY |

## Публичные REST endpoints — VERIFIED

| Endpoint | Метод | Описание |
|----------|-------|----------|
| `GET /api/v1/timestamp` | Public | Серверное время (data = JSON number ms) |
| `GET /api/v1/contracts/active` | Public | Все торгуемые контракты |
| `GET /api/v1/funding-rate/{symbol}/current` | Public | Текущий + predicted funding rate |
| `GET /api/v1/ticker?symbol=` | Public | Тикер (price, bestBid, bestAsk) |
| `GET /api/v1/level2/depth20?symbol=` | Public | Order book depth 20 |
| `GET /api/v1/level2/depth100?symbol=` | Public | Order book depth 100 (TODO:VERIFY) |

## Приватные REST endpoints — VERIFIED

| Endpoint | Метод | Описание |
|----------|-------|----------|
| `GET /api/v1/account-overview?currency=USDT` | Signed GET | Баланс futures |
| `GET /api/v1/positions` | Signed GET | Все позиции |
| `GET /api/v1/orders?status=active&symbol=` | Signed GET | Открытые ордера |
| `POST /api/v1/orders` | Signed POST | Разместить ордер |
| `DELETE /api/v1/orders/{orderId}` | Signed DELETE | Отменить по orderId |
| `GET /api/v1/orders/{orderId}` | Signed GET | Состояние ордера по orderId |
| `GET /api/v1/orders/byClientOid?clientOid=` | Signed GET | Состояние/резолв по clientOid |

## Spot API endpoints (api.kucoin.com) — TODO:VERIFY

| Endpoint | Описание |
|----------|----------|
| `POST /api/v1/withdrawals` | Создание вывода |
| `GET /api/v1/withdrawals` | История выводов |
| `GET /api/v1/deposits` | История депозитов |
| `GET /api/v3/currencies/{currency}` | Информация по сетям |

## Transfer — TODO:VERIFY

| Endpoint | Описание |
|----------|----------|
| `POST /api/v3/transfer-out` | Futures → Main/Trade |
| `POST /api/v3/transfer-in` | Main/Trade → Futures |

## PlaceOrder — параметры — VERIFIED

```json
{
  "clientOid":    "<uuid-like, max 40, [A-Za-z0-9_-]>",
  "symbol":       "XBTUSDTM",
  "side":         "buy" | "sell",
  "type":         "limit" | "market",
  "size":         100,           // ЦЕЛОЕ число лотов (не base qty)
  "price":        "50000",       // только для limit
  "timeInForce":  "GTC" | "IOC" | "FOK",
  "reduceOnly":   false,
  "positionSide": "BOTH",        // one-way mode
  "leverage":     3,             // TODO: параметризовать
  "marginMode":   "ISOLATED"     // TODO: параметризовать
}
```

Ответ: `{"orderId": "...", "clientOid": "..."}`

**clientOid:** максимум 40 символов, разрешены буквы, цифры, `-`, `_` — VERIFIED.

## SetLeverage — TODO:VERIFY

KuCoin Futures передаёт `leverage` в каждом ордере (per-order).
Отдельный endpoint `POST /api/v2/position/changeLeverage` TODO:VERIFY.
Текущая реализация: no-op с комментарием.

## SetMarginMode — TODO:VERIFY

`marginMode: "ISOLATED" | "CROSS"` передаётся в ордере.
Отдельного endpoint для смены режима не верифицировано.

## SetPositionMode — TODO:VERIFY

KuCoin Futures по умолчанию one-way (positionSide=BOTH).
Hedge mode появился, но endpoint не верифицирован.
Текущая реализация: no-op.

## GetADLState

Нет публичного ADL endpoint у KuCoin Futures.
Возвращает нулевой ADLState.
`delevPercentage` в positions WebSocket TODO:VERIFY как ADL-индикатор.

## WebSocket — TODO:VERIFY

KuCoin WS требует **bullet-token** перед подключением:
- Публичный: `POST /api/v1/bullet-public` → token + endpoint
- Приватный: `POST /api/v1/bullet-private` → token + endpoint (signed)

Без токена подключение невозможно. Реализация WS не включена в адаптер.
`SubscribePublic` и `SubscribePrivate` возвращают `ErrWSNotImplemented`.

WS base URL: `wss://ws-api-futures.kucoin.com`

Публичные топики: `/contractMarket/ticker:{symbol}`, `/contractMarket/level2Depth20:{symbol}`
Приватные топики: `/contract/order`, `/contract/wallet`, `/contract/positionAll`

## Funding

- Endpoint: `GET /api/v1/funding-rate/{symbol}/current`
- Поля ответа: `value` (realized), `predictedValue`, `fundingTime` (ms, следующий), `granularity` (ms)
- Интервал по умолчанию: 8h (28800000 ms)
- Confidence policy: < 30 мин → HIGH, < 4 ч → MEDIUM, иначе → LOW

## account-overview vs positions

- `account-overview` возвращает **один** баланс по валюте (USDT)
- `positions` возвращает список позиций (все символы)

## clientOid

- Параметр: `clientOid`
- Максимум 40 символов
- Допустимые символы: буквы, цифры, `-`, `_` — VERIFIED
- Уникальность: KuCoin требует уникальности per-account

## Rate limits

TODO:VERIFY: конкретные лимиты и заголовки ответа.

## Права ключа

- Futures: `Trade` (для ордеров)
- Spot: `General` (для withdraw/deposit history), `Withdraw` (для вывода)

## TODO для production

- [ ] Верифицировать точные поля `GET /api/v1/positions` (liquidationPrice, maintMarginReq)
- [ ] Верифицировать `POST /api/v3/transfer-out` / `transfer-in` (структура запроса)
- [ ] Верифицировать `/api/v1/withdrawals`, `/api/v1/deposits` (spot API поля)
- [ ] Верифицировать `/api/v3/currencies/{currency}` (поля chain/network)
- [ ] Верифицировать error code 100001 → ErrInvalidSymbol
- [ ] Верифицировать error code 300003 → ErrInsufficientMargin
- [ ] Верифицировать error code 100004 → ErrOrderNotFound
- [ ] Реализовать WS (bullet-token flow)
- [ ] Параметризовать leverage и marginMode в PlaceOrder
- [ ] Верифицировать SetLeverage endpoint (`/api/v2/position/changeLeverage`)
- [ ] Верифицировать Hedge position mode endpoint
- [ ] Проверить rate limit заголовки в ответах
