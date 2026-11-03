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

// An exact plate, chassis number or job number goes straight to its record
// even when weaker matches come with it -- the digits of a plate in a
// customer's telephone number, a job's number inside a chassis number --
// while a part of a chassis number is not exact and leaves the list to
// choose from.
func TestAnExactMatchGoesStraightThereAmongWeakerOnes(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	currentPool = pool
	job := newJob(t, pool)
	var number int64
	if err := pool.QueryRow(ctx, `SELECT number FROM work_orders WHERE id = $1`, job).Scan(&number); err != nil {
		t.Fatal(err)
	}
	vin := "YV2SW58D0234567" + itoa(number%10) + itoa(number%10)
	if _, err := pool.Exec(ctx, `UPDATE vehicles SET vin = $2 WHERE id = $1`, vehicleA, vin); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE people SET phone = $2 WHERE id = $1`, customerPersonID,
		"070-"+strings.Map(digitsOnly, vin)+"-12"+itoa(number)); err != nil {
		t.Fatal(err)
	}
	straight := func(q string) (workshop.Found, bool, int) {
		t.Helper()
		found := must(workshop.SearchAll(ctx, pool, advisor(), q))
		f, ok := workshop.Straight(found)
		return f, ok, len(found)
	}
	for q, want := range map[string]string{
		"abc 12d":            "/vehicles/" + vehicleA,
		vin:                  "/vehicles/" + vehicleA,
		strings.ToLower(vin): "/vehicles/" + vehicleA,
		"#" + itoa(number):   "/jobs/" + job,
	} {
		f, ok, n := straight(q)
		if n < 2 {
			t.Errorf("%q found %d, want the exact match among weaker ones", q, n)
		}
		if !ok || f.Link() != want {
			t.Errorf("%q goes to %q (%v), want %q", q, f.Link(), ok, want)
		}
	}
	if f, ok, n := straight("34567"); ok {
		t.Errorf("a part of a chassis number goes straight to %q, of %d", f.Link(), n)
	}
}

func digitsOnly(r rune) rune {
	if r >= '0' && r <= '9' {
		return r
	}
	return -1
}

// A chassis number stored with an O, I or Q -- an old car's, not ISO 3779,
// or one typed with a letter at intake -- is found by typing it as it was
// stored, and by typing the digits it is mistaken for.
func TestAChassisNumberStoredWithALetterForADigitIsFoundAsStored(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	currentPool = pool
	if _, err := pool.Exec(ctx, `UPDATE vehicles SET vin = 'WOLQI58D023456789' WHERE id = $1`, vehicleA); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"WOLQI58D023456789", "w0l0158d023456789", "OLQI58"} {
		if got := foundKinds(t, technician(), q); len(got) != 1 || got[0] != "vehicle:chassis number" {
			t.Errorf("%q: %v, want the car by its chassis number", q, got)
		}
	}
}

// The exact plate is first however many cars match a part of it: the
// ranking happens before the list is cut, not after.
func TestTheExactPlateIsFoundAmongMoreMatchesThanAreShown(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	currentPool = pool
	for _, q := range []string{
		`INSERT INTO vehicles (shop_id, label) SELECT $1, 'XYZ99 spare ' || n FROM generate_series(1, 40) n`,
		`INSERT INTO vehicles (id, shop_id) VALUES ('ffffffff-ffff-ffff-ffff-ffffffffffff', $1)`,
		`INSERT INTO vehicle_registrations (shop_id, vehicle_id, registration, normalised)
		 VALUES ($1, 'ffffffff-ffff-ffff-ffff-ffffffffffff', 'XYZ 99', 'XYZ99')`,
	} {
		if _, err := pool.Exec(ctx, q, shopID); err != nil {
			t.Fatal(err)
		}
	}
	found := must(workshop.SearchAll(ctx, pool, advisor(), "XYZ99"))
	if len(found) == 0 || found[0].ID != "ffffffff-ffff-ffff-ffff-ffffffffffff" || found[0].MatchedOn != "registration" {
		t.Errorf("the exact plate is not first among %d: %+v", len(found), found)
	}
}

// A chassis number imported with spaces or hyphens in it is found by
// typing the number, with or without them, and goes straight there: the
// stored number is compared without its separators wherever it is
// compared, not only where the match is ranked.
func TestAChassisNumberStoredWithSpacesIsFoundTypedWithout(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	currentPool = pool
	if _, err := pool.Exec(ctx, `UPDATE vehicles SET vin = 'YV1 SW612-52 1234567' WHERE id = $1`, vehicleA); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"YV1SW612521234567", "yv1 sw612 52 1234567", "SW61252"} {
		if got := foundKinds(t, technician(), q); len(got) != 1 || got[0] != "vehicle:chassis number" {
			t.Errorf("%q: %v, want the car by its chassis number", q, got)
		}
	}
	f, ok := workshop.Straight(must(workshop.SearchAll(ctx, pool, advisor(), "YV1SW612521234567")))
	if !ok || f.Link() != "/vehicles/"+vehicleA {
		t.Errorf("the whole number goes to %q (%v), want the car", f.Link(), ok)
	}
}
