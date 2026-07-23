# Gate.io USDT-settled Perpetual Futures — контракт адаптера

> **ВАЖНО:** все пути endpoint-ов ОБЯЗАТЕЛЬНО сверить с актуальной официальной документацией
> https://www.gate.io/docs/developers/apiv4/ перед production-интеграцией.

## Дата проверки

2026-07. Signing algorithm — VERIFIED (официальная документация gateio.ws).
Endpoint-пути — VERIFIED по changelog gateio.ws (v4). Поля ответов — VERIFIED для основных,
часть помечена TODO:VERIFY (см. ниже).

## Версия API

APIv4. REST prefix: `/api/v4`. Settle-параметр: `usdt`.

## Base URLs

| Назначение       | URL |
|------------------|-----|
| REST Production  | `https://api.gateio.ws` |
| REST Testnet     | `https://fx-api-testnet.gateio.ws` |
| WS Futures USDT  | `wss://fx-ws.gateio.ws/v4/ws/usdt` (TODO:VERIFY) |

## Тип инструмента

USDT-settled linear perpetual. Settle = `usdt`.

## Формат symbol

`<ASSET>_USDT`, например `BTC_USDT`, `ETH_USDT`. Обратите внимание на нижнее подчёркивание —
в отличие от Bybit (`BTCUSDT`) и Binance (`BTCUSDT`).

---

## Подпись REST (VERIFIED)

Gate.io APIv4 использует **HMAC-SHA512**.

### Алгоритм

```
signature_string = Method + "\n"
                 + URL_Path + "\n"
                 + QueryString + "\n"
                 + HexEncode(SHA512(RequestBody)) + "\n"
                 + Timestamp
SIGN = HexEncode(HMAC-SHA512(api_secret, signature_string))
```

**Детали:**
- `Method` — HTTP-метод в UPPERCASE: `GET`, `POST`, `DELETE`.
- `URL_Path` — путь без хоста и query, например `/api/v4/futures/usdt/orders`.
- `QueryString` — строка параметров без URL-encode, в том порядке что и в URL. Пустая строка `""` если параметров нет.
- `SHA512(RequestBody)` — SHA512 тела запроса, hex-encoded. Для GET/DELETE без тела: `SHA512("")` = `cf83e1357eef...` (фиксированная константа).
- `Timestamp` — Unix timestamp в **секундах** (целое). Передаётся как строка в заголовке.

### Заголовки запроса

```http
KEY: <api_key>
Timestamp: <unix_seconds>
SIGN: <hex_hmac_sha512>
Content-Type: application/json  (для POST с телом)
```

**Passphrase** не используется (в отличие от OKX, Bitget, KuCoin).

### Тестовые векторы (независимо сгенерированы на Python)

**GET** `/api/v4/futures/usdt/contracts?limit=100` timestamp=1700000000 secret=`test-secret-key`:
```
SIGN = f56f0a0ca520cc886eced17e761c207ed2ffc3186d71ec237e4edef15086558bbe36949675795f18e7029726879d097e0cb5c9736d23f8e49ce26b4880e5b770
```

**POST** `/api/v4/futures/usdt/orders` body=`{"contract":"BTC_USDT","size":1}` timestamp=1700000000:
```
SIGN = 25a181446cc2d96f9886c781f126ed0a41cf577bd3e9b556de34d0d68788043ce38cec27a75e4a918e8bc0df41117c92101cc45524fc9d1ad09510a05378921d
```

---

## Особенности Gate.io futures (КРИТИЧНО)

### Количество контрактов vs base qty

Gate.io фьючерсы оперируют **контрактами** (целые числа), не базовым активом:

- `size` в ордерах/позициях — **целое со знаком**: положительное = long, отрицательное = short.
- `quanto_multiplier` из ответа contracts — размер одного контракта в единицах базового актива.

**Конверсия:**
```
contracts = floor(baseQty / quanto_multiplier)   # округление вниз, никогда не превышать
baseQty   = abs(contracts) × quanto_multiplier
```

**Пример (BTC_USDT, quanto_multiplier = 0.0001):**
```
10 BTC → 10 / 0.0001 = 100 000 контрактов
100 000 контрактов × 0.0001 = 10 BTC  ✓ (round-trip)
0.00015 BTC → floor(0.00015 / 0.0001) = 1 контракт (не 2!)
```

**Кэш quanto_multiplier:** адаптер кэширует multiplier in-memory после первого запроса
`GET /api/v4/futures/usdt/contracts/{contract}`. Кэш не персистентный — сбрасывается при рестарте.

### Знак size = сторона позиции

| size   | Сторона  |
|--------|----------|
| > 0    | Long     |
| < 0    | Short    |
| = 0    | Нет позиции (пропускается) |

При PlaceOrder:
- `SideLong` → `size = +contracts`
- `SideShort` → `size = -contracts`

### Рыночный ордер

```json
{"price": "0", "tif": "ioc"}
```

---

## ClientOrderID → text (VERIFIED)

Gate.io использует поле `text` для пользовательского идентификатора.

**Правила:**
- Обязательный префикс `t-` (Gate.io требует).
- Итоговый `text` ≤ 30 символов суммарно (т.е. ≤ 28 символов после `t-`).
- Допустимые символы: буквы `[A-Za-z0-9._-]`.
- Недопустимые символы (`:`, `/` и т.п.) удаляются при санитизации.
- При превышении длины — детерминированное обрезание хвоста.

**Пример:** clientID = `my-order-abc-123` → text = `t-my-order-abc-123`

---

## Резолюция ClientOrderID → ExchangeOrderID

**Ограничение:** Gate.io не предоставляет прямой REST-поиск ордеров по `text` через официально
документированный параметр. Адаптер использует следующую стратегию:

1. **In-memory map** `clientID → exchangeOrderID` — заполняется при PlaceOrder.
2. При наличии ExchangeOrderID — GET `/api/v4/futures/usdt/orders/{order_id}`.
3. При наличии clientID в map — то же.
4. **Фоллбэк** (только после рестарта когда map пуст): итерация открытых + завершённых ордеров
   с фильтрацией по полю `text`.

**ОГРАНИЧЕНИЕ после рестарта:** маппинг clientID → orderID теряется. Если ордер давно
завершён и не найден в открытых — может быть не найден. Для production рекомендуется
персистентный маппинг (БД или Redis).

---

## Публичные REST endpoints (VERIFIED)

| Endpoint | Описание |
|----------|----------|
| `GET /api/v4/spot/time` | Серверное время (поле `server_time`, Unix seconds) |
| `GET /api/v4/futures/usdt/contracts` | Список всех контрактов |
| `GET /api/v4/futures/usdt/contracts/{contract}` | Один контракт (funding_rate, funding_next_apply, quanto_multiplier) |
| `GET /api/v4/futures/usdt/tickers?contract=` | Тикер (last, mark_price, index_price, volume_24h_settle) |
| `GET /api/v4/futures/usdt/order_book?contract=&limit=` | Стакан (asks/bids как `[[price, size], ...]`) |

---

## Приватные REST endpoints (VERIFIED)

| Endpoint | Метод | Описание |
|----------|-------|----------|
| `/api/v4/futures/usdt/accounts` | GET | Баланс (total, available, currency) |
| `/api/v4/futures/usdt/positions` | GET | Позиции (size со знаком, entry_price, mark_price, liq_price, margin) |
| `/api/v4/futures/usdt/orders?status=open&contract=` | GET | Открытые ордера |
| `/api/v4/futures/usdt/orders/{order_id}` | GET | Один ордер по exchange ID |
| `/api/v4/futures/usdt/orders` | POST | Создать ордер |
| `/api/v4/futures/usdt/orders/{order_id}` | DELETE | Отменить ордер |
| `/api/v4/futures/usdt/positions/{contract}/leverage?leverage=` | POST | Установить плечо |
| `/api/v4/wallet/transfers` | POST | Перевод spot ↔ futures |
| `/api/v4/withdrawals` | POST | Вывод средств |
| `/api/v4/withdrawals` | GET | История выводов |
| `/api/v4/wallet/deposits` | GET | История депозитов |
| `/api/v4/wallet/currency_chains?currency=` | GET | Сети для актива |

---

## TODO:VERIFY

| Endpoint / поле | Описание |
|-----------------|----------|
| `GET /api/v4/futures/usdt/dual_mode` | Чтение текущего режима позиций (поле dual_mode) |
| `POST /api/v4/futures/usdt/dual_mode?dual_mode=false` | Переключение one-way/hedge режима |
| Поле `adl_ranking` в позициях | Gate.io может называть поле иначе; нормализация [1-5] → [0,1] |
| `GET /api/v4/futures/usdt/orders?status=finished&text=t-...` | Поиск завершённых ордеров по text-полю |
| WS URI `wss://fx-ws.gateio.ws/v4/ws/usdt` | Проверить в production |
| WS каналы `futures.tickers`, `futures.order_book` | Формат подписки и сообщений |
| WS приватная аутентификация | Формат auth-фрейма Gate.io |
| `POST /api/v4/futures/usdt/positions/{contract}/leverage` с leverage=0 | Cross margin mode через leverage=0 |
| Поля `withdraw_fix`, `withdraw_amount_mini`, `deposit_amount_mini` в `/wallet/currency_chains` | Точные имена полей комиссий |

---

## Funding

- Поля в ответе `/api/v4/futures/usdt/contracts/{contract}`:
  - `funding_rate` — текущий funding rate (строка).
  - `funding_interval` — интервал в **секундах** (обычно 28800 = 8h).
  - `funding_next_apply` — Unix timestamp следующего применения (**float64, секунды**).
- Gate.io использует mark-price для расчёта funding.

**Confidence policy:**
| Времени до funding | ConfidenceLevel |
|--------------------|-----------------|
| < 30 мин           | HIGH            |
| < 4 ч              | MEDIUM          |
| иначе              | LOW             |

---

## Маппинг ошибок

| HTTP status / label | Sentinel error |
|---------------------|----------------|
| HTTP 429 | `ErrRateLimited` |
| HTTP 401/403 | `ErrUnauthorized` |
| label `INVALID_KEY` | `ErrUnauthorized` |
| label `INVALID_SIGNATURE` | `ErrUnauthorized` |
| label `ORDER_NOT_FOUND` | `ErrOrderNotFound` |
| label `BALANCE_NOT_ENOUGH` | `ErrInsufficientMargin` |
| label `INSUFFICIENT_AVAILABLE` | `ErrInsufficientMargin` |
| label `CONTRACT_NOT_FOUND` | `ErrInvalidSymbol` |
| label `INVALID_CONTRACT` | `ErrInvalidSymbol` |

Тело ошибки: `{"label":"...","message":"..."}`.

---

## WebSocket

**Статус:** не реализован в v1. `SubscribePublic` и `SubscribePrivate` возвращают
`ErrWSNotImplemented`. Используйте REST polling.

**Планируемые каналы (TODO:VERIFY):**
- `futures.tickers` — тикеры фьючерсов
- `futures.order_book` — стакан фьючерсов
- Приватные каналы для ордеров и позиций

---

## Позиции: режим маржи

Gate.io определяет режим маржи через поле `leverage` позиции:
- `leverage = 0` → **cross margin**
- `leverage > 0` → **isolated**, значение = плечо

---

## ADL

TODO:VERIFY: точное имя поля ADL-ранга в ответе GET /api/v4/futures/usdt/positions.
Предполагается `adl_ranking`, значения [1-5], нормализуются в [0,1] делением на 5.

---

## Rate Limits

Gate.io использует rate limiting по IP и API-ключу. HTTP 429 → `ErrRateLimited`.
Точные лимиты зависят от endpoint-а и уровня аккаунта — проверьте документацию.

---

## Права ключа

Для production: Read + Trade (для ордеров/позиций) + Withdraw (для выводов, отдельный ключ).

---

## TODO для production

- [ ] Реализовать WebSocket (SubscribePublic/SubscribePrivate) через `wss://fx-ws.gateio.ws/v4/ws/usdt`
- [ ] Персистентный маппинг clientID → orderID (Redis/PostgreSQL) для надёжного GetOrder после рестарта
- [ ] Проверить поле `adl_ranking` в позициях (TODO:VERIFY)
- [ ] Проверить dual_mode endpoint-ы (TODO:VERIFY)
- [ ] Добавить retry с exponential backoff при HTTP 429
- [ ] Верифицировать поля `/api/v4/wallet/currency_chains` (TODO:VERIFY)
