package portfolio

import (
	"time"

	"github.com/thecd/fundarbitrage/internal/decimal"
	"github.com/thecd/fundarbitrage/internal/domain"
)

// RestoreArgs содержит все поля, необходимые для восстановления позиции из персистентного хранилища.
// Используется исключительно слоем repository при загрузке из БД.
// Вызывающий (repository) несёт ответственность за корректность всех полей.
type RestoreArgs struct {
	ID            domain.PositionID
	Asset         domain.AssetSymbol
	LongExchange  domain.ExchangeID
	ShortExchange domain.ExchangeID

	State         State
	TargetBaseQty decimal.Decimal
	// ActualDelta хранится как actual_delta в БД.
	// При восстановлении устанавливается в LongBaseQty; ShortBaseQty — в Zero,
	// так как БД хранит только суммарную дельту, а не отдельные ноги.
	ActualDelta decimal.Decimal

	RealisedPnL decimal.Decimal
	FundingPnL  decimal.Decimal
	FeesPaid    decimal.Decimal

	EntryReason string
	ExitReason  string
	ExitedAt    *time.Time

	CreatedAt time.Time
	UpdatedAt time.Time
}

// RestorePosition восстанавливает Position из персистентного хранилища.
// Этот конструктор намеренно обходит валидацию переходов state machine —
// данные уже были валидированы при первичном сохранении.
// Только repository должен вызывать эту функцию.
func RestorePosition(args RestoreArgs) *Position {
	return &Position{
		ID:            args.ID,
		Asset:         args.Asset,
		LongExchange:  args.LongExchange,
		ShortExchange: args.ShortExchange,
		State:         args.State,
		TargetBaseQty: args.TargetBaseQty,
		// actual_delta из БД — это LongBaseQty - ShortBaseQty.
		// При восстановлении используем дельту как LongBaseQty, ShortBaseQty=0.
		// Точное разделение по ногам должно быть восстановлено из position_legs.
		LongBaseQty:  args.ActualDelta,
		ShortBaseQty: decimal.Zero,
		RealisedPnL:  args.RealisedPnL,
		FundingPnL:   args.FundingPnL,
		FeesPaid:     args.FeesPaid,
		EntryReason:  args.EntryReason,
		ExitReason:   args.ExitReason,
		ExitedAt:     args.ExitedAt,
		CreatedAt:    args.CreatedAt,
		UpdatedAt:    args.UpdatedAt,
	}
}
