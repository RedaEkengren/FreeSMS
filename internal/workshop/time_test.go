package workshop_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/RedaEkengren/RedaSMS/internal/access"
	"github.com/RedaEkengren/RedaSMS/internal/database"
	"github.com/RedaEkengren/RedaSMS/internal/workshop"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// backdate moves a running clock into the past, which is what a technician who
// forgot to stop it has done by the next morning.
func backdate(t *testing.T, pool *pgxpool.Pool, hours int) {
	t.Helper()
	err := database.InShop(context.Background(), pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`UPDATE time_entries SET started_at = now() - make_interval(hours => $1) WHERE ended_at IS NULL`,
			hours)
		return err
	})
	if err != nil {
		t.Fatalf("backdate: %v", err)
	}
}

// The overnight clock-in. Closing it silently hides a payroll dispute rather
// than settling one; refusing to close it leaves the technician unable to
// start the next car.
func TestAForgottenClockIsClosedAndFlagged(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	ctx := context.Background()

	first := working(t, pool)
	backdate(t, pool, 16)

	// The technician arrives and starts the next car.
	second := newJob2(t, pool)
	move(t, pool, second, workshop.StateEstimated, workshop.StateAwaitingApproval, workshop.StateApproved)
	if err := workshop.ClockIn(ctx, pool, technician(), second); err != nil {
		t.Fatalf("ClockIn: %v", err)
	}

	entries, err := workshop.TimeFor(ctx, pool, advisor(), first)
	if err != nil {
		t.Fatalf("TimeFor: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	e := entries[0]
	if e.Running() {
		t.Error("the old clock is still running")
	}
	if !e.Flagged {
		t.Error("a sixteen-hour entry was closed without being flagged")
	}
	if e.Note == "" {
		t.Error("the entry does not say why it was closed")
	}
	if !e.Implausible() {
		t.Errorf("%s hours does not read as implausible", e.Hours())
	}
}

// A normal stretch is closed without fuss.
func TestANormalClockIsNotFlagged(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	ctx := context.Background()

	first := working(t, pool)
	backdate(t, pool, 2)

	second := newJob2(t, pool)
	move(t, pool, second, workshop.StateEstimated, workshop.StateAwaitingApproval, workshop.StateApproved)
	if err := workshop.ClockIn(ctx, pool, technician(), second); err != nil {
		t.Fatalf("ClockIn: %v", err)
	}

	entries, _ := workshop.TimeFor(ctx, pool, advisor(), first)
	if entries[0].Flagged {
		t.Error("a two-hour entry was flagged")
	}
}

// Flagged entries are the front desk's list, because the point of a
// correction is that a second person agreed to it.
func TestFlaggedTimeIsForTheFrontDesk(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	ctx := context.Background()

	working(t, pool)
	backdate(t, pool, 16)
	second := newJob2(t, pool)
	move(t, pool, second, workshop.StateEstimated, workshop.StateAwaitingApproval, workshop.StateApproved)
	workshop.ClockIn(ctx, pool, technician(), second)

	flagged, err := workshop.FlaggedTime(ctx, pool, advisor())
	if err != nil {
		t.Fatalf("FlaggedTime: %v", err)
	}
	if len(flagged) != 1 {
		t.Fatalf("the front desk sees %d flagged entries, want 1", len(flagged))
	}
	if _, err := workshop.FlaggedTime(ctx, pool, technician()); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("a technician can see the flagged list: %v", err)
	}
}

// A correction somebody cannot see is indistinguishable from the hours having
// always been that.
func TestCorrectingKeepsTheOriginal(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	ctx := context.Background()

	job := working(t, pool)
	backdate(t, pool, 16)
	second := newJob2(t, pool)
	move(t, pool, second, workshop.StateEstimated, workshop.StateAwaitingApproval, workshop.StateApproved)
	workshop.ClockIn(ctx, pool, technician(), second)

	flagged, _ := workshop.FlaggedTime(ctx, pool, advisor())
	entry := flagged[0]
	originalStart := entry.StartedAt

	newEnd := entry.StartedAt.Add(3 * time.Hour)
	if err := workshop.CorrectTime(ctx, pool, advisor(), entry.ID,
		entry.StartedAt, newEnd, "Went home at 17:00"); err != nil {
		t.Fatalf("CorrectTime: %v", err)
	}

	entries, _ := workshop.TimeFor(ctx, pool, advisor(), job)
	e := entries[0]
	if !e.Corrected() {
		t.Fatal("the entry does not read as corrected")
	}
	if e.CorrectedBy == "" {
		t.Error("the correction does not say who made it")
	}
	if e.OriginalStart == nil || !e.OriginalStart.Equal(originalStart) {
		t.Error("the original start time was not kept")
	}
	if e.OriginalEnd == nil {
		t.Error("the original end time was not kept")
	}
	if e.Flagged {
		t.Error("the entry is still flagged after being corrected")
	}
	if got := e.Duration(); got < 2*time.Hour+59*time.Minute || got > 3*time.Hour+time.Minute {
		t.Errorf("duration = %v, want about three hours", got)
	}

	// It leaves the list that asks for attention.
	if flagged, _ := workshop.FlaggedTime(ctx, pool, advisor()); len(flagged) != 0 {
		t.Errorf("%d entries still need attention", len(flagged))
	}
}

func TestCorrectionsAreRefusedWhereTheyWouldBeWrong(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	ctx := context.Background()

	job := working(t, pool)
	backdate(t, pool, 16)
	second := newJob2(t, pool)
	move(t, pool, second, workshop.StateEstimated, workshop.StateAwaitingApproval, workshop.StateApproved)
	workshop.ClockIn(ctx, pool, technician(), second)

	entries, _ := workshop.TimeFor(ctx, pool, advisor(), job)
	e := entries[0]

	// A technician cannot correct their own hours.
	if err := workshop.CorrectTime(ctx, pool, technician(), e.ID,
		e.StartedAt, e.StartedAt.Add(time.Hour), ""); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("a technician corrected their own hours: %v", err)
	}
	// Backwards.
	if err := workshop.CorrectTime(ctx, pool, advisor(), e.ID,
		e.StartedAt, e.StartedAt.Add(-time.Hour), ""); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("an entry that ends before it starts was accepted: %v", err)
	}
	// Replacing one implausible number with another.
	if err := workshop.CorrectTime(ctx, pool, advisor(), e.ID,
		e.StartedAt, e.StartedAt.Add(20*time.Hour), ""); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a twenty-hour correction was accepted: %v", err)
	}
}

// Hours underneath an issued invoice must not move: the invoice cannot
// change, so the two would disagree with nothing to show which is right.
func TestHoursOnAnInvoicedJobCannotBeCorrected(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	ctx := context.Background()

	job := readyToInvoice(t, pool)
	if err := workshop.ClockIn(ctx, pool, technician(), job); err != nil {
		t.Fatalf("ClockIn: %v", err)
	}
	if err := workshop.SetState(ctx, pool, technician(), job, workshop.StateReady); err != nil {
		t.Fatalf("SetState: %v", err)
	}
	if _, err := workshop.Issue(ctx, pool, advisor(), job); err != nil {
		t.Fatalf("Issue: %v", err)
	}

	entries, _ := workshop.TimeFor(ctx, pool, advisor(), job)
	if len(entries) == 0 {
		t.Fatal("no entries to correct")
	}
	err := workshop.CorrectTime(ctx, pool, advisor(), entries[0].ID,
		entries[0].StartedAt, entries[0].StartedAt.Add(time.Hour), "")
	if !errors.Is(err, workshop.ErrInvalid) {
		t.Fatalf("CorrectTime on an invoiced job = %v, want a refusal", err)
	}
}
