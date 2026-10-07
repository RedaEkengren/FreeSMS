package workshop_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/database"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// A job waiting for a part said nothing about whether the car was on the
// lift or at home until Thursday.

func presence(t *testing.T, pool *pgxpool.Pool, jobID string) workshop.Presence {
	t.Helper()
	p, err := workshop.PresenceFor(context.Background(), pool, technician(), jobID)
	if err != nil {
		t.Fatalf("PresenceFor: %v", err)
	}
	return p
}

func boardEntry(t *testing.T, pool *pgxpool.Pool, jobID string) workshop.BoardEntry {
	t.Helper()
	board, err := workshop.Board(context.Background(), pool, advisor())
	if err != nil {
		t.Fatalf("Board: %v", err)
	}
	for _, b := range board {
		if b.ID == jobID {
			return b
		}
	}
	t.Fatalf("job %s is not on the board", jobID)
	return workshop.BoardEntry{}
}

func thursday() time.Time { return time.Date(2026, time.October, 15, 0, 0, 0, 0, time.UTC) }

func TestACarGoesHomeUnfinishedAndComesBackToTheSameJob(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	addTechnician(t, pool)
	jobID := working(t, pool)

	if p := presence(t, pool, jobID); !p.Here || len(p.Events) != 1 || p.Events[0].Event != "left" {
		t.Fatalf("after intake: %+v, want here with the car recorded as left", p)
	}

	back := thursday()
	if err := workshop.CarCollected(ctx, pool, advisor(), jobID, &back); err != nil {
		t.Fatalf("CarCollected: %v", err)
	}
	p := presence(t, pool, jobID)
	if p.Here || p.ExpectedBack == nil || !p.ExpectedBack.Equal(back) {
		t.Fatalf("after collection: %+v, want away until the 15th", p)
	}
	b := boardEntry(t, pool, jobID)
	if !b.CarAway || b.ExpectedBack == nil || !b.ExpectedBack.Equal(back) {
		t.Errorf("the board says away=%v back=%v", b.CarAway, b.ExpectedBack)
	}
	jobs, _ := workshop.OpenJobs(ctx, pool, technician())
	for _, j := range jobs {
		if j.ID == jobID && !j.CarAway {
			t.Error("the technician's list does not say the car is at home")
		}
	}

	if err := workshop.CarReturned(ctx, pool, advisor(), jobID); err != nil {
		t.Fatalf("CarReturned: %v", err)
	}
	p = presence(t, pool, jobID)
	if !p.Here || p.ExpectedBack != nil || len(p.Events) != 3 {
		t.Errorf("after the return: %+v, want here, no date, and all three events kept", p)
	}
	if b := boardEntry(t, pool, jobID); b.CarAway {
		t.Error("the board still says the car is at home")
	}
}

// What cannot have happened is refused, not stored.
func TestACarCannotBeCollectedTwiceOrReturnWithoutLeaving(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	jobID := newJob(t, pool)

	if err := workshop.CarReturned(ctx, pool, advisor(), jobID); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("returned while here: %v", err)
	}
	if err := workshop.CarRebooked(ctx, pool, advisor(), jobID, thursday()); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("rebooked while here: %v", err)
	}
	if err := workshop.CarCollected(ctx, pool, advisor(), jobID, nil); err != nil {
		t.Fatalf("CarCollected: %v", err)
	}
	if err := workshop.CarCollected(ctx, pool, advisor(), jobID, nil); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("collected twice: %v", err)
	}
	if n := len(presence(t, pool, jobID).Events); n != 2 {
		t.Errorf("%d events stored, want 2: the refusals stored nothing", n)
	}
}

// A moved date is a row of its own: the shop can see it moved, and when.
func TestMovingTheReturnKeepsTheFirstDate(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	jobID := newJob(t, pool)
	first, later := thursday(), thursday().AddDate(0, 0, 4)

	if err := workshop.CarCollected(ctx, pool, advisor(), jobID, &first); err != nil {
		t.Fatal(err)
	}
	if err := workshop.CarRebooked(ctx, pool, advisor(), jobID, later); err != nil {
		t.Fatalf("CarRebooked: %v", err)
	}
	p := presence(t, pool, jobID)
	if p.ExpectedBack == nil || !p.ExpectedBack.Equal(later) {
		t.Errorf("expected back %v, want the moved date", p.ExpectedBack)
	}
	if len(p.Events) != 3 || !p.Events[1].ExpectedBack.Equal(first) {
		t.Errorf("the first date is gone: %+v", p.Events)
	}
	if b := boardEntry(t, pool, jobID); b.ExpectedBack == nil || !b.ExpectedBack.Equal(later) {
		t.Errorf("the board shows %v, want the moved date", b.ExpectedBack)
	}
}

// Handing a car back is the counter's; knowing where it is, everybody's.
func TestOnlyTheCounterRecordsWhereTheCarIs(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	addTechnician(t, pool)
	jobID := working(t, pool)

	if err := workshop.CarCollected(ctx, pool, technician(), jobID, nil); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("a technician recorded a collection: %v", err)
	}
	if _, err := workshop.PresenceFor(ctx, pool, technician(), jobID); err != nil {
		t.Errorf("a technician could not read where the car is: %v", err)
	}
}

// What happened is not edited. Checked in the database, where a hurried
// fix would otherwise go.
func TestWhereACarWasIsNotEdited(t *testing.T) {
	pool := setup(t)
	jobID := newJob(t, pool)
	err := database.InShop(context.Background(), pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE vehicle_presence SET event = 'collected' WHERE work_order_id = $1`, jobID)
		return err
	})
	if err == nil {
		t.Error("a presence row was edited")
	}
}

// A job opened before presence was recorded had its car in the shop.
func TestAJobWithNoPresenceRowsHasItsCarHere(t *testing.T) {
	pool := setup(t)
	jobID := newJob(t, pool)
	database.InShop(context.Background(), pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM vehicle_presence WHERE work_order_id = $1`, jobID)
		return err
	})
	if p := presence(t, pool, jobID); !p.Here {
		t.Error("a job with no rows reads as away")
	}
	if b := boardEntry(t, pool, jobID); b.CarAway {
		t.Error("the board reads a job with no rows as away")
	}
}
