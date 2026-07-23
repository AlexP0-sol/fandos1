package repository_test

import (
	"context"
	"testing"
	"time"

	"github.com/thecd/fundarbitrage/internal/domain"
	"github.com/thecd/fundarbitrage/internal/portfolio"
	"github.com/thecd/fundarbitrage/internal/repository"
)

// TestLegs_UpsertLoadRoundTrip — ноги сохраняются и читаются с точным сплитом
// (регрессия: раньше в БД жила только нетто-дельта, short-объём терялся при рестарте).
func TestLegs_UpsertLoadRoundTrip(t *testing.T) {
	if testPool == nil {
		t.Skip("no postgres")
	}
	ctx := context.Background()
	repo := repository.NewLegsRepo(testPool)

	posID := "test-legs-" + uid()
	// position_legs.position_id → FK на positions: создаём родителя через Persister.
	pos := makePosition(posID, "BTC")
	persister := repository.NewPersister(testPool)
	if err := pos.TransitionTo(portfolio.StateQualified, time.Now().UTC(), "seed", "test", persister); err != nil {
		t.Fatalf("seed position: %v", err)
	}
	defer func() {
		_, _ = testPool.Exec(ctx, `DELETE FROM position_legs WHERE position_id=$1`, posID)
		cleanupPosition(t, posID)
	}()

	long := mustDec("0.037")
	short := mustDec("0.037")
	if err := repo.Upsert(ctx, repository.LegState{
		PositionID: domain.PositionID(posID), Side: domain.SideLong, Exchange: domain.ExchangeBinance,
		Symbol: "BTCUSDT", BaseQty: long, EntryVWAP: mustDec("50010"), Status: "open",
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.Upsert(ctx, repository.LegState{
		PositionID: domain.PositionID(posID), Side: domain.SideShort, Exchange: domain.ExchangeGate,
		Symbol: "BTC_USDT", BaseQty: short, EntryVWAP: mustDec("50040"), Status: "open",
	}); err != nil {
		t.Fatal(err)
	}

	got, err := repo.Load(ctx, domain.PositionID(posID))
	if err != nil {
		t.Fatal(err)
	}
	if !got.Found {
		t.Fatal("legs not found")
	}
	if !got.LongBaseQty.Equal(long) || !got.ShortBaseQty.Equal(short) {
		t.Errorf("legs = long %s / short %s, want %s / %s",
			got.LongBaseQty, got.ShortBaseQty, long, short)
	}

	// Ротация объёма (repair/close): повторный upsert обновляет, не дублирует.
	newShort := mustDec("0.020")
	if err := repo.Upsert(ctx, repository.LegState{
		PositionID: domain.PositionID(posID), Side: domain.SideShort, Exchange: domain.ExchangeGate,
		Symbol: "BTC_USDT", BaseQty: newShort, Status: "degraded",
	}); err != nil {
		t.Fatal(err)
	}
	got2, err := repo.Load(ctx, domain.PositionID(posID))
	if err != nil {
		t.Fatal(err)
	}
	if !got2.ShortBaseQty.Equal(newShort) {
		t.Errorf("short after update = %s, want %s", got2.ShortBaseQty, newShort)
	}
	if !got2.LongBaseQty.Equal(long) {
		t.Errorf("long must be unchanged = %s, want %s", got2.LongBaseQty, long)
	}
}

// TestLegs_LoadEmpty — позиция без ног → Found=false, нули.
func TestLegs_LoadEmpty(t *testing.T) {
	if testPool == nil {
		t.Skip("no postgres")
	}
	repo := repository.NewLegsRepo(testPool)
	got, err := repo.Load(context.Background(), domain.PositionID("test-legs-none-"+uid()))
	if err != nil {
		t.Fatal(err)
	}
	if got.Found {
		t.Error("expected Found=false for position without legs")
	}
}
