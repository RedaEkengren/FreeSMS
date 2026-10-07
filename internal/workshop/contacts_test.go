package workshop_test

import (
	"context"
	"errors"
	"testing"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Nothing used to record that the customer was told their car is ready.

func readyJob(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	addTechnician(t, pool)
	job := working(t, pool)
	move(t, pool, job, workshop.StateInProgress, workshop.StateReady)
	return job
}

func TestTellingTheCustomerIsRecordedAndShownOnTheBoard(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	job := readyJob(t, pool)

	if b := boardEntry(t, pool, job); b.ToldAt != nil {
		t.Fatal("a ready car reads as told before anybody did")
	}
	// A try that did not reach them is a row, and not "told".
	if err := workshop.RecordContact(ctx, pool, advisor(), job, "no_answer", "Röstbrevlåda"); err != nil {
		t.Fatalf("RecordContact: %v", err)
	}
	if b := boardEntry(t, pool, job); b.ToldAt != nil {
		t.Error("no answer reads as told")
	}
	if err := workshop.RecordContact(ctx, pool, advisor(), job, "phoned", ""); err != nil {
		t.Fatalf("RecordContact: %v", err)
	}
	if b := boardEntry(t, pool, job); b.ToldAt == nil {
		t.Error("the board does not say the customer was told")
	}
	contacts, err := workshop.ContactsFor(ctx, pool, advisor(), job)
	if err != nil || len(contacts) != 2 || contacts[0].How != "phoned" || contacts[1].Note != "Röstbrevlåda" {
		t.Errorf("contacts = %+v, %v; want both tries, newest first", contacts, err)
	}
}

// Told about the last time it was ready does not count once it has been
// back on a lift.
func TestTellingThemCountsOnlySinceItLastBecameReady(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	job := readyJob(t, pool)
	if err := workshop.RecordContact(ctx, pool, advisor(), job, "phoned", ""); err != nil {
		t.Fatal(err)
	}
	move(t, pool, job, workshop.StateInProgress, workshop.StateReady)
	if b := boardEntry(t, pool, job); b.ToldAt != nil {
		t.Error("a call about the previous time it was ready counts as told")
	}
}

func TestThereIsNothingToTellBeforeTheCarIsReady(t *testing.T) {
	pool := setup(t)
	job := newJob(t, pool)
	if err := workshop.RecordContact(context.Background(), pool, advisor(), job, "phoned", ""); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("told about a car that is not ready: %v", err)
	}
}

func TestTellingTheCustomerIsTheCountersAndSaysHow(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	job := readyJob(t, pool)
	if err := workshop.RecordContact(ctx, pool, technician(), job, "phoned", ""); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("a technician recorded telling the customer: %v", err)
	}
	if _, err := workshop.ContactsFor(ctx, pool, technician(), job); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("a technician read the contacts: %v", err)
	}
	if err := workshop.RecordContact(ctx, pool, advisor(), job, "carrier pigeon", ""); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("an invented way was accepted: %v", err)
	}
}
