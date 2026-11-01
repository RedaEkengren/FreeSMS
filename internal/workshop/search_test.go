package workshop_test

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// The one search (#159).

func foundKinds(t *testing.T, scope access.Scope, q string) []string {
	t.Helper()
	found, err := workshop.SearchAll(context.Background(), currentPool, scope, q)
	if err != nil {
		t.Fatalf("SearchAll(%q) as %s: %v", q, scope.Role, err)
	}
	var out []string
	for _, f := range found {
		out = append(out, f.Kind+":"+f.MatchedOn)
	}
	return out
}

// A plate, a chassis number typed with an O for a 0, a job's number and a
// name, each found by whoever may see it and by nobody else: a name finds
// the customer and their car for the counter and nothing for a
// technician, and a job's number finds the job only while the workshop
// runs. The exact plate ranks above a name, and a customer erased is not
// found by the name they had.
func TestTheOneSearchFindsWhatTheRoleMaySee(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	currentPool = pool
	job := newJob(t, pool)
	if _, err := pool.Exec(ctx, `UPDATE vehicles SET vin = 'YV2SW58D023456789' WHERE id = $1`, vehicleA); err != nil {
		t.Fatal(err)
	}
	var number int64
	if err := pool.QueryRow(ctx, `SELECT number FROM work_orders WHERE id = $1`, job).Scan(&number); err != nil {
		t.Fatal(err)
	}

	if got := foundKinds(t, advisor(), "abc 12d"); len(got) != 1 || got[0] != "vehicle:registration" {
		t.Errorf("a plate: %v", got)
	}
	if got := foundKinds(t, technician(), "YV2SW58DO23456789"); len(got) != 1 || got[0] != "vehicle:chassis number" {
		t.Errorf("a chassis number with an O for a 0: %v", got)
	}
	for _, s := range []access.Scope{advisor(), technician()} {
		found, err := workshop.SearchAll(ctx, pool, s, "#"+itoa(number))
		if err != nil || len(found) != 1 || found[0].Kind != "job" || found[0].ID != job || found[0].Link() != "/jobs/"+job {
			t.Errorf("%s, a job's number: %+v, %v", s.Role, found, err)
		}
	}
	off := advisor()
	off.Modules = []access.Module{access.Dealer}
	if got := foundKinds(t, off, itoa(number)); len(got) != 0 {
		t.Errorf("a job's number with the workshop off: %v", got)
	}
	got := foundKinds(t, advisor(), "A Customer")
	if strings.Join(got, " ") != "vehicle:owner customer:customer" {
		t.Errorf("a name at the counter: %v, want the car and the customer", got)
	}
	if got := foundKinds(t, technician(), "A Customer"); len(got) != 0 {
		t.Errorf("a name, as a technician: %v, want nothing", got)
	}
	if _, err := workshop.SearchAll(ctx, pool, advisor(), strings.Repeat("A", 101)); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a search too long: %v", err)
	}
	if err := workshop.ErasePerson(ctx, pool, owner(), nil, "22222222-2222-2222-2222-222222222223", "Kunden bad om det"); err != nil {
		t.Fatal(err)
	}
	for _, f := range must(workshop.SearchAll(ctx, pool, owner(), "A Customer")) {
		if f.Kind == "customer" || strings.Contains(f.Detail, "A Customer") {
			t.Errorf("found by the name of a person erased: %+v", f)
		}
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
