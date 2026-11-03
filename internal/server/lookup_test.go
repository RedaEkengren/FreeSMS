package server

import (
	"context"
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
