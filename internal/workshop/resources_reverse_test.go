package workshop_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// A replanning that is regretted is put back by a new event: the move gives the
// block its old time, the end gives back a block that had not begun, and the
// trail keeps both. Another booking in the way stops it, naming what has it.
func TestAReplanningRegrettedIsReversedByANewEventAndTheTrailKeepsBoth(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	boss := everything(owner())
	res := trailer(t, pool)
	moved, err := rent(pool, rentalDesk(), res, in2031(10, 9), in2031(10, 17))
	if err != nil {
		t.Fatal(err)
	}
	if err := workshop.OverrideBlock(ctx, pool, boss, moved, in2031(12, 9), in2031(12, 17), "Kunden bad om en annan dag"); err != nil {
		t.Fatal(err)
	}
	// Somebody else takes the old time meanwhile, so putting it back is refused.
	other, err := rent(pool, rentalDesk(), res, in2031(10, 9), in2031(10, 17))
	if err != nil {
		t.Fatal(err)
	}
	if err := workshop.ReverseOverrideBlock(ctx, pool, boss, moved, "Det var ett misstag"); !errors.Is(err, workshop.ErrInvalid) {
		t.Fatalf("put back onto a time somebody has taken: %v", err)
	}
	if err := workshop.ReleaseBlock(ctx, pool, rentalDesk(), other); err != nil {
		t.Fatal(err)
	}
	if err := workshop.ReverseOverrideBlock(ctx, pool, rentalDesk(), moved, "Det var ett misstag"); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("the rental desk put a replanning back: %v", err)
	}
	if err := workshop.ReverseOverrideBlock(ctx, pool, boss, moved, " "); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("put back with no reason: %v", err)
	}
	if err := workshop.ReverseOverrideBlock(ctx, pool, boss, moved, "Det var ett misstag"); err != nil {
		t.Fatal(err)
	}
	var from time.Time
	if err := pool.QueryRow(ctx, `SELECT lower(period) FROM resource_blocks WHERE id = $1`, moved).Scan(&from); err != nil || !from.Equal(in2031(10, 9)) {
		t.Errorf("the block starts %v after being put back, %v", from, err)
	}
	if err := workshop.ReverseOverrideBlock(ctx, pool, boss, moved, "Igen"); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("put back twice: %v", err)
	}
	var events int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE subject_id = $1 AND event IN ('block.overridden', 'block.override_reversed')`, moved).Scan(&events); err != nil || events != 2 {
		t.Errorf("the trail has %d events, %v", events, err)
	}

	// An end of a block not yet begun is put back as it stood; one that had begun
	// stays as it was.
	ended, err := rent(pool, rentalDesk(), res, in2031(20, 9), in2031(20, 17))
	if err != nil {
		t.Fatal(err)
	}
	if err := workshop.OverrideBlock(ctx, pool, boss, ended, time.Time{}, time.Time{}, "Bilen behövs"); err != nil {
		t.Fatal(err)
	}
	if err := workshop.ReverseOverrideBlock(ctx, pool, boss, ended, "Den behövdes inte"); err != nil {
		t.Fatal(err)
	}
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM resource_blocks WHERE id = $1`, ended).Scan(&status); err != nil || status != workshop.BlockConfirmed {
		t.Errorf("an ended block put back is %q, %v", status, err)
	}
	begun, err := workshop.BookResource(ctx, pool, rentalDesk(), workshop.ResourceBooking{
		ResourceID: res, Kind: workshop.BlockRental, From: time.Now().Add(-2 * time.Hour), To: time.Now().Add(2 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if err := workshop.OverrideBlock(ctx, pool, boss, begun, time.Time{}, time.Time{}, "Bilen behövs nu"); err != nil {
		t.Fatal(err)
	}
	if err := workshop.ReverseOverrideBlock(ctx, pool, boss, begun, "Nej"); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a block that had begun was put back: %v", err)
	}
	if err := workshop.ReverseOverrideBlock(ctx, pool, boss, other, "Nej"); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a block never replanned was put back: %v", err)
	}
}
