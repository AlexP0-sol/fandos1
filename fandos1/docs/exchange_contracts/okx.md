# OKX Linear USDT-Margined Perpetual (SWAP) — контракт адаптера

> **WARNING:** все endpoint-ы, поля запросов/ответов и правила подписи ОБЯЗАТЕЛЬНО сверить
> с актуальной официальной документацией OKX V5 (https://www.okx.com/docs-v5/en/)
> перед production-интеграцией. Поля помеченные `TODO:VERIFY` проверены неполностью.

## Дата проверки
VERIFIED — 2026-07 (по документации OKX V5 и CCXT source).

## Версия API
V5 API. Base URL: `https://www.okx.com`.

## Тип инструмента
SWAP (perpetual), USDT-margined linear. Фильтр: `instType=SWAP && settleCcy==USDT && ctType==linear`.

## Формат symbol
`<ASSET>-USDT-SWAP`, например `BTC-USDT-SWAP`, `ETH-USDT-SWAP`.

## Подпись REST (VERIFIED)

```
pre-hash = timestamp + METHOD + requestPath + body
sign     = base64(HMAC-SHA256(secretKey, pre-hash))
```

- `timestamp` — ISO 8601 UTC с миллисекундной точностью: `"2020-12-08T09:08:57.715Z"`.
- `METHOD` — uppercase: `"GET"` / `"POST"`.
- `requestPath` — для GET: `/api/v5/path?key=val` (включает query-строку); для POST: `/api/v5/path`.
- `body` — JSON-строка для POST, пустая строка `""` для GET.
- Заголовки аутентификации:
  - `OK-ACCESS-KEY` — API ключ.
  - `OK-ACCESS-SIGN` — base64-подпись.
  - `OK-ACCESS-TIMESTAMP` — timestamp в ISO8601 формате.
  - `OK-ACCESS-PASSPHRASE` — passphrase (OKX-специфично, обязателен).

## Конверт ответа (VERIFIED)

```json
{"code":"0","msg":"","data":[...]}
```

`code != "0"` → ошибка. Маппинг:

| code | sentinel-ошибка |
|------|----------------|
| `50011`, `429` | `ErrRateLimited` |
| `50111`, `50113`, `50112`, `50114` | `ErrUnauthorized` |
| `51000` (с instId), `51001`, `51002` | `ErrInvalidSymbol` |
| `51008`, `51502` | `ErrInsufficientMargin` |
| `51603` | `ErrOrderNotFound` |

## Публичные REST endpoints (VERIFIED)

| Endpoint | Метод | Описание |
|----------|-------|----------|
| `GET /api/v5/public/time` | GET | Серверное время (`ts` — ms) |
| `GET /api/v5/public/instruments?instType=SWAP` | GET | Реестр инструментов |
| `GET /api/v5/public/funding-rate?instId=` | GET | Funding rate (текущий + прогноз) |
| `GET /api/v5/market/ticker?instId=` | GET | Тикер (last/bid/ask/vol) |
| `GET /api/v5/market/books?instId=&sz=` | GET | Стакан (books) |

## Приватные REST endpoints (VERIFIED)

| Endpoint | Метод | Описание |
|----------|-------|----------|
| `GET /api/v5/account/balance` | GET | Баланс (availEq, eq per currency) |
| `GET /api/v5/account/positions?instType=SWAP` | GET | Позиции (pos, avgPx, liqPx, adl) |
| `GET /api/v5/trade/orders-pending?instType=SWAP&instId=` | GET | Открытые ордера |
| `POST /api/v5/trade/order` | POST | Разместить ордер |
| `POST /api/v5/trade/cancel-order` | POST | Отменить ордер |
| `GET /api/v5/trade/order?instId=&clOrdId=` | GET | Запрос состояния ордера |
| `POST /api/v5/account/set-leverage` | POST | Установить плечо |
| `POST /api/v5/account/set-position-mode` | POST | Режим позиций (net/long-short) |
| `POST /api/v5/asset/transfer` | POST | Внутренний перевод |
| `POST /api/v5/asset/withdrawal` | POST | Вывод средств |
| `GET /api/v5/asset/withdrawal-history` | GET | История выводов |
| `GET /api/v5/asset/deposit-history` | GET | История депозитов |
| `GET /api/v5/asset/currencies?ccy=` | GET | Информация по сетям |

## Конверсия контрактов (VERIFIED)

OKX использует **контракты** как единицу qty для SWAP-ордеров, а не базовую монету.

- `ctVal` = размер одного контракта в базовой монете (из `/api/v5/public/instruments`).
- **base qty → контракты:** `floor(baseQty / ctVal)`, округление вниз (целое).
- **контракты → base qty:** `contracts * ctVal`.

Пример: BTC-USDT-SWAP, `ctVal=0.01` BTC/контракт:
- 0.05 BTC → 5 контрактов.
- 5 контрактов → 0.05 BTC.

`ctVal` кешируется per instId после `GetInstruments`; при отсутствии — lazy-запрос инструментов.

**WARNING:** `QtyStep` и `MinQty` в `CanonicalInstrument` хранятся в **базовых единицах**
(`lotSz * ctVal`, `minSz * ctVal`), а не в контрактах.

## clOrdId (VERIFIED)

OKX клиентский ID: только `[a-zA-Z0-9]`, максимум **32 символа**.

Трансляция нашего `ClientOrderID`:
- Символы `_` и `-` удаляются.
- Остальные `[a-zA-Z0-9]` остаются as-is.
- Если результат > 32 символов — обрезается до 32.

Уникальность сохраняется: компоненты ID уже содержат `[A-Z0-9]` с достаточной энтропией.

## ADL (VERIFIED)

Поле `adl` в ответе `/api/v5/account/positions` — integer `1..5`.
Нормализация: `(adl - 1) / 4` → `[0, 1]`.

## Position mode (VERIFIED)

`POST /api/v5/account/set-position-mode`:
- `posMode = "net_mode"` — one-way.
- `posMode = "long_short_mode"` — hedge (long/short независимо).

## Маржа

`tdMode = "cross"` для всех SWAP-ордеров (cross margin по умолчанию).

## Funding

- Интервал: 8 часов по умолчанию. `TODO:VERIFY`: точное поле в instruments-response.
- Поля ответа `/api/v5/public/funding-rate`:
  - `fundingRate` — текущая реализованная ставка.
  - `nextFundingRate` — прогноз (может быть пустым).
  - `fundingTime` — следующий settlement timestamp (ms).

## WebSocket

- **Не реализован** в текущей версии: `SubscribePublic`/`SubscribePrivate` возвращают `ErrWSNotImplemented`.
- TODO: реализовать `wss://ws.okx.com:8443/ws/v5/public` (channels: `tickers`, `books5`).
- TODO: приватный WS с login-frame `{"op":"login","args":[{apiKey,passphrase,timestamp,sign}]}`.

## Rate limits

- Safe=true только для GET. POST (ордера/переводы) Safe=false.
- `TODO:VERIFY`: точная таблица weight-лимитов OKX V5.

## TODO для production

- [ ] Сверить поле `fundingInterval` в ответе `/api/v5/public/instruments` (точное имя).
- [ ] Проверить маппинг account type для `InternalTransfer` (from/to: "6"=funding, "18"=trading).
- [ ] Формат chain в `Withdraw` (например "USDT-TRC20" или "TRC20").
- [ ] Поле `fee` в `Withdraw` (обязательно? получать через GetNetworkInfo).
- [ ] Поле `SetMarginMode` — уточнить endpoint для per-instrument cross/isolated в OKX V5.
- [ ] Реализовать WebSocket подписки.
- [ ] Проверить "already set" response при `SetPositionMode` (нет отдельного кода от OKX).
- [ ] Верифицировать поля `withdrawalHistoryData`, `depositHistoryData`, `currencyData`.
