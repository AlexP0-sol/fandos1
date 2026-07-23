# Модель угроз (Threat Model)

**Связанные разделы промпта:** 1.2, 13, 26, 27, 28.
Методология: STRIDE (Spoofing, Tampering, Repudiation, Information disclosure, Denial of service, Elevation of privilege).

## 1. Границы доверия (Trust boundaries)

```
TB1 — Telegram client (телефон)        НЕ ДОВЕРЯЕМ: клиентское ПО, network
TB2 — Internet transport (TLS)         НЕ ДОВЕРЯЕМ: MITM (mitigated TLS)
TB3 — Backend HTTP API                  ДОВЕРЯЕМ только после auth
TB4 — Backend worker (торговый движок)  ДОВЕРЯЕМ (внутри)
TB5 — PostgreSQL                        ДОВЕРЯЕМ (внутри, но данные at-rest зашифрованы секретами)
TB6 — Exchange APIs                     НЕ ДОВЕРЯЕМ: внешние системы, rate limits, malice
TB7 — Server OS / filesystem            ДОВЕРЯЕМ (но master key — из env/KMS, не из файла в репо)
```

## 2. Активы (Assets)

- **A1 — API ключи бирж** (trade + withdrawal). Компрометация = потеря средств.
- **A2 — Telegram bot token**. Компрометация = подмена бота.
- **A3 — Master encryption key**. Компрометация = расшифровка всех секретов.
- **A4 — Средства на биржах** (USDT). Косвенно защищены через A1.
- **A5 — Целостность торговой логики** (дельта-нейтральность, risk limits). Нарушение = направленный риск.
- **A6 — Данные пользователей** (адреса вывода, история сделок).

## 3. Угрозы (STRIDE) и митигации

### S — Spoofing (подмена)

| Угроза | Вектор | Митигация |
|---|---|---|
| Подмена Telegram-пользователя | Фронтенд шлёт поддельный user_id | 13.4: валидация `initData` на backend по официальному алгоритму; не доверять user_id из фронта |
| Подмена запроса к API | Перехват сессии | Короткоживущая server-side session; TLS; allowlist Admin IDs |
| Подмена withdrawal-адреса | MITM/compromised client | Address fingerprint; 2FA на изменение адреса (27); whitelist на стороне биржи |
| Подмена биржи | Фишинговый endpoint | COLD `rest_base_url`/`ws_base_url` из config; не из user input |

### T — Tampering (изменение данных)

| Угроза | Вектор | Митигация |
|---|---|---|
| Replay мутации | Повтор старого запроса | 13.5: nonce/timestamp + idempotency key + проверка устаревания |
| Подмена ордера в полёте | bug → дублирующий ордер | `ClientOrderIdScheme` (5.3): детерминированный id; QUERY_THEN_DECIDE при ack timeout |
| Подмена состояния позиции | race/desync | State machine + transactional outbox; REST-recon после WS reconnect (6.3) |
| Подмена risk limits в рантайме | bug в hot-reload | Atomic swap; валидация backend; нельзя установить небезопасное значение без подтверждения |

### R — Repudiation (отказ от действия)

| Угроза | Вектор | Митигация |
|---|---|---|
| Пользователь отрицает действие | нет лога | 1.2.9: каждое действие аудируемо (кто, когда, почему, параметры, результат); immutable audit_log |
| Система «не знает», что сделала | crash без persist | Transactional outbox; state machine в БД; после рестарта — reconciliation |

### I — Information disclosure (утечка)

| Угроза | Вектор | Митигация |
|---|---|---|
| Утечка API secret | в логах/telemetry/сообщениях | 13.3: запрещено логировать secret/passphrase/token/подписи; raw responses redacted |
| Утечка master key | в config.yaml/репо | Только env/secret manager; не в репо |
| Утечка withdrawal address | в логах | Fingerprint в логах; полное значение только в защищённом audit UI |
| Утечка БД | кража диска | Secrets зашифрованы AEAD; backup encrypted at rest |
| Утечка через Mini App | frontend хранит secret | 13.1: Mini App передаёт секрет один раз, backend не возвращает |

### D — Denial of service

| Угроза | Вектор | Митигация |
|---|---|---|
| Перегрузка CPU/RAM | полный стакан всех токенов | Раздел 7: 4 уровня; coalescing; bounded worker pools; GOMEMLIMIT |
| Перегрузка очередей | public data flood | Bounded queues; coalesce public; drop policy с метрикой |
| Потеря private events | переполнение | Private events терять нельзя; bounded + reconciliation при overflow |
| Rate limit от биржи | слишком много запросов | Per-adapter rate limiter + circuit breaker + backoff с jitter |
| Недоступность БД | отказ СУБД | 28: SAFE_HALT; in-memory state; durable-side logging; обязательная сверка |

### E — Elevation of privilege

| Угроза | Вектор | Митигация |
|---|---|---|
| Несанкционированный LIVE режим | user error/bug | 2FA на переключение LIVE/AUTO (27) |
| Trade key с правом withdraw | misconfiguration | 13.2: отдельные ключи; trade key без withdraw; обязательная проверка прав при precheck |
| Авто-rebalance без test barrier | bug | 12.6: двухэтапный барьер; circuit breaker |

## 4. Специфические финансовые угрозы

| Угроза | Описание | Митигация |
|---|---|---|
| **ADL** (раздел 23) | биржа урезает один leg → направленный дисбаланс | Мониторинг ADL queue; экстренное закрытие второго leg; ADLExposureLimit |
| **Funding sign flip** | edge инвертируется после входа | ExitIfFundingSignChanges; FundingUncertaintyReserve |
| **Predicted ≠ realized** | ставка меняется до события | 3.2: ConfidenceLevel + адаптивный резерв; не гарантировать predicted |
| **Joint slippage при шоке** | обе ноги скользят одновременно | 3.3: JointSlippageCapBps; остановка slices |
| **Execution skew** | один leg заполняется, другой нет | 10.2: QUERY_THEN_DECIDE; repair; bounded compensation |
| **Counterparty failure** | биржа теряет ликвидность/solvency | CounterpartyRiskTier; haircut; MaxExposurePerExchange |
| **Blind retry → дублирующий ордер** | таймаут ack → повторная отправка | AckTimeoutBehavior: QUERY_THEN_DECIDE; idempotency |
| **Blind retry → дублирующий withdrawal** | таймаут вывода → повтор | 12.7: никакого blind retry; REBALANCE_LOCKED; ручное решение |
| **Clock skew → неверный timestamp подписи** | рассинхрон часов | 24: NTP; sync перед подписью; stop-trading при превышении |

## 5. Приоритизация митигаций

**P0 (блокер live):** A1/A3 защита секретов, initData валидация, QUERY_THEN_DECIDE, test-transfer barrier, ADL-реакция, reconciliation после рестарта, clock sync.
**P1 (до AUTO режима):** 2FA критичных мутаций, circuit breakers, RiskSnapAfterMaxDailyLoss, counterparty risk активен.
**P2 (до масштабирования):** full observability, chaos tests, RTO/RPO rehearsal.

## 6. Открытые риски (требуют отдельного внимания)

- **R1:** Тестnet-среды не у всех бирж идентичны production (особенно MEXC/Gate/KuCoin). Mitigation: контракт-тесты на fixtures + осторожный live rollout одной пары (раздел 19).
- **R2:** Withdrawal API может быть приостановлен биржей без уведомления. Mitigation: precheck wallet status; circuit breaker; ручное вмешательство.
- **R3:** ADL indicator публикуется не всеми биржами. Mitigation: где нет — консервативный exposure cap по CounterpartyRiskTier.
