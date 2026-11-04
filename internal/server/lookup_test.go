package server

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/RedaEkengren/FreeSMS/internal/vehicledata"
)

type fakeLookup struct{ v vehicledata.Vehicle }

func (f fakeLookup) Lookup(context.Context, string) (vehicledata.Vehicle, error) { return f.v, nil }

// The intake form offers a lookup only when the shop has a provider. With
// one, a lookup fills in what is empty and keeps what was typed, says where
// it came from and when, offers to look again, says what the provider does
// not give, and shows a VIN unlike the typed one as a conflict -- never
// writing it over.
func TestALookupAtIntakeFillsOnlyWhatIsEmpty(t *testing.T) {
	ts, _ := testServer(t)
	desk := signIn(t, ts, advisorEmail)
	if _, form := page(t, desk, ts.URL+"/jobs/new"); strings.Contains(form, "Look the registration up") {
		t.Error("the form offers a lookup with no provider")
	}

	v, _ := testServers.Load(ts.URL)
	srv := v.(*Server)
	srv.vehicleLookup = fakeLookup{v: vehicledata.Vehicle{Make: "Polestar", Model: "2", ModelYear: 2022,
		VIN: vehicledata.Of("LPSVSEDEEML000001"), DrivingBan: vehicledata.Of(false)}}
	srv.lookupProvider = "Bilregistret"
	if _, form := page(t, desk, ts.URL+"/jobs/new"); !strings.Contains(form, "Look the registration up") {
		t.Error("the form offers no lookup with a provider")
	}

	q := url.Values{"registration": {"abc 12 d"}, "make": {"Volvo"}, "vin": {"YV1SW58D"}}
	_, frag := page(t, desk, ts.URL+"/jobs/new/lookup?"+q.Encode())
	for _, want := range []string{`name="make" value="Volvo"`, `name="model" value="2"`, `value="2022"`,
		`name="vin" value="YV1SW58D"`, "From Bilregistret", "Look it up again", "LPSVSEDEEML000001", "not provided by the service"} {
		if !strings.Contains(frag, want) {
			t.Errorf("the lookup's answer lacks %q:\n%s", want, frag)
		}
	}
	if strings.Contains(frag, `value="Polestar"`) {
		t.Error("a lookup wrote over the make that was typed")
	}
}

// A car being bought and a vehicle being added to the rental fleet are
// looked up the same way (#155): the form offers it only with a provider,
// the answer fills in only what is empty and says what the provider does
// not give, each lookup is recorded for its own purpose, and a role that
// does not buy cars or keep the fleet is refused.
func TestAPurchaseAndARentalUnitAreLookedUpTheSameWay(t *testing.T) {
	ts, pool := testServer(t)
	dealerShop(t, pool)
	seedRentalDesk(t, func(t *testing.T, stmts []string) { seedShop(t, pool, shopA, stmts) })
	buyer, desk := signIn(t, ts, buyerEmail), signIn(t, ts, rentalEmail)
	if _, form := page(t, buyer, ts.URL+"/cars/stock/new"); strings.Contains(form, "Look the registration up") {
		t.Error("the purchase form offers a lookup with no provider")
	}

	v, _ := testServers.Load(ts.URL)
	srv := v.(*Server)
	srv.vehicleLookup = fakeLookup{v: vehicledata.Vehicle{Make: "Volvo", Model: "V70", ModelYear: 2012,
		VIN: vehicledata.Of("YV1SW61R021234567"), DrivingBan: vehicledata.Of(true), Inspection: vehicledata.Of("2026-05-31")}}
	srv.lookupProvider = "Bilregistret"

	if _, form := page(t, buyer, ts.URL+"/cars/stock/new"); !strings.Contains(form, "/cars/stock/new/lookup") {
		t.Error("the purchase form offers no lookup with a provider")
	}
	q := url.Values{"registration": {"ABC 123"}, "model": {"V70 Cross Country"}}
	status, frag := page(t, buyer, ts.URL+"/cars/stock/new/lookup?"+q.Encode())
	for _, want := range []string{`name="make" value="Volvo"`, `name="model" value="V70 Cross Country"`, `value="YV1SW61R021234567"`,
		"2026-05-31", "From Bilregistret", "/cars/stock/new/lookup?refresh=1", "not provided by the service"} {
		if !strings.Contains(frag, want) {
			t.Errorf("the purchase lookup lacks %q (%d):\n%s", want, status, frag)
		}
	}

	if _, units := page(t, desk, ts.URL+"/rental"); !strings.Contains(units, "/rental/units/lookup") {
		t.Error("adding a rental unit offers no lookup")
	}
	if status, frag := page(t, desk, ts.URL+"/rental/units/lookup?registration=XYZ+789"); status != http.StatusOK ||
		!strings.Contains(frag, "Volvo V70") || !strings.Contains(frag, "/rental/units/lookup?refresh=1") {
		t.Errorf("the rental unit lookup: %d\n%s", status, frag)
	}

	// Each recorded for its own reason.
	var purposes []string
	if err := pool.QueryRow(context.Background(), `SELECT array_agg(purpose ORDER BY looked_up_at) FROM vehicle_lookups`).Scan(&purposes); err != nil {
		t.Fatal(err)
	}
	if len(purposes) != 2 || purposes[0] != "purchase" || purposes[1] != "rental_unit" {
		t.Errorf("the lookups recorded: %v", purposes)
	}
	// The rental desk does not buy cars, and a technician keeps no fleet.
	if status, _ := page(t, desk, ts.URL+"/cars/stock/new/lookup?registration=ABC+123"); status != http.StatusForbidden {
		t.Errorf("the rental desk looked up a purchase: %d", status)
	}
	if status, _ := page(t, signIn(t, ts, techEmail), ts.URL+"/rental/units/lookup?registration=XYZ+789"); status != http.StatusForbidden {
		t.Errorf("a technician looked up a rental unit: %d", status)
	}
}
