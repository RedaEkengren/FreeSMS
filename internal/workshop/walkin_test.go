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
)

// A car with nothing known about it but what is wrong is still taken in, and
// the job says what it does not know: no customer, nobody on it, no promise.
func TestAWalkInWithNothingButTheComplaintIsTakenIn(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	id, err := workshop.TakeInWalkIn(ctx, pool, advisor(), workshop.WalkIn{Registration: "WLK 001", Complaint: "Rattles"})
	if err != nil {
		t.Fatalf("TakeInWalkIn: %v", err)
	}
	job, _, err := workshop.JobByID(ctx, pool, advisor(), id)
	if err != nil {
		t.Fatal(err)
	}
	if job.HasCustomer || job.AssignedTo != "" || job.PromisedAt != nil {
		t.Errorf("a walk-in with nothing given has a customer, technician or promise: %+v", job)
	}
	step := workshop.NextStep(workshop.StepFacts{State: workshop.State(job.State), HasCustomer: job.HasCustomer})
	if len(step.Missing) != 3 {
		t.Errorf("the job does not say what is missing: %v", step.Missing)
	}
}

// What the desk confirms is on the job: a new customer made from what was
// typed, the technician and the promise -- and the promise is nothing else.
func TestAWalkInCarriesWhatTheDeskConfirmed(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	ctx := context.Background()
	promise := time.Date(2026, 10, 16, 15, 0, 0, 0, time.UTC)
	id, err := workshop.TakeInWalkIn(ctx, pool, advisor(), workshop.WalkIn{
		Registration: "WLK 002", Complaint: "Squeaks", Customer: "new", NewName: "Walter Walkin", NewPhone: "070-111 22 33",
		TechnicianID: techUserID, PromisedAt: &promise})
	if err != nil {
		t.Fatalf("TakeInWalkIn: %v", err)
	}
	job, _, _ := workshop.JobByID(ctx, pool, advisor(), id)
	if !job.HasCustomer || job.AssignedTo != "A Technician" || job.PromisedAt == nil || !job.PromisedAt.Equal(promise) {
		t.Errorf("the job after intake: customer %v, on it %q, promised %v", job.HasCustomer, job.AssignedTo, job.PromisedAt)
	}
	contact, err := workshop.ContactFor(ctx, pool, advisor(), id)
	if err != nil || contact.Name != "Walter Walkin" {
		t.Errorf("the contact is %q (%v)", contact.Name, err)
	}
	// The payer is not the owner unless the desk said so.
	var owners int
	database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM vehicle_ownership o JOIN vehicle_registrations r ON r.vehicle_id = o.vehicle_id
			WHERE r.normalised = 'WLK002'`).Scan(&owners)
	})
	if owners != 0 {
		t.Errorf("the customer was made the owner without being asked")
	}
}

func TestAWalkInRefusesWhatCannotBeKept(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	for name, in := range map[string]workshop.WalkIn{
		"no complaint":           {Registration: "WLK 003"},
		"a new customer unnamed": {Registration: "WLK 003", Complaint: "x", Customer: "new"},
		"an unknown technician":  {Registration: "WLK 003", Complaint: "x", TechnicianID: "aaaaaaaa-0000-0000-0000-000000000009"},
		"a way nobody invented":  {Registration: "WLK 003", Complaint: "x", Customer: "both"},
	} {
		if _, err := workshop.TakeInWalkIn(ctx, pool, advisor(), in); !errors.Is(err, workshop.ErrInvalid) {
			t.Errorf("%s: %v, want ErrInvalid", name, err)
		}
	}
	if jobs, _, _ := counts(t, pool); jobs != 0 {
		t.Errorf("%d jobs were left behind by refused intakes", jobs)
	}
	if _, err := workshop.TakeInWalkIn(ctx, pool, access.Scope{ShopID: shopID, UserID: techUserID, Role: access.RoleTechnician},
		workshop.WalkIn{Registration: "WLK 003", Complaint: "x"}); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("a technician taking a car in: %v", err)
	}
}
