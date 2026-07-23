package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/thecd/fundarbitrage/internal/domain"
)

// OrderRepo — репозиторий ордеров, заявок и funding payments.
type OrderRepo struct {
	pool *pgxpool.Pool
}

// NewOrderRepo создаёт OrderRepo.
func NewOrderRepo(pool *pgxpool.Pool) *OrderRepo {
	return &OrderRepo{pool: pool}
}

// UpsertOrder идемпотентно сохраняет ордер. Уникальность — (exchange, client_order_id).
// При повторной вставке обновляет изменяемые поля (status, filled_qty, avg_fill_price, fees,
// exchange_order_id, ack_state, exchange_ts).
func (r *OrderRepo) UpsertOrder(
	ctx context.Context,
	o domain.Order,
	exchange domain.ExchangeID,
	positionID, legID string,
) error {
	var price *string
	if !o.AvgFillPrice.IsZero() {
		s := decimalToNumeric(o.AvgFillPrice)
		price = &s
	}
	var exOrderID *string
	if o.ExchangeOrderID != "" {
		exOrderID = &o.ExchangeOrderID
	}
	var posID *string
	if positionID != "" {
		posID = &positionID
	}
	var lID *string
	if legID != "" {
		lID = &legID
	}

	var exchangeTS *time.Time
	if !o.ExchangeTimestamp.IsZero() {
		ts := o.ExchangeTimestamp
		exchangeTS = &ts
	}

	timeInForce := ""
	// domain.Order не содержит TimeInForce напрямую; оставляем пустым
	// (можно расширить позже).

	_, err := r.pool.Exec(ctx, `
		INSERT INTO orders (
			exchange, exchange_order_id, client_order_id,
			position_id, leg_id,
			side, reduce_only, order_mode, time_in_force,
			requested_qty, price, filled_qty, avg_fill_price, fees,
			status, ack_state, exchange_ts
		) VALUES (
			$1, $2, $3,
			$4, $5,
			$6, $7, $8, $9,
			$10, $11, $12, $13, $14,
			$15, $16, $17
		)
		ON CONFLICT (exchange, client_order_id) DO UPDATE SET
			exchange_order_id = COALESCE(EXCLUDED.exchange_order_id, orders.exchange_order_id),
			status            = EXCLUDED.status,
			filled_qty        = EXCLUDED.filled_qty,
			avg_fill_price    = EXCLUDED.avg_fill_price,
			fees              = EXCLUDED.fees,
			ack_state         = EXCLUDED.ack_state,
			exchange_ts       = EXCLUDED.exchange_ts,
			updated_at        = now()
	`,
		string(exchange),
		exOrderID,
		string(o.ClientOrderID),
		posID,
		lID,
		string(o.Side),
		o.ReduceOnly,
		string(o.OrderMode),
		nullableString(timeInForce),
		decimalToNumeric(o.RequestedQty),
		price,
		decimalToNumeric(o.FilledQty),
		price, // avg_fill_price — используем то же поле
		decimalToNumeric(o.Fees),
		string(o.Status),
		string(o.AckState),
		exchangeTS,
	)
	if err != nil {
		return fmt.Errorf("repository: upsert ордера %s: %w", o.ClientOrderID, err)
	}
	return nil
}

// FillParams — параметры для InsertFill.
type FillParams struct {
	Exchange        domain.ExchangeID
	ExchangeFillID  string
	ExchangeOrderID string
	ClientOrderID   domain.ClientOrderID
	PositionID      string
	LegID           string
	Side            domain.Side
	BaseQty         string // NUMERIC строка
	Price           string // NUMERIC строка
	Fee             string // NUMERIC строка
	FeeAsset        string
	IsMaker         bool
	ExchangeTS      time.Time
}

// InsertFill идемпотентно вставляет fill. Уникальность — (exchange, exchange_fill_id).
// При конфликте ничего не делает (ON CONFLICT DO NOTHING).
func (r *OrderRepo) InsertFill(ctx context.Context, fp FillParams) error {
	var exFillID *string
	if fp.ExchangeFillID != "" {
		exFillID = &fp.ExchangeFillID
	}
	var exOrderID *string
	if fp.ExchangeOrderID != "" {
		exOrderID = &fp.ExchangeOrderID
	}
	var posID *string
	if fp.PositionID != "" {
		posID = &fp.PositionID
	}
	var lID *string
	if fp.LegID != "" {
		lID = &fp.LegID
	}
	var feeAsset *string
	if fp.FeeAsset != "" {
		feeAsset = &fp.FeeAsset
	}

	_, err := r.pool.Exec(ctx, `
		INSERT INTO fills (
			exchange, exchange_fill_id, exchange_order_id,
			client_order_id, position_id, leg_id,
			side, base_qty, price, fee, fee_asset,
			is_maker, exchange_ts
		) VALUES (
			$1, $2, $3,
			$4, $5, $6,
			$7, $8, $9, $10, $11,
			$12, $13
		)
		ON CONFLICT (exchange, exchange_fill_id) DO NOTHING
	`,
		string(fp.Exchange),
		exFillID,
		exOrderID,
		string(fp.ClientOrderID),
		posID,
		lID,
		string(fp.Side),
		fp.BaseQty,
		fp.Price,
		fp.Fee,
		feeAsset,
		fp.IsMaker,
		fp.ExchangeTS,
	)
	if err != nil {
		return fmt.Errorf("repository: вставка fill %s/%s: %w", fp.Exchange, fp.ExchangeFillID, err)
	}
	return nil
}

// FundingPaymentParams — параметры для InsertFundingPayment.
type FundingPaymentParams struct {
	Exchange       domain.ExchangeID
	ExchangeSymbol string
	PositionID     string
	LegID          string
	Amount         string // NUMERIC строка, знаковая
	Rate           string // NUMERIC строка
	Notional       *string
	FundingTime    time.Time
}

// InsertFundingPayment идемпотентно вставляет funding payment.
// Уникальность: (exchange, exchange_symbol, funding_time, position_id).
//
// Особенность PostgreSQL: NULL != NULL в UNIQUE constraints, поэтому ON CONFLICT
// не срабатывает при NULL position_id. В этом случае применяется явная проверка
// EXISTS перед вставкой через INSERT ... WHERE NOT EXISTS.
func (r *OrderRepo) InsertFundingPayment(ctx context.Context, fp FundingPaymentParams) error {
	var posID *string
	if fp.PositionID != "" {
		posID = &fp.PositionID
	}
	var lID *string
	if fp.LegID != "" {
		lID = &fp.LegID
	}

	var err error
	if posID != nil {
		// Когда position_id задан — ON CONFLICT работает штатно.
		_, err = r.pool.Exec(ctx, `
			INSERT INTO funding_payments (
				exchange, exchange_symbol,
				position_id, leg_id,
				amount, rate, notional, funding_time
			) VALUES (
				$1, $2,
				$3, $4,
				$5, $6, $7, $8
			)
			ON CONFLICT (exchange, exchange_symbol, funding_time, position_id) DO NOTHING
		`,
			string(fp.Exchange),
			fp.ExchangeSymbol,
			posID,
			lID,
			fp.Amount,
			fp.Rate,
			fp.Notional,
			fp.FundingTime,
		)
	} else {
		// Когда position_id = NULL — ON CONFLICT не работает (NULL != NULL).
		// Используем INSERT ... WHERE NOT EXISTS для идемпотентности.
		// Не передаём posID как параметр — явно используем NULL в SQL.
		_, err = r.pool.Exec(ctx, `
			INSERT INTO funding_payments (
				exchange, exchange_symbol,
				position_id, leg_id,
				amount, rate, notional, funding_time
			)
			SELECT $1, $2, NULL, $3, $4, $5, $6, $7
			WHERE NOT EXISTS (
				SELECT 1 FROM funding_payments
				WHERE exchange        = $1
				  AND exchange_symbol = $2
				  AND funding_time    = $7
				  AND position_id IS NULL
			)
		`,
			string(fp.Exchange),
			fp.ExchangeSymbol,
			lID,
			fp.Amount,
			fp.Rate,
			fp.Notional,
			fp.FundingTime,
		)
	}
	if err != nil {
		return fmt.Errorf("repository: вставка funding payment %s/%s@%s: %w",
			fp.Exchange, fp.ExchangeSymbol, fp.FundingTime.Format(time.RFC3339), err)
	}
	return nil
}

// nullableString возвращает *string или nil при пустой строке.
func nullableString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
