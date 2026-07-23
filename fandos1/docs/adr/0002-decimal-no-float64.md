# ADR-0002: Запрет float64, унификация decimal-типов

- **Статус:** Accepted
- **Дата:** 2026-07-16
- **Связанные разделы промпта:** 1.2 (принцип 1), 3.6 (v2)

## Контекст

Принцип 1.2.1 категорически запрещает `float64` для денег, цены, количества, комиссии, PnL, funding rate, округлений и лимитов. IEEE-754 double precision теряет точность при операциях с деньгами (например, `0.1 + 0.2 != 0.3`), что в торговой системе недопустимо: накопленные ошибки приводят к дельта-рассогласованию, неверному расчёту PnL и funding.

Однако требование производительности для горячего контура (миллионы WS-сообщений в секунду по разделу 7) делает чистый `big.Int`/decimal слишком медленным на hot path.

## Рассмотренные варианты

1. **Везде `shopspring/decimal`** — безопасно, но медленно на hot path нормализации миллионов сообщений.
2. **Везде `float64`** — запрещено принципом 1.2.1; неприемлемо.
3. **Двухконтурная модель: `int64` (горячий контур) + `decimal` (риск/торговый контур).** ← выбрано

## Решение

Два контура с явной границей (раздел 3.6):

```text
Риск-контур и торговая логика:
  shopspring/decimal — все расчёты ExpectedNetPnL, funding, basis, fees, дельты, лимитов.
Горячий контур (нормализация WS market data, агрегация snapshot):
  нормализованный int64 с фиксированной scale (price scale = 8, qty scale = 8)
  с контролем переполнения (overflow-checked arithmetic).
Граница между контурами — одно место: lossless conversion int64↔decimal
  с проверкой и логированием потерь точности.
Запрещено неявное decimal→float64 и обратно в финансовой логике.
```

## Реализация в проекте

- Пакет `internal/decimal`:
  - `Decimal` — thin wrapper/alias над `shopspring/decimal.Decimal` для риск-контура.
  - `Fixed64` (или `Int64Scaled`) — нормализованное целое для горячего контура с явной `Scale` и overflow-проверенными операциями.
  - `Convert(Fixed64) → Decimal` / `Convert(Decimal) → Fixed64` — единственная точка конверсии, lossless, с ошибкой при потере точности.
- lint-правило / review-чек: поиск `float64` в `internal/risk`, `internal/execution`, `internal/scanner`, `internal/strategy`, `internal/portfolio`, `internal/rebalance`, `internal/decimal`.

## Последствия

- ✅ Соответствие принципу 1.2.1 (нет float64 в финансах).
- ✅ Производительность горячего контура сохранена (int64 без аллокаций).
- ⚠️ Дисциплина: разработчики не должны использовать `decimal` в hot path нормализации (аллокации) и не должны использовать raw `int64` в риск-расчётах без overflow-проверок.
- ⚠️ Conversion-точка — потенциальный источник тонких багов; требует property-тестов (round-trip int64→decimal→int64 сохраняет значение).

## Критерий приёмки

- `grep -r "float64" internal/{risk,execution,scanner,strategy,portfolio,rebalance,decimal}` → 0 совпадений (кроме, возможно, метрик/observability, не влияющих на деньги).
- Property-тест: round-trip conversion int64↔decimal lossless для всех значений в диапазоне инструментов.
- Overflow-тесты для Fixed64.
