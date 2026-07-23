# MEXC Linear USDT Perpetual — контракт адаптера

> **ВАЖНО:** все пути endpoint-ов ОБЯЗАТЕЛЬНО сверить с актуальной официальной документацией
> https://mexcdevelop.github.io/apidocs/contract_v1_en/ перед production-интеграцией.

## Дата проверки

Заготовка — 2026-07. Верификация по официальным docs + official Go SDK (mexcdevelop/mexc-api-demo).

## Версия API

Contract API v1 (`/api/v1/contract/...` и `/api/v1/private/...`).

## Base URL

- Production Contract: `https://contract.mexc.com`
- Production Spot (для переводов/выводов): `https://api.mexc.com` — **TODO:VERIFY**
- WS: `wss://contract.mexc.com/edge` (обновлено 2024-01-31, VERIFIED)

## Тип инструмента

LINEAR USDT perpetual, settlement USDT (`settleCoin=USDT`).

## Формат symbol

`<ASSET>_USDT`, например `BTC_USDT`, `ETH_USDT`.

## Подпись REST (VERIFIED)

**Источник:** официальная документация MEXC Contract API v1 (Java-пример) + official Go SDK
(`mexcdevelop/mexc-api-demo/go/clients/futures/signer.go`).

```
stringToSign = accessKey + requestTime + parameterString
signature    = hex(HMAC-SHA256(apiSecret, stringToSign))
```

- **GET:** `parameterString` = отсортированный query string (`key=value&...`, без URL-кодирования,
  ключи отсортированы лексикографически).
- **POST:** `parameterString` = сырое JSON-тело запроса (без сортировки ключей).

### Заголовки приватных запросов (VERIFIED)

| Заголовок      | Значение                             |
|----------------|--------------------------------------|
| `ApiKey`       | Ваш API ключ                         |
| `Request-Time` | Unix timestamp в мс                  |
| `Signature`    | hex(HMAC-SHA256(secret, stringToSign)) |
| `Content-Type` | `application/json`                   |

## Envelope ответа (VERIFIED)

```json
{"success": true, "code": 0, "data": {...}}
{"success": false, "code": 500, "message": "Error description"}
```

`success=false` или `code≠0` — ошибка.

## Маппинг кодов ошибок

| Код  | Sentinel                | Статус     |
|------|-------------------------|------------|
| 401  | `ErrUnauthorized`       | VERIFIED   |
| 1002 | `ErrUnauthorized`       | TODO:VERIFY (типичный MEXC "invalid signature") |
| 429  | `ErrRateLimited`        | VERIFIED   |
| 510  | `ErrRateLimited`        | TODO:VERIFY |
| 2001 | `ErrInvalidSymbol`      | TODO:VERIFY |
| 2011 | `ErrOrderNotFound`      | TODO:VERIFY |
| 2013 | `ErrInsufficientMargin` | TODO:VERIFY |

Дополнительно: эвристика по тексту `message` (содержит "not found" + "order" и т.п.).

## Конверсия объёма: baseQty ↔ vol (контракты)

**ВАЖНО:** MEXC Futures оперирует объёмами в контрактах (`vol`), а не в базовой монете.

```
vol = floor(baseQty / contractSize)    # конверсия для ордеров
baseQty = vol × contractSize           # конверсия для позиций
```

`contractSize` для каждого инструмента берётся из `/api/v1/contract/detail` (поле `contractSize`).
Кешируется в памяти при каждом вызове `GetInstruments`.

**Пример BTC_USDT:** `contractSize = 0.0001 BTC`. Значит, 1 контракт = 0.0001 BTC.
Для `baseQty = 0.05 BTC`: `vol = floor(0.05 / 0.0001) = 500 контрактов`.

**Округление:** всегда вниз (`floor`), никогда не превышать допустимый объём.

## Маппинг side (VERIFIED)

| Код | Значение    | domain.Side  | ReduceOnly |
|-----|-------------|--------------|------------|
| 1   | open long   | `SideLong`   | `false`    |
| 2   | close short | `SideShort`  | `true`     |
| 3   | open short  | `SideShort`  | `false`    |
| 4   | close long  | `SideLong`   | `true`     |

## Маппинг типа ордера

| Код | Тип       |
|-----|-----------|
| 1   | limit     |
| 5   | market    |

## Маппинг openType (маржа)

| Код | Режим    |
|-----|----------|
| 1   | isolated |
| 2   | cross    |

## Публичные REST endpoints (VERIFIED)

| Endpoint                                      | Метод | Назначение                          |
|-----------------------------------------------|-------|-------------------------------------|
| `GET /api/v1/contract/ping`                   | GET   | Серверное время                     |
| `GET /api/v1/contract/detail`                 | GET   | Все контракты (symbol, contractSize, etc.) |
| `GET /api/v1/contract/funding_rate/{symbol}`  | GET   | Funding rate + nextSettleTime       |
| `GET /api/v1/contract/ticker?symbol=`         | GET   | Тикер (lastPrice, bid1, ask1, vol)  |
| `GET /api/v1/contract/depth/{symbol}`         | GET   | Стакан (bids/asks + version/seq)    |

## Приватные REST endpoints (VERIFIED)

| Endpoint                                                  | Метод | Назначение                     |
|-----------------------------------------------------------|-------|--------------------------------|
| `GET /api/v1/private/account/assets`                      | GET   | Балансы (все активы)           |
| `GET /api/v1/private/position/open_positions`             | GET   | Открытые позиции               |
| `GET /api/v1/private/order/list/open_orders`              | GET   | Открытые ордера (по symbol)    |
| `POST /api/v1/private/order/submit`                       | POST  | Разместить ордер               |
| `POST /api/v1/private/order/cancel`                       | POST  | Отменить ордер (массив ID)     |
| `GET /api/v1/private/order/external/{symbol}/{externalOid}` | GET | Получить ордер по externalOid  |
| `POST /api/v1/private/position/change_leverage`           | POST  | Установить плечо               |

## Приватные REST endpoints (TODO:VERIFY)

| Endpoint                                              | Назначение                          |
|-------------------------------------------------------|-------------------------------------|
| `POST /api/v1/private/position/change_margin_type`    | Смена режима маржи                  |
| `POST /api/v1/private/position/position_mode/change`  | Смена режима позиций (one-way/hedge) |
| `POST /api/v1/private/order/cancel_with_external`     | Отмена ордера по externalOid        |

## Spot API для переводов (TODO:VERIFY)

Переводы, вывод, история — через Spot API (`https://api.mexc.com`).
Отдельный `SpotBaseURL` в `Config` (дефолт: `https://api.mexc.com`).
Формат подписи Spot API может отличаться от Contract API — требует отдельной верификации.

Предполагаемые endpoint-ы:
- `POST /api/v3/capital/transfer` — внутренний перевод
- `POST /api/v3/capital/withdraw/apply` — вывод средств
- `GET /api/v3/capital/withdraw/history` — история выводов
- `GET /api/v3/capital/deposit/hisrec` — история депозитов

## WebSocket (TODO:VERIFY)

- Base URL: `wss://contract.mexc.com/edge` (VERIFIED, обновлено 2024-01-31)
- Публичные каналы: `sub.ticker`, `sub.depth` — формат сообщений **TODO:VERIFY**
- Приватная авторизация и каналы — **TODO:VERIFY**

**Текущая реализация:** WS возвращает `errWSNotImplemented`; используйте REST polling.

## Funding

- Интервал: 8h по умолчанию (поле `collectCycle` в часах из funding_rate endpoint).
- Поле `nextSettleTime` — Unix timestamp следующего расчёта в мс.
- `fundingRate` × notional: positive → long платит short.

### Confidence policy

| Условие                       | Уровень   |
|-------------------------------|-----------|
| До nextSettleTime < 30 мин    | HIGH      |
| До nextSettleTime < 4 ч       | MEDIUM    |
| Иначе                         | LOW       |

## ADL

Нет публичного ADL-индикатора в MEXC Contract API (TODO:VERIFY).
`GetADLState` возвращает нулевые значения.

## clientOrderID / externalOid (VERIFIED)

- Поле запроса: `externalOid`
- Формат: `[a-zA-Z0-9_-]`, ≤32 символов
- Запрос по externalOid: `GET /api/v1/private/order/external/{symbol}/{externalOid}`

## Особенности реализации

### Кеш contractSize

`contractSizeMap` заполняется при каждом вызове `GetInstruments` и используется для конверсии
`baseQty ↔ vol`. Если кеш не заполнен, `PlaceOrder` вернёт ошибку.
**Вызывайте `GetInstruments` перед первым `PlaceOrder`.**

### Два API домена

| Домен               | Назначение                          |
|---------------------|-------------------------------------|
| `contract.mexc.com` | Futures/Contract операции (торговля, позиции, рыночные данные) |
| `api.mexc.com`      | Spot операции (переводы, вывод, сети) — TODO:VERIFY |

### Passphrase

MEXC не использует Passphrase (в отличие от OKX/Bitget/KuCoin). Поле в `Config` оставлено для совместимости интерфейса.

## Права ключа

- Futures Trading: чтение позиций, размещение/отмена ордеров.
- Withdrawal: для операций вывода через Spot API.
- IP whitelist поддерживается в UI MEXC.

## TODO для production

- [ ] Верифицировать точные error-коды (1002, 2001, 2011, 2013, 510).
- [ ] Верифицировать endpoint и формат Spot API для переводов/выводов.
- [ ] Верифицировать endpoint для смены margin mode и position mode.
- [ ] Верифицировать WS-каналы и формат сообщений.
- [ ] Верифицировать cancel по externalOid (`/api/v1/private/order/cancel_with_external`).
- [ ] Верифицировать наличие/отсутствие ADL индикатора.
- [ ] Верифицировать поле `collectCycle` (единицы измерения, ненулевое значение).
- [ ] Верифицировать `state` контракта: 0=active, остальные → delisted.
- [ ] Реализовать WS после верификации формата сообщений.
