package workshop_test

import (
	"context"
	"testing"

	"github.com/RedaEkengren/FreeSMS/internal/database"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func i16(n int16) *int16 { return &n }

func store(t *testing.T, pool *pgxpool.Pool, l workshop.LabourTime) {
	t.Helper()
	if err := workshop.SaveLabourTime(context.Background(), pool, advisor(), l); err != nil {
		t.Fatalf("SaveLabourTime(%s): %v", l.Operation, err)
	}
}

// Two stored times can both apply. The narrower one wins, deterministically,
// and the page says which was used -- a suggestion nobody can account for is
// one nobody trusts.
func TestTheNarrowestMatchWins(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()

	store(t, pool, workshop.LabourTime{Operation: "Front brakes", Minutes: 120})
	store(t, pool, workshop.LabourTime{Operation: "Front brakes", Make: "Volvo", Minutes: 100})
	store(t, pool, workshop.LabourTime{Operation: "Front brakes", Make: "Volvo", Model: "V70", Minutes: 90})
	// A different model must not win.
	store(t, pool, workshop.LabourTime{Operation: "Front brakes", Make: "Volvo", Model: "S60", Minutes: 45})

	got, err := workshop.SuggestFor(ctx, pool, advisor(), vehicleA)
	if err != nil {
		t.Fatalf("SuggestFor: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d suggestions, want 1 per operation", len(got))
	}
	if got[0].Minutes != 90 {
		t.Errorf("suggested %d minutes, want 90 -- the Volvo V70 rule", got[0].Minutes)
	}
	if got[0].MatchedRule != "Volvo V70" {
		t.Errorf("matched rule reads %q, want Volvo V70", got[0].MatchedRule)
	}
}

// A time scoped to years must not be offered for a car outside them.
func TestYearScopeIsRespected(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()

	// vehicleA is a Volvo V70 with no model year in the fixture, so a rule
	// requiring years cannot match it -- which is the safe direction.
	store(t, pool, workshop.LabourTime{
		Operation: "Cambelt", Make: "Volvo", YearFrom: i16(2000), YearTo: i16(2008), Minutes: 300})

	got, err := workshop.SuggestFor(ctx, pool, advisor(), vehicleA)
	if err != nil {
		t.Fatalf("SuggestFor: %v", err)
	}
	for _, s := range got {
		if s.Operation == "Cambelt" {
			t.Error("a year-scoped time was offered for a vehicle with no year on record")
		}
	}
}

// The library has to learn, or a wrong entry poisons every future estimate
// quietly.
func TestTheLibraryShowsWhatTheJobsReallyTook(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	ctx := context.Background()

	store(t, pool, workshop.LabourTime{Operation: "Front brakes", Make: "Volvo", Minutes: 60})
	suggestions, _ := workshop.SuggestFor(ctx, pool, advisor(), vehicleA)
	if len(suggestions) != 1 {
		t.Fatalf("got %d suggestions, want 1", len(suggestions))
	}
	timeID := suggestions[0].ID

	if suggestions[0].Actuals.Known() {
		t.Error("a brand new entry already claims to know what jobs took")
	}

	// Put it on a job, clock against that line, and look again.
	job := working(t, pool)
	if err := workshop.AddLine(ctx, pool, advisor(), job, workshop.NewLine{
		Kind: "labour", Description: "Front brakes", QuantityMilli: 1000,
		UnitPriceMinor: 89500, VATRateBasis: 2500, LabourTimeID: timeID,
	}); err != nil {
		t.Fatalf("AddLine: %v", err)
	}

	// Two hours clocked against that line: the stored hour was optimistic.
	if err := clockAgainstTheLine(t, pool, job, 120); err != nil {
		t.Fatalf("clock: %v", err)
	}

	times, err := workshop.LabourTimes(ctx, pool, advisor())
	if err != nil {
		t.Fatalf("LabourTimes: %v", err)
	}
	if len(times) != 1 {
		t.Fatalf("got %d stored times, want 1", len(times))
	}
	a := times[0].Actuals
	if !a.Known() {
		t.Fatal("the entry still shows nothing about real jobs")
	}
	if a.Jobs != 1 {
		t.Errorf("jobs = %d, want 1", a.Jobs)
	}
	if a.MedianMinutes < 118 || a.MedianMinutes > 122 {
		t.Errorf("median = %d minutes, want about 120 -- the stored hour was optimistic", a.MedianMinutes)
	}
}

// A model without a make is not a scope anybody can reason about.
func TestAModelWithoutAMakeIsRefused(t *testing.T) {
	pool := setup(t)
	err := workshop.SaveLabourTime(context.Background(), pool, advisor(),
		workshop.LabourTime{Operation: "Front brakes", Model: "V70", Minutes: 60})
	if err == nil {
		t.Error("a model with no make was stored")
	}
}

// A technician does not keep the shop's prices.
func TestATechnicianCannotChangeTheLibrary(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	err := workshop.SaveLabourTime(context.Background(), pool, technician(),
		workshop.LabourTime{Operation: "Front brakes", Minutes: 60})
	if err == nil {
		t.Error("a technician stored a labour time")
	}
}

// The shop's own adjustment stays visible rather than disappearing into the
// book time.
func TestTheShopsAdjustmentIsKeptApart(t *testing.T) {
	pool := setup(t)
	store(t, pool, workshop.LabourTime{
		Operation: "Front brakes", Make: "Volvo", Minutes: 60, AdjustmentMinutes: 15,
		Note: "Add a quarter of an hour, the bolts seize on these"})

	times, err := workshop.LabourTimes(context.Background(), pool, advisor())
	if err != nil {
		t.Fatalf("LabourTimes: %v", err)
	}
	l := times[0]
	if l.Minutes != 60 {
		t.Errorf("book time = %d, want 60 kept apart from the adjustment", l.Minutes)
	}
	if l.AdjustmentMinutes != 15 {
		t.Errorf("adjustment = %d, want 15", l.AdjustmentMinutes)
	}
	if l.TotalMinutes() != 75 {
		t.Errorf("total = %d, want 75", l.TotalMinutes())
	}
}

// clockAgainstTheLine records a finished stretch of work on the job's only
// labour line.
func clockAgainstTheLine(t *testing.T, pool *pgxpool.Pool, jobID string, minutes int) error {
	t.Helper()
	ctx := context.Background()
	return database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		var lineID string
		if err := tx.QueryRow(ctx,
			`SELECT id FROM work_order_lines WHERE work_order_id = $1 AND labour_time_id IS NOT NULL LIMIT 1`,
			jobID).Scan(&lineID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO time_entries (shop_id, work_order_id, line_id, user_id, started_at, ended_at)
			VALUES ($1, $2, $3, $4, now() - make_interval(mins => $5), now())`,
			shopID, jobID, lineID, techUserID, minutes)
		return err
	})
}
