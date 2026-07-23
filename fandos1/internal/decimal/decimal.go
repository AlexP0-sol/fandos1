// Package decimal реализует двухконтурную арифметику для финансовой логики
// (ADR-0002, раздел 3.6 промпта v2).
//
// Два контура:
//
//  1. Decimal — thin wrapper над shopspring/decimal.Decimal для риск-контура
//     и торговой логики (ExpectedNetPnL, funding, basis, fees, дельта, лимиты).
//     Безопасен по точности, но аллоцирует — не для горячего контура.
//
//  2. Fixed64 — нормализованное целое с фиксированной scale для горячего контура
//     (нормализация WS market data). Overflow-проверенные операции.
//
// Граница между контурами — единственная точка конверсии ToDecimal/FromDecimal,
// lossless, с ошибкой при потере точности или переполнении.
//
// Принцип 1.2.1: float64 запрещён в финансовой логике.
package decimal

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	sp "github.com/shopspring/decimal"
)

// init фиксирует точность деления shopspring для всего процесса.
// Значение по умолчанию (16 знаков) может незаметно обрезать хвост при цепочках
// делений; 28 знаков — консервативный запас для риск-контура (ADR-0002).
// Все округления в бизнес-логике должны выполняться ЯВНО (Round/Truncate/Quantize).
func init() {
	sp.DivisionPrecision = 28
}

// ============================================================
// Decimal — риск-контур
// ============================================================

// Decimal — точное десятичное число для риск-контура и торговой логики.
type Decimal struct {
	v sp.Decimal
}

// MustNew создаёт Decimal из целого коэффициента и степени (mantissa × 10^exp).
// Паникует при некорректном вводе — только для литералов/констант.
func MustNew(value int64, exp int32) Decimal {
	d, err := New(value, exp)
	if err != nil {
		panic(err)
	}
	return d
}

// New создаёт Decimal из целого коэффициента и степени (mantissa × 10^exp).
// Пример: New(123, -2) == 1.23; New(5, 0) == 5.
func New(value int64, exp int32) (Decimal, error) {
	return Decimal{v: sp.New(value, exp)}, nil
}

// FromString парсит строку. Возвращает ошибку при некорректном формате.
// Запрещает использование float-литералов с потерей точности — только десятичная строка.
func FromString(s string) (Decimal, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Decimal{}, fmt.Errorf("decimal: empty string")
	}
	v, err := sp.NewFromString(s)
	if err != nil {
		return Decimal{}, fmt.Errorf("decimal: parse %q: %w", s, err)
	}
	return Decimal{v: v}, nil
}

// MustFromString — FromString, паникующая при ошибке (только для известных констант).
func MustFromString(s string) Decimal {
	d, err := FromString(s)
	if err != nil {
		panic(err)
	}
	return d
}

// FromInt создаёт Decimal из int64.
func FromInt(i int64) Decimal {
	return Decimal{v: sp.New(i, 0)}
}

// Zero — нулевое значение.
var Zero = Decimal{v: sp.New(0, 0)}

// One — единица.
var One = Decimal{v: sp.New(1, 0)}

// IsZero — true, если значение равно нулю.
func (d Decimal) IsZero() bool { return d.v.IsZero() }

// IsPositive — true, если значение строго больше нуля.
func (d Decimal) IsPositive() bool { return d.v.IsPositive() }

// IsNegative — true, если значение строго меньше нуля.
func (d Decimal) IsNegative() bool { return d.v.IsNegative() }

// Sign возвращает -1, 0 или 1.
func (d Decimal) Sign() int { return d.v.Sign() }

// Add возвращает d + o без изменения операндов.
func (d Decimal) Add(o Decimal) Decimal { return Decimal{v: d.v.Add(o.v)} }

// Sub возвращает d - o.
func (d Decimal) Sub(o Decimal) Decimal { return Decimal{v: d.v.Sub(o.v)} }

// Neg возвращает -d.
func (d Decimal) Neg() Decimal { return Decimal{v: d.v.Neg()} }

// Abs возвращает |d|.
func (d Decimal) Abs() Decimal { return Decimal{v: d.v.Abs()} }

// Mul возвращает d × o.
func (d Decimal) Mul(o Decimal) Decimal { return Decimal{v: d.v.Mul(o.v)} }

// Div возвращает d / o. Паникует при делении на ноль (контракт вызова).
func (d Decimal) Div(o Decimal) Decimal {
	if o.IsZero() {
		panic("decimal: division by zero")
	}
	return Decimal{v: d.v.Div(o.v)}
}

// Cmp сравнивает d и o: -1, 0, 1.
func (d Decimal) Cmp(o Decimal) int { return d.v.Cmp(o.v) }

// Equal — true, если d == o.
func (d Decimal) Equal(o Decimal) bool { return d.v.Equal(o.v) }

// GreaterThan — d > o.
func (d Decimal) GreaterThan(o Decimal) bool { return d.v.GreaterThan(o.v) }

// GreaterThanOrEqual — d >= o.
func (d Decimal) GreaterThanOrEqual(o Decimal) bool { return d.v.GreaterThanOrEqual(o.v) }

// LessThan — d < o.
func (d Decimal) LessThan(o Decimal) bool { return d.v.LessThan(o.v) }

// LessThanOrEqual — d <= o.
func (d Decimal) LessThanOrEqual(o Decimal) bool { return d.v.LessThanOrEqual(o.v) }

// MulInt умножает на целое (частый случай: qty × price, qty × leverage).
func (d Decimal) MulInt(i int64) Decimal { return Decimal{v: d.v.Mul(sp.New(i, 0))} }

// Round округляет до precision знаков банковским округлением (round half to even):
// 0.125 → 0.12, 0.135 → 0.14. Детерминированно и без систематического смещения —
// подходит для отчётных величин (PnL, fees). Для объёмов ордеров использовать
// Quantize/Truncate (округление вниз, раздел 9.2).
func (d Decimal) Round(precision int32) Decimal {
	return Decimal{v: d.v.RoundBank(precision)}
}

// Truncate обрезает до precision знаков без округления (к нулю).
func (d Decimal) Truncate(precision int32) Decimal {
	return Decimal{v: d.v.Truncate(precision)}
}

// Quantize приводит к шагу step: floor(d / step) × step, возвращает также остаток.
// Используется для округления объёма в большую безопасную сторону (раздел 9.2):
// никогда не превышать разрешённый объём → round down (Floor).
func (d Decimal) Quantize(step Decimal) (quantized Decimal, residue Decimal) {
	if step.IsZero() {
		panic("decimal: quantize step is zero")
	}
	// floor(d / step) × step. Floor округляет к -∞; для положительных d это round-down.
	q := d.v.Div(step.v).Floor()
	quant := q.Mul(step.v)
	return Decimal{v: quant}, Decimal{v: d.v.Sub(quant)}
}

// String — каноничное строковое представление.
func (d Decimal) String() string { return d.v.String() }

// StringFixed — строка с фиксированным числом знаков.
func (d Decimal) StringFixed(places int32) string { return d.v.StringFixed(places) }

// Float64Lossy — ТОЛЬКО для observability/логов/метрик, никогда для финансового расчёта.
// Помечено Lossy, чтобы случайно не использовать в risk/execution.
func (d Decimal) Float64Lossy() float64 {
	f, _ := d.v.Float64()
	return f
}

// Underlying возвращает внутренний shopspring.Decimal для редких случаев,
// когда нужна полная функциональность. Не злоупотреблять.
func (d Decimal) Underlying() sp.Decimal { return d.v }

// Sum складывает слайс; возвращает Zero для пустого.
func Sum(items ...Decimal) Decimal {
	out := sp.New(0, 0)
	for _, it := range items {
		out = out.Add(it.v)
	}
	return Decimal{v: out}
}

// Min возвращает меньшее из двух.
func Min(a, b Decimal) Decimal {
	if a.LessThan(b) {
		return a
	}
	return b
}

// Max возвращает большее из двух.
func Max(a, b Decimal) Decimal {
	if a.GreaterThan(b) {
		return a
	}
	return b
}

// ============================================================
// Fixed64 — горячий контур (нормализация WS market data)
// ============================================================

// Fixed64 — нормализованное знаковое целое с фиксированной scale.
// Значение = unscaled / 10^scale. Overflow-проверенные арифметические операции.
// Используется в горячем контуре нормализации market data, где важна скорость
// и отсутствие аллокаций (раздел 7.3, ADR-0002).
type Fixed64 struct {
	unscaled int64
	scale    int8 // всегда в [0,18]
}

// errOverflow — ошибка переполнения арифметики Fixed64.
var errOverflow = fmt.Errorf("fixed64: overflow")

// NewFixed создаёт Fixed64 из unscaled-значения и scale.
// Scale должен быть в [0,18].
func NewFixed(unscaled int64, scale int8) (Fixed64, error) {
	if scale < 0 || scale > 18 {
		return Fixed64{}, fmt.Errorf("fixed64: scale %d out of range [0,18]", scale)
	}
	return Fixed64{unscaled: unscaled, scale: scale}, nil
}

// MustFixed — NewFixed, паникующий при ошибке (только для констант в коде).
func MustFixed(unscaled int64, scale int8) Fixed64 {
	f, err := NewFixed(unscaled, scale)
	if err != nil {
		panic(err)
	}
	return f
}

// ZeroFixed — нулевое значение со scale=8 (типичная цена/qty scale).
var ZeroFixed = MustFixed(0, 8)

// IsZero — true, если unscaled == 0.
func (f Fixed64) IsZero() bool { return f.unscaled == 0 }

// Scale возвращает масштаб.
func (f Fixed64) Scale() int8 { return f.scale }

// Unscaled возвращает немасштабированное значение (для сериализации/метрик).
func (f Fixed64) Unscaled() int64 { return f.unscaled }

// addUnscaled выполняет сложение с проверкой переполнения.
func addUnscaled(a, b int64) (int64, error) {
	if b > 0 && a > math.MaxInt64-b {
		return 0, errOverflow
	}
	if b < 0 && a < math.MinInt64-b {
		return 0, errOverflow
	}
	return a + b, nil
}

// mulUnscaled выполняет умножение с проверкой переполнения.
func mulUnscaled(a, b int64) (int64, error) {
	if a == 0 || b == 0 {
		return 0, nil
	}
	// MinInt64 × -1 переполняется, а MinInt64 / -1 паникует в Go — отсекаем явно.
	if (a == math.MinInt64 && b == -1) || (b == math.MinInt64 && a == -1) {
		return 0, errOverflow
	}
	r := a * b
	if r/b != a {
		return 0, errOverflow
	}
	return r, nil
}

// AddWith проверяет совпадение scale и складывает.
func (f Fixed64) AddWith(o Fixed64) (Fixed64, error) {
	if f.scale != o.scale {
		return Fixed64{}, fmt.Errorf("fixed64: scale mismatch %d vs %d", f.scale, o.scale)
	}
	sum, err := addUnscaled(f.unscaled, o.unscaled)
	if err != nil {
		return Fixed64{}, err
	}
	return Fixed64{unscaled: sum, scale: f.scale}, nil
}

// SubWith — вычитание с проверкой scale и переполнения.
func (f Fixed64) SubWith(o Fixed64) (Fixed64, error) {
	if f.scale != o.scale {
		return Fixed64{}, fmt.Errorf("fixed64: scale mismatch %d vs %d", f.scale, o.scale)
	}
	diff, err := addUnscaled(f.unscaled, -o.unscaled)
	if err != nil {
		return Fixed64{}, err
	}
	return Fixed64{unscaled: diff, scale: f.scale}, nil
}

// Neg — унарный минус с проверкой переполнения.
func (f Fixed64) Neg() (Fixed64, error) {
	if f.unscaled == math.MinInt64 {
		return Fixed64{}, errOverflow
	}
	return Fixed64{unscaled: -f.unscaled, scale: f.scale}, nil
}

// MulInt — умножение на целое без scale-конверсии (b считает «целым» при scale=0).
// Используется, когда множитель — натуральное число (напр. количество уровней стакана).
func (f Fixed64) MulInt(m int64) (Fixed64, error) {
	p, err := mulUnscaled(f.unscaled, m)
	if err != nil {
		return Fixed64{}, err
	}
	return Fixed64{unscaled: p, scale: f.scale}, nil
}

// ============================================================
// Конверсия между контурами (единственная точка, lossless)
// ============================================================

// ToDecimal конвертирует Fixed64 → Decimal lossless.
// Fixed64 — точное целое со scale, поэтому конверсия всегда lossless.
// ToDecimal конвертирует Fixed64 → Decimal lossless.
// Контракт Fixed64: значение = unscaled / 10^scale, поэтому экспонента отрицательная.
// sp.New(value, exp) = value × 10^exp, значит для value=unscaled нужна степень -scale.
func (f Fixed64) ToDecimal() Decimal {
	return Decimal{v: sp.New(f.unscaled, -int32(f.scale))}
}

// FromDecimal конвертирует Decimal → Fixed64 с заданной scale.
// Возвращает ошибку, если:
//   - значение не представимо с данной scale без потери точности,
//   - unscaled-значение вне диапазона int64.
//
// Для горячего контура scale фиксируется (напр. 8 для цены/количества).
func FromDecimal(d Decimal, scale int8) (Fixed64, error) {
	if scale < 0 || scale > 18 {
		return Fixed64{}, fmt.Errorf("fixed64: scale %d out of range", scale)
	}
	// Приводим к нужной scale через Truncate. Если после truncate значение
	// изменилось — конверсия lossy.
	trunc := d.v.Truncate(int32(scale))
	if !trunc.Equal(d.v) {
		return Fixed64{}, fmt.Errorf("fixed64: lossy conversion %s at scale %d", d.String(), scale)
	}
	// Извлекаем unscaled: value × 10^scale как точное целое.
	// sp.New(1, exp) = 1 × 10^exp, поэтому для умножения на 10^scale нужен +scale.
	unscaled := trunc.Mul(sp.New(1, int32(scale))) // value × 10^scale
	if !unscaled.IsInteger() {
		return Fixed64{}, fmt.Errorf("fixed64: cannot represent %s as int64 at scale %d", d.String(), scale)
	}
	// IntPart() возвращает усечённое к нулю целое (ok, т.к. IsInteger == true).
	unscaledInt := unscaled.IntPart()
	// Проверяем переполнение: IntPart возвращает int64 с truncation при overflow,
	// но поскольку мы умножили на 10^scale, проверяем через обратное преобразование.
	verifier := sp.New(unscaledInt, 0)
	if !verifier.Equal(unscaled) {
		return Fixed64{}, errOverflow
	}
	return Fixed64{unscaled: unscaledInt, scale: scale}, nil
}

// String — каноничная строка Fixed64 (для сериализации в БД/логи).
func (f Fixed64) String() string {
	if f.scale == 0 {
		return strconv.FormatInt(f.unscaled, 10)
	}
	neg := f.unscaled < 0
	var u uint64
	if neg {
		// Через uint64: -MinInt64 не представим в int64.
		u = uint64(-(f.unscaled + 1)) + 1
	} else {
		u = uint64(f.unscaled)
	}
	str := strconv.FormatUint(u, 10)
	// Дополняем нулями слева до scale+1.
	for int8(len(str)) <= f.scale {
		str = "0" + str
	}
	pos := int8(len(str)) - f.scale
	intPart := str[:pos]
	fracPart := str[pos:]
	out := intPart + "." + fracPart
	if neg {
		out = "-" + out
	}
	return out
}
