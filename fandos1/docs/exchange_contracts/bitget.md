# Bitget USDT-M Perpetual Futures — контракт адаптера

> **ВАЖНО (раздел 2 промпта v2):** все пути endpoint-ов ОБЯЗАТЕЛЬНО сверить с актуальной
> официальной документацией https://www.bitget.com/api-doc/contract/intro перед
> production-интеграцией. Bitget меняет поля и коды ошибок между минорными версиями.

## Дата проверки
2026-07. Верифицировано по документации Bitget API (classic/quickStart/intro + contract endpoints).

## Версия API
V2 API (`/api/v2/...`).

## Base URL
- Production REST: `https://api.bitget.com`
- WS Public:  `wss://ws.bitget.com/v2/ws/public`  (VERIFIED)
- WS Private: `wss://ws.bitget.com/v2/ws/private` (TODO:VERIFY точный путь)

## Тип инструмента
USDT-M Perpetual Futures (`productType=USDT-FUTURES`).

## Формат symbol
`<ASSET>USDT`, например `BTCUSDT`, `ETHUSDT`. (VERIFIED)

## Подпись REST (VERIFIED)

Алгоритм HMAC-SHA256 + base64 (не hex как у Bybit):

```
preHash  = timestamp + METHOD.toUpperCase() + requestPath [+"?"+queryString] [+ body]
signature = base64(HMAC-SHA256(secretKey, preHash))
```

- Если `queryString` пустой — `"?"` опускается.
- Для GET body не добавляется.
- Для POST queryString пустой, body = JSON-строка тела.

### Обязательные заголовки (VERIFIED)

| Заголовок          | Описание                            |
|--------------------|-------------------------------------|
| `ACCESS-KEY`       | API Key                             |
| `ACCESS-SIGN`      | base64(HMAC-SHA256(secret, preHash))|
| `ACCESS-TIMESTAMP` | Unix ms (миллисекунды UTC)          |
| `ACCESS-PASSPHRASE`| Passphrase (установленный при создании key) |
| `Content-Type`     | `application/json` (для POST)       |
| `locale`           | `en-US`                             |

### Отличие от Bybit
- Bybit V5: подпись hex-encoded, передаётся через `X-BAPI-SIGN`.
- Bitget V2: подпись **base64**-encoded, передаётся через `ACCESS-SIGN`.
- Bitget требует `ACCESS-PASSPHRASE` (у Bybit passphrase отсутствует).

## Timestamp / recv_window
- `ACCESS-TIMESTAMP` — миллисекунды UTC.
- Допустимое отклонение: ±30 секунд от серверного времени (VERIFIED).
- Bitget не использует `recv_window` (в отличие от Bybit).

## Учётные данные
API Key, API Secret, **Passphrase** (обязательна). При утере Passphrase необходимо пересоздать ключ.

## Конверт ответа (VERIFIED)

```json
{
  "code": "00000",
  "msg": "success",
  "data": {...},
  "requestTime": 1700000000000
}
```

`code != "00000"` → ошибка. Маппинг кодов (частично TODO:VERIFY):

| Код (строка) | Описание                          | Sentinel-ошибка         |
|-------------|-----------------------------------|-------------------------|
| `"40429"`   | Rate limit exceeded               | `ErrRateLimited`        |
| `"40001"`   | Invalid API key                   | `ErrUnauthorized`       |
| `"40009"`   | Sign error / wrong signature      | `ErrUnauthorized`       |
| `"40034"`   | Symbol not exist                  | `ErrInvalidSymbol`      |
| `"40754"`   | Insufficient margin               | `ErrInsufficientMargin` |
| `"40109"`   | Order not found                   | `ErrOrderNotFound`      |

> **TODO:VERIFY** — точные коды ошибок не подтверждены по реальным запросам;
> взяты из косвенной документации и аналогии с другими биржами.

## Публичные REST endpoints

| Endpoint | Метод | Статус |
|---|---|---|
| `/api/v2/public/time` | GET | **VERIFIED** |
| `/api/v2/mix/market/contracts?productType=USDT-FUTURES` | GET | **VERIFIED** |
| `/api/v2/mix/market/current-fund-rate?symbol=&productType=` | GET | **VERIFIED** |
| `/api/v2/mix/market/funding-time?symbol=&productType=` | GET | **VERIFIED** |
| `/api/v2/mix/market/ticker?symbol=&productType=` | GET | **VERIFIED** |
| `/api/v2/mix/market/merge-depth?symbol=&productType=&limit=` | GET | **VERIFIED** |

## Приватные REST endpoints

| Endpoint | Метод | Статус |
|---|---|---|
| `/api/v2/mix/account/accounts?productType=` | GET | **VERIFIED** |
| `/api/v2/mix/position/all-position?productType=` | GET | **VERIFIED** |
| `/api/v2/mix/order/orders-pending?productType=` | GET | **VERIFIED** |
| `/api/v2/mix/order/place-order` | POST | **VERIFIED** |
| `/api/v2/mix/order/cancel-order` | POST | **VERIFIED** |
| `/api/v2/mix/order/detail?symbol=&clientOid=` | GET | **VERIFIED** |
| `/api/v2/mix/account/set-leverage` | POST | **VERIFIED** |
| `/api/v2/mix/account/set-margin-mode` | POST | **VERIFIED** |
| `/api/v2/mix/account/set-position-mode` | POST | **VERIFIED** |
| `/api/v2/spot/wallet/transfer` | POST | VERIFIED (endpoint) |
| `/api/v2/spot/wallet/withdrawal` | POST | VERIFIED (endpoint) |
| `/api/v2/spot/wallet/withdrawal-records` | GET | VERIFIED (endpoint) |
| `/api/v2/spot/wallet/deposit-records` | GET | VERIFIED (endpoint) |
| `/api/v2/spot/public/coins` | GET | VERIFIED (endpoint) |

## Поля контрактов (GetInstruments)

| Поле API | Маппинг | Статус |
|---|---|---|
| `symbol` | `ExchangeSymbol` | VERIFIED |
| `baseCoin` | `CanonicalBaseAsset` | VERIFIED |
| `symbolStatus = "normal"` | `InstrumentStatusActive` | VERIFIED |
| `minTradeNum` | `MinQty` | VERIFIED |
| `volumePlace` | `QtyStep = 10^(-volumePlace)` | TODO:VERIFY |
| `pricePlace` / `priceEndStep` | `TickSize = priceEndStep * 10^(-pricePlace)` | TODO:VERIFY |
| `maxLeverageOver` | `MaxLeverage` | TODO:VERIFY (название поля) |
| `fundingInterval` | `FundingIntervalSec` (предполагаем часы) | TODO:VERIFY единицы |

## PlaceOrder — тело запроса (VERIFIED основные поля)

```json
{
  "symbol": "BTCUSDT",
  "productType": "USDT-FUTURES",
  "marginMode": "crossed",
  "marginCoin": "USDT",
  "size": "0.001",
  "price": "50000",
  "side": "buy",
  "tradeSide": "open",
  "orderType": "limit",
  "force": "ioc",
  "clientOid": "myorder-123",
  "reduceOnly": "NO"
}
```

| Поле | Значения | Статус |
|---|---|---|
| `side` | `"buy"` / `"sell"` | VERIFIED |
| `tradeSide` | `"open"` / `"close"` (hedge mode) | VERIFIED |
| `orderType` | `"market"` / `"limit"` | VERIFIED |
| `force` | `"gtc"` / `"ioc"` / `"fok"` / `"post_only"` | VERIFIED |
| `reduceOnly` | `"YES"` / `"NO"` (one-way mode) | VERIFIED |
| `clientOid` | до 40 символов `[A-Za-z0-9_#-]` | TODO:VERIFY точный regex |
| `size` | base coin qty (для USDT-FUTURES) | VERIFIED |

## Funding

- Интервал: 8 ч по умолчанию для USDT-M Perpetual (VERIFIED).
- Отдельные эндпоинты для rate и time (в отличие от Bybit V5 tickers).
- `fundingRate × notional`, положительный → long платит short.

## Позиции

- `holdSide`: `"long"` / `"short"` (VERIFIED, в отличие от Bybit `"Buy"` / `"Sell"`).
- `total`: размер позиции в base coin (TODO:VERIFY vs `size`).
- `openPriceAvg`: средняя цена входа (VERIFIED).

## ADL

- TODO:VERIFY: Bitget не документирует явного ADL-ранга в `/api/v2/mix/position/all-position`.
- Адаптер возвращает нулевой `ADLState` — без вызовов к бирже.

## Position Mode

- `posMode`: `"one_way_mode"` / `"hedge_mode"` (VERIFIED).
- Endpoint: `POST /api/v2/mix/account/set-position-mode` (VERIFIED).

## clientOrderId

- Параметр `clientOid`.
- TODO:VERIFY точный regex и максимальная длина (предположительно 40 символов).
- В тестах используем: `[A-Za-z0-9_-]`.

## WebSocket

- Публичные: `wss://ws.bitget.com/v2/ws/public` (VERIFIED URL).
- Приватные: `wss://ws.bitget.com/v2/ws/private` (TODO:VERIFY).
- WS **не реализован** в текущей версии адаптера: `SubscribePublic` и `SubscribePrivate`
  возвращают `errWSNotImplemented`.

## Rate Limits

- REST: 6000 requests/IP/min общий лимит (VERIFIED).
- При превышении: HTTP 429 или envelope `code="40429"` (TODO:VERIFY строковый код).

## Особенности vs Bybit

| Aspect | Bybit V5 | Bitget V2 |
|---|---|---|
| Signature | HMAC-SHA256 hex | HMAC-SHA256 **base64** |
| Auth headers | `X-BAPI-*` | `ACCESS-*` |
| Passphrase | Нет | **Обязательна** |
| Envelope | `{"retCode":0,...}` int | `{"code":"00000",...}` string |
| productType | `category=linear` | `productType=USDT-FUTURES` |
| Side | `"Buy"/"Sell"` | `"buy"/"sell"` |
| Position side | `"Buy"/"Sell"` | `"long"/"short"` |
| Funding | Один эндпоинт (tickers) | Два: current-fund-rate + funding-time |

## TODO для production

- [ ] Верифицировать точные коды ошибок (40429, 40001, 40009, 40034, 40754, 40109) по реальным ответам API.
- [ ] Верифицировать поля `volumePlace` → `qtyStep` и `pricePlace`/`priceEndStep` → `tickSize`.
- [ ] Верифицировать поле `maxLeverageOver` или аналог.
- [ ] Верифицировать единицы `fundingInterval` (часы vs минуты).
- [ ] Верифицировать regex для `clientOid` (длина ≤40 и допустимые символы).
- [ ] Верифицировать ADL field в позициях (или подтвердить отсутствие).
- [ ] Реализовать WebSocket: pub/priv channels (ticker, orderbook, orders, positions, wallet).
- [ ] Параметризировать `marginMode` в PlaceOrder/SetMarginMode.
- [ ] Верифицировать поле `total` vs `size` в позициях.
- [ ] Верифицировать `baseVolume` (filledQty) и `fillPrice` (avgFillPrice) в ордерах.
