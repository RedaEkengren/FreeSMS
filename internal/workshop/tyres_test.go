package workshop_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

func fourTyres(dimension string) []workshop.NewTyre {
	var out []workshop.NewTyre
	for _, p := range []string{"front_left", "front_right", "rear_left", "rear_right"} {
		out = append(out, workshop.NewTyre{Position: p, Dimension: dimension, DotWeek: 12, DotYear: 2023})
	}
	return out
}

// anotherCar adds a car to the customer, or to nobody when customer is empty.
func anotherCar(t *testing.T, pool *pgxpool.Pool, customer string) string {
	t.Helper()
	ctx := context.Background()
	var id string
	if err := pool.QueryRow(ctx, `INSERT INTO vehicles (shop_id, make, model) VALUES ($1, 'Saab', '9-3') RETURNING id`, shopID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if customer != "" {
		if _, err := pool.Exec(ctx, `INSERT INTO vehicle_ownership (shop_id, vehicle_id, customer_id) VALUES ($1, $2, $3)`, shopID, id, customer); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

func summerSet(customer, vehicle string) workshop.NewTyreSet {
	return workshop.NewTyreSet{CustomerID: customer, VehicleID: vehicle, Season: workshop.SeasonSummer, Brand: "Michelin", Model: "Primacy 4",
		Rims: workshop.RimsAlloy, TPMS: true, Fasteners: true, Note: "Låsbultar i handskfacket", Tyres: fourTyres("205/55R16 91V")}
}

// A customer with two sets on one car keeps them apart, each with its own
// tyres, its rims and sensors, and a set can be brought in with no car at all.
func TestTwoSetsOnOneCarAndASetWithNoCarAreKeptApart(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	summer, err := workshop.CreateTyreSet(ctx, pool, advisor(), summerSet(customerA, vehicleA))
	if err != nil {
		t.Fatal(err)
	}
	winter := summerSet(customerA, vehicleA)
	winter.Season, winter.Brand, winter.Model, winter.Rims, winter.TPMS = workshop.SeasonWinterFriction, "Nokian", "WR D4", workshop.RimsSteel, false
	winter.Tyres = fourTyres("205/55R16 91H")
	winterID, err := workshop.CreateTyreSet(ctx, pool, advisor(), winter)
	if err != nil {
		t.Fatal(err)
	}
	loose := workshop.NewTyreSet{CustomerID: customerA, Season: workshop.SeasonAllSeason, Rims: workshop.RimsNone,
		Tyres: []workshop.NewTyre{{Position: "spare", Dimension: "T125/80R17"}}}
	looseID, err := workshop.CreateTyreSet(ctx, pool, advisor(), loose)
	if err != nil {
		t.Fatal(err)
	}

	onCar, err := workshop.TyreSetsOf(ctx, pool, advisor(), workshop.TyreFilter{VehicleID: vehicleA})
	if err != nil || len(onCar) != 2 {
		t.Fatalf("the sets on the car: %+v, %v", onCar, err)
	}
	all, _ := workshop.TyreSetsOf(ctx, pool, advisor(), workshop.TyreFilter{CustomerID: customerA})
	if len(all) != 3 {
		t.Fatalf("the customer's sets: %d", len(all))
	}
	s, err := workshop.TyreSetByID(ctx, pool, advisor(), summer)
	if err != nil {
		t.Fatal(err)
	}
	if s.Season != workshop.SeasonSummer || s.Brand != "Michelin" || s.Rims != workshop.RimsAlloy || !s.TPMS || !s.Fasteners ||
		s.Note != "Låsbultar i handskfacket" || s.Owner != "A Customer" || s.CustomerID != customerA || s.VehicleID != vehicleA ||
		len(s.Tyres) != 4 || s.Tyres[0].Position != "front_left" || s.Tyres[0].Dimension != "205/55R16 91V" || s.Tyres[0].DotWeek != 12 || s.Tyres[0].DotYear != 2023 {
		t.Errorf("the summer set: %+v", s)
	}
	if w, _ := workshop.TyreSetByID(ctx, pool, advisor(), winterID); w.Season != workshop.SeasonWinterFriction || w.TPMS || len(w.Tyres) != 4 {
		t.Errorf("the winter set: %+v", w)
	}
	if l, _ := workshop.TyreSetByID(ctx, pool, advisor(), looseID); l.VehicleID != "" || l.Vehicle != "" || len(l.Tyres) != 1 || l.Tyres[0].DotYear != 0 {
		t.Errorf("the set with no car: %+v", l)
	}
}

// One tyre replaced keeps its history: the old row stays, the new points at it,
// and the set shows the new one; a tyre cannot be replaced twice.
func TestOneTyreReplacedKeepsItsHistory(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	id, err := workshop.CreateTyreSet(ctx, pool, advisor(), summerSet(customerA, vehicleA))
	if err != nil {
		t.Fatal(err)
	}
	s, _ := workshop.TyreSetByID(ctx, pool, advisor(), id)
	old := s.Tyres[1]
	if err := workshop.ReplaceTyre(ctx, pool, advisor(), old.ID, workshop.NewTyre{Position: old.Position, Dimension: "205/55R16 91V", DotWeek: 3, DotYear: 2025}); err != nil {
		t.Fatal(err)
	}
	s, _ = workshop.TyreSetByID(ctx, pool, advisor(), id)
	if len(s.Tyres) != 4 || s.Tyres[1].ID == old.ID || s.Tyres[1].ReplacesID != old.ID || s.Tyres[1].DotYear != 2025 || s.Tyres[0].DotYear != 2023 {
		t.Errorf("the set after the replacement: %+v", s.Tyres)
	}
	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM tyres WHERE tyre_set_id = $1`, id).Scan(&rows); err != nil || rows != 5 {
		t.Errorf("rows kept: %d, %v", rows, err)
	}
	if err := workshop.ReplaceTyre(ctx, pool, advisor(), old.ID, workshop.NewTyre{Position: old.Position, Dimension: "205/55R16 91V"}); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a tyre replaced twice: %v", err)
	}
	if err := workshop.ReplaceTyre(ctx, pool, advisor(), "nope", workshop.NewTyre{Position: "spare", Dimension: "205/55R16"}); !errors.Is(err, workshop.ErrNotFound) {
		t.Errorf("a tyre that is not there: %v", err)
	}
}

// A set moves to another car of its owner as an event: where it was stays, and
// a car that is not the owner's is refused.
func TestASetMovesToTheOwnersNewCarAndKeepsWhereItWas(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	id, err := workshop.CreateTyreSet(ctx, pool, advisor(), summerSet(customerA, vehicleA))
	if err != nil {
		t.Fatal(err)
	}
	newCar, strangers := anotherCar(t, pool, customerA), anotherCar(t, pool, "")
	if err := workshop.MoveTyreSet(ctx, pool, advisor(), id, strangers); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a set put on a car its owner does not have: %v", err)
	}
	if err := workshop.MoveTyreSet(ctx, pool, advisor(), id, newCar); err != nil {
		t.Fatal(err)
	}
	if err := workshop.MoveTyreSet(ctx, pool, advisor(), id, ""); err != nil {
		t.Fatal(err)
	}
	s, _ := workshop.TyreSetByID(ctx, pool, advisor(), id)
	if s.VehicleID != "" || len(s.Moves) != 3 || s.Moves[0].VehicleID != vehicleA || s.Moves[1].VehicleID != newCar || s.Moves[2].VehicleID != "" {
		t.Errorf("the moves: %+v", s.Moves)
	}
	if err := workshop.MoveTyreSet(ctx, pool, advisor(), id, newCar); err != nil {
		t.Fatal(err)
	}
	if on, _ := workshop.TyreSetsOf(ctx, pool, advisor(), workshop.TyreFilter{VehicleID: vehicleA}); len(on) != 0 {
		t.Errorf("the first car still has the set: %+v", on)
	}
	if on, _ := workshop.TyreSetsOf(ctx, pool, advisor(), workshop.TyreFilter{VehicleID: newCar}); len(on) != 1 {
		t.Errorf("the new car has not got the set: %+v", on)
	}
	if _, err := workshop.CreateTyreSet(ctx, pool, advisor(), summerSet(customerA, strangers)); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a set taken in on a car its owner does not have: %v", err)
	}
}

// A technician sees the set and the car and never whose it is; only the
// roles that deal with customers write; the rental desk and a shop with the
// workshop off see nothing.
func TestATechnicianSeesTheSetAndTheCarButNotTheOwner(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	id, err := workshop.CreateTyreSet(ctx, pool, advisor(), summerSet(customerA, vehicleA))
	if err != nil {
		t.Fatal(err)
	}
	s, err := workshop.TyreSetByID(ctx, pool, technician(), id)
	if err != nil || s.Vehicle == "" || len(s.Tyres) != 4 || s.Owner != "" || s.CustomerID != "" {
		t.Errorf("the technician's set: %+v, %v", s, err)
	}
	list, err := workshop.TyreSetsOf(ctx, pool, technician(), workshop.TyreFilter{})
	if err != nil || len(list) != 1 || list[0].Owner != "" || list[0].CustomerID != "" {
		t.Errorf("the technician's list: %+v, %v", list, err)
	}
	if _, err := workshop.CreateTyreSet(ctx, pool, technician(), summerSet(customerA, vehicleA)); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("a technician taking a set in: %v", err)
	}
	if err := workshop.MoveTyreSet(ctx, pool, technician(), id, ""); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("a technician moving a set: %v", err)
	}
	if err := workshop.ReplaceTyre(ctx, pool, technician(), s.Tyres[0].ID, workshop.NewTyre{Position: "spare", Dimension: "205/55R16"}); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("a technician replacing a tyre: %v", err)
	}
	for _, sc := range []access.Scope{everything(rentalDesk()), everything(buyerScope()), everything(sales())} {
		if _, err := workshop.TyreSetsOf(ctx, pool, sc, workshop.TyreFilter{}); !errors.Is(err, access.ErrForbidden) {
			t.Errorf("%s read the sets: %v", sc.Role, err)
		}
	}
	off := advisor()
	off.Modules = []access.Module{access.Dealer}
	if _, err := workshop.TyreSetsOf(ctx, pool, off, workshop.TyreFilter{}); !errors.Is(err, access.ErrModuleOff) {
		t.Errorf("the sets with the workshop off: %v", err)
	}
	if _, err := workshop.CreateTyreSet(ctx, pool, off, summerSet(customerA, "")); !errors.Is(err, access.ErrModuleOff) {
		t.Errorf("a set taken in with the workshop off: %v", err)
	}
}

// What a set is made of is checked: a season and rims from the lists, a
// number of tyres, a dimension, a date that is both week and year.
func TestASetThatIsNotOneIsRefused(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	bad := map[string]func(*workshop.NewTyreSet){
		"season":   func(n *workshop.NewTyreSet) { n.Season = "monsoon" },
		"rims":     func(n *workshop.NewTyreSet) { n.Rims = "gold" },
		"no tyres": func(n *workshop.NewTyreSet) { n.Tyres = nil },
		"thirteen": func(n *workshop.NewTyreSet) {
			n.Tyres = append(fourTyres("205/55R16"), fourTyres("205/55R16")...)
			n.Tyres = append(n.Tyres, n.Tyres[:5]...)
		},
		"dimension":    func(n *workshop.NewTyreSet) { n.Tyres[0].Dimension = "x" },
		"position":     func(n *workshop.NewTyreSet) { n.Tyres[0].Position = "roof" },
		"week only":    func(n *workshop.NewTyreSet) { n.Tyres[0].DotYear = 0 },
		"week 54":      func(n *workshop.NewTyreSet) { n.Tyres[0].DotWeek = 54 },
		"year 1850":    func(n *workshop.NewTyreSet) { n.Tyres[0].DotYear = 1850 },
		"a long note":  func(n *workshop.NewTyreSet) { n.Note = strings.Repeat("x", 501) },
		"a long brand": func(n *workshop.NewTyreSet) { n.Brand = strings.Repeat("x", 101) },
	}
	for name, change := range bad {
		n := summerSet(customerA, vehicleA)
		n.Tyres = fourTyres("205/55R16")
		change(&n)
		if _, err := workshop.CreateTyreSet(ctx, pool, advisor(), n); !errors.Is(err, workshop.ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := workshop.CreateTyreSet(ctx, pool, advisor(), summerSet("nope", "")); !errors.Is(err, workshop.ErrNotFound) {
		t.Errorf("a customer that is not an id: %v", err)
	}
	if _, err := workshop.CreateTyreSet(ctx, pool, advisor(), summerSet("99999999-9999-9999-9999-999999999999", "")); !errors.Is(err, workshop.ErrNotFound) {
		t.Errorf("a customer that is not there: %v", err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM tyre_sets`).Scan(&n); err != nil || n != 0 {
		t.Errorf("sets written by refusals: %d, %v", n, err)
	}
}

// A set's wear over three seasons is its measurements, each a row; a tyre never
// measured says so rather than zero; one below the limit is told apart from one
// below the law; a wrong figure is corrected by another row, and the first stays.
func TestASetsWearOverSeasonsIsItsMeasurementsAndNoneIsOverwritten(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	ctx := context.Background()
	id, err := workshop.CreateTyreSet(ctx, pool, advisor(), summerSet(customerA, vehicleA))
	if err != nil {
		t.Fatal(err)
	}
	s, _ := workshop.TyreSetByID(ctx, pool, technician(), id)
	if s.TreadWarning != workshop.DefaultTreadWarningTenths {
		t.Errorf("the default limit: %d", s.TreadWarning)
	}
	for _, ty := range s.Tyres {
		if ty.TreadTenths != nil || ty.TreadStatus != workshop.TreadNotMeasured {
			t.Errorf("a tyre never measured: %+v", ty)
		}
	}
	front := s.Tyres[0]
	depths := []int{82, 61, 38}
	for i, d := range depths {
		mid, err := workshop.RecordTreadDepth(ctx, pool, technician(), front.ID, d, "")
		if err != nil {
			t.Fatal(err)
		}
		// Each season a year on.
		if _, err := pool.Exec(ctx, `UPDATE tyre_measurements SET measured_at = clock_timestamp() - make_interval(years => $2) WHERE id = $1`, mid, len(depths)-1-i); err != nil {
			t.Fatal(err)
		}
	}
	s, _ = workshop.TyreSetByID(ctx, pool, technician(), id)
	got := s.Tyres[0]
	if got.TreadTenths == nil || *got.TreadTenths != 38 || got.TreadStatus != workshop.TreadOK || len(got.Readings) != 3 ||
		got.Readings[0].Tenths != 82 || got.Readings[2].Tenths != 38 {
		t.Fatalf("the front tyre: %+v", got)
	}
	if s.Tyres[1].TreadStatus != workshop.TreadNotMeasured {
		t.Errorf("a tyre never measured after another was: %+v", s.Tyres[1])
	}

	// One at the shop's limit, one below the law.
	if _, err := workshop.RecordTreadDepth(ctx, pool, technician(), s.Tyres[1].ID, 30, "slitage på innerkanten"); err != nil {
		t.Fatal(err)
	}
	if _, err := workshop.RecordTreadDepth(ctx, pool, technician(), s.Tyres[2].ID, 14, ""); err != nil {
		t.Fatal(err)
	}
	s, _ = workshop.TyreSetByID(ctx, pool, technician(), id)
	if s.Tyres[1].TreadStatus != workshop.TreadWarning || s.Tyres[2].TreadStatus != workshop.TreadBelowLegal || s.Tyres[1].Readings[0].Note != "slitage på innerkanten" {
		t.Errorf("the statuses: %+v / %+v", s.Tyres[1], s.Tyres[2])
	}
	if err := workshop.SetTreadWarning(ctx, pool, owner(), 40); err != nil {
		t.Fatal(err)
	}
	if s, _ = workshop.TyreSetByID(ctx, pool, technician(), id); s.Tyres[0].TreadStatus != workshop.TreadWarning || s.TreadWarning != 40 {
		t.Errorf("the front tyre under a limit of 4.0: %+v", s.Tyres[0])
	}

	// An older figure corrected after a newer one was taken: the newer still
	// stands, for the correction keeps the time of what it corrects.
	{
		tyre := s.Tyres[0]
		first := tyre.Readings[0]
		if _, err := workshop.CorrectTreadDepth(ctx, pool, technician(), first.ID, 80, "Avläst på fel ställe"); err != nil {
			t.Fatal(err)
		}
		again, _ := workshop.TyreSetByID(ctx, pool, technician(), id)
		now := again.Tyres[0]
		if now.TreadTenths == nil || *now.TreadTenths != 38 || len(now.Readings) != 4 || now.Readings[1].Tenths != 80 || now.Readings[1].CorrectsID != first.ID {
			t.Errorf("the front tyre after an old figure was corrected: %+v", now)
		}
	}

	// A figure entered wrongly is corrected, not edited.
	wrong, _ := workshop.RecordTreadDepth(ctx, pool, technician(), s.Tyres[3].ID, 5, "")
	if _, err := workshop.CorrectTreadDepth(ctx, pool, technician(), wrong, 50, ""); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a correction with no reason: %v", err)
	}
	if _, err := workshop.CorrectTreadDepth(ctx, pool, technician(), wrong, 50, "Läste av fel däck"); err != nil {
		t.Fatal(err)
	}
	if _, err := workshop.CorrectTreadDepth(ctx, pool, technician(), wrong, 60, "En gång till"); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a measurement corrected twice: %v", err)
	}
	s, _ = workshop.TyreSetByID(ctx, pool, technician(), id)
	last := s.Tyres[3]
	if last.TreadTenths == nil || *last.TreadTenths != 50 || last.TreadStatus != workshop.TreadWarning && last.TreadStatus != workshop.TreadOK || len(last.Readings) != 2 ||
		last.Readings[0].Tenths != 5 || last.Readings[1].CorrectsID != wrong || last.Readings[1].Reason != "Läste av fel däck" {
		t.Errorf("the corrected tyre: %+v", last)
	}
	// The row that was wrong is still there.
	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM tyre_measurements WHERE tyre_id = $1`, last.ID).Scan(&rows); err != nil || rows != 2 {
		t.Errorf("rows kept: %d, %v", rows, err)
	}
}

// What is refused: a depth that cannot be, a tyre that has been replaced, a
// role outside the workshop, the shop's limit set by anyone but who runs it.
func TestATreadDepthThatCannotBeIsRefused(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	ctx := context.Background()
	id, _ := workshop.CreateTyreSet(ctx, pool, advisor(), summerSet(customerA, vehicleA))
	s, _ := workshop.TyreSetByID(ctx, pool, advisor(), id)
	tyre := s.Tyres[0]
	for _, bad := range []int{-1, 251} {
		if _, err := workshop.RecordTreadDepth(ctx, pool, technician(), tyre.ID, bad, ""); !errors.Is(err, workshop.ErrInvalid) {
			t.Errorf("%d tenths: %v", bad, err)
		}
	}
	if _, err := workshop.RecordTreadDepth(ctx, pool, technician(), tyre.ID, 50, strings.Repeat("x", 301)); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a long note: %v", err)
	}
	if _, err := workshop.RecordTreadDepth(ctx, pool, technician(), "nope", 50, ""); !errors.Is(err, workshop.ErrNotFound) {
		t.Errorf("a tyre that is not an id: %v", err)
	}
	if err := workshop.ReplaceTyre(ctx, pool, advisor(), tyre.ID, workshop.NewTyre{Position: tyre.Position, Dimension: "205/55R16"}); err != nil {
		t.Fatal(err)
	}
	if _, err := workshop.RecordTreadDepth(ctx, pool, technician(), tyre.ID, 50, ""); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a tyre that has been replaced: %v", err)
	}
	for _, sc := range []access.Scope{everything(rentalDesk()), everything(sales())} {
		if _, err := workshop.RecordTreadDepth(ctx, pool, sc, tyre.ID, 50, ""); !errors.Is(err, access.ErrForbidden) {
			t.Errorf("%s measured: %v", sc.Role, err)
		}
	}
	for _, sc := range []access.Scope{advisor(), technician()} {
		if err := workshop.SetTreadWarning(ctx, pool, sc, 35); !errors.Is(err, access.ErrForbidden) {
			t.Errorf("%s set the limit: %v", sc.Role, err)
		}
	}
	for _, bad := range []int{15, 101} {
		if err := workshop.SetTreadWarning(ctx, pool, owner(), bad); !errors.Is(err, workshop.ErrInvalid) {
			t.Errorf("a limit of %d: %v", bad, err)
		}
	}
	off := technician()
	off.Modules = []access.Module{access.Dealer}
	if _, err := workshop.RecordTreadDepth(ctx, pool, off, s.Tyres[1].ID, 50, ""); !errors.Is(err, access.ErrModuleOff) {
		t.Errorf("a measurement with the workshop off: %v", err)
	}
}
