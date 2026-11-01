package vehicledata

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestTheDefaultSaysItIsNotConfigured(t *testing.T) {
	// Distinct from "no such vehicle". One means the shop has not set this up,
	// the other means the plate is unknown -- three outcomes, not two.
	if _, err := (None{}).Lookup(context.Background(), "ABC12D"); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("None.Lookup = %v, want ErrNotConfigured", err)
	}
}

func TestLookupReadsAProvidersFields(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/vehicle/ABC12D" {
			t.Errorf("asked for %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer secret" {
			t.Errorf("Authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"fabrikat":"Volvo","modell":"V70","arsmodell":2008,"motor":"D5 2.4"}`))
	}))
	defer srv.Close()

	l := HTTPLookup{
		URL:   srv.URL + "/vehicle/{registration}",
		Token: "secret",
		Fields: FieldNames{
			Make: "fabrikat", Model: "modell", ModelYear: "arsmodell", Engine: "motor",
		},
	}
	got, err := l.Lookup(context.Background(), "abc 12 d")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if got.Make != "Volvo" || got.Model != "V70" || got.ModelYear != 2008 || got.Engine != "D5 2.4" {
		t.Errorf("got %+v", got)
	}
}

// An endpoint offering the registered keeper does not make that the
// workshop's business. Nothing outside the named fields is read.
func TestNothingBeyondTheNamedFieldsIsRead(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"make":"Volvo","owner_name":"Margareta Öberg","owner_personal_id":"19800101-1234"}`))
	}))
	defer srv.Close()

	got, err := HTTPLookup{URL: srv.URL + "/{registration}"}.Lookup(context.Background(), "ABC12D")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if got.Make != "Volvo" {
		t.Errorf("make = %q", got.Make)
	}
	// The Vehicle type has nowhere to put an owner, which is the point: there
	// is no field to leak into.
	if got != (Vehicle{Make: "Volvo"}) {
		t.Errorf("something other than the make was taken: %+v", got)
	}
}

func TestOutcomesAreDistinguishable(t *testing.T) {
	notFound := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer notFound.Close()
	if _, err := (HTTPLookup{URL: notFound.URL + "/{registration}"}).Lookup(context.Background(), "ABC12D"); !errors.Is(err, ErrNotFound) {
		t.Errorf("a 404 = %v, want ErrNotFound", err)
	}

	throttled := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer throttled.Close()
	_, err := (HTTPLookup{URL: throttled.URL + "/{registration}"}).Lookup(context.Background(), "ABC12D")
	if err == nil || errors.Is(err, ErrNotFound) {
		t.Errorf("being rate limited = %v, want something that is not ErrNotFound", err)
	}

	// An empty body is not a vehicle.
	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{}`))
	}))
	defer empty.Close()
	if _, err := (HTTPLookup{URL: empty.URL + "/{registration}"}).Lookup(context.Background(), "ABC12D"); !errors.Is(err, ErrNotFound) {
		t.Errorf("an empty answer = %v, want ErrNotFound", err)
	}
}

// Somebody is standing at the counter with keys in their hand.
func TestASlowServiceGivesUp(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(5 * time.Second)
	}))
	defer slow.Close()

	start := time.Now()
	_, err := (HTTPLookup{URL: slow.URL + "/{registration}"}).Lookup(context.Background(), "ABC12D")
	if err == nil {
		t.Fatal("a service that never answered was treated as success")
	}
	if elapsed := time.Since(start); elapsed > 4*time.Second {
		t.Errorf("waited %v for a service that was never going to answer", elapsed)
	}
}

func TestAnUnconfiguredHTTPLookupSaysSo(t *testing.T) {
	if _, err := (HTTPLookup{}).Lookup(context.Background(), "ABC12D"); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("Lookup with no URL = %v, want ErrNotConfigured", err)
	}
}

// The state a register holds -- inspection, tax, a driving ban, the last
// odometer reading, the VIN -- is read when the provider offers it, and a
// fact it does not offer stays "not provided", which is not the same as
// provided and empty.
func TestAStatusFieldIsProvidedOrNot(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"make":"Volvo","vin":"YV1SW58D","inspection":"approved until 2027-05-31","driving_ban":false,"odometer_km":184500,"tax":null}`))
	}))
	defer srv.Close()
	got, err := HTTPLookup{URL: srv.URL + "/{registration}", Fields: DefaultFields()}.Lookup(context.Background(), "ABC12D")
	if err != nil {
		t.Fatal(err)
	}
	want := Vehicle{Make: "Volvo", VIN: Of("YV1SW58D"), Inspection: Of("approved until 2027-05-31"),
		DrivingBan: Of(false), OdometerKm: Of(int64(184500)), Tax: Of("")}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
	only := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"make":"Volvo"}`))
	}))
	defer only.Close()
	got, err = HTTPLookup{URL: only.URL + "/{registration}"}.Lookup(context.Background(), "ABC12D")
	if err != nil {
		t.Fatal(err)
	}
	if got.DrivingBan.Provided || got.Tax.Provided || got.Inspection.Provided || got.OdometerKm.Provided || got.VIN.Provided {
		t.Errorf("facts the provider never offered read as provided: %+v", got)
	}
}

type countingLookup struct {
	calls int
	err   error
}

func (c *countingLookup) Lookup(context.Context, string) (Vehicle, error) {
	c.calls++
	return Vehicle{Make: "Volvo"}, c.err
}

// The provider is asked no faster than the limit, and not at all for a
// while after it says it is rate limiting; a call held back fails at once
// as rate limited, never waits, and never reaches the provider.
func TestALimitedLookupHoldsBackAndBacksOff(t *testing.T) {
	inner := &countingLookup{}
	l := NewLimited(inner, 12) // two at once, then one each five seconds
	now := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	l.now = func() time.Time { return now }
	for i := range 2 {
		if _, err := l.Lookup(context.Background(), "ABC12D"); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if _, err := l.Lookup(context.Background(), "ABC12D"); !errors.Is(err, ErrRateLimited) || inner.calls != 2 {
		t.Errorf("the third at once: %v, provider asked %d times", err, inner.calls)
	}
	now = now.Add(5 * time.Second)
	if _, err := l.Lookup(context.Background(), "ABC12D"); err != nil {
		t.Errorf("after the bucket refilled: %v", err)
	}
	inner.err = ErrRateLimited
	now = now.Add(time.Minute)
	if _, err := l.Lookup(context.Background(), "ABC12D"); !errors.Is(err, ErrRateLimited) {
		t.Fatal(err)
	}
	asked := inner.calls
	now = now.Add(29 * time.Second)
	if _, err := l.Lookup(context.Background(), "ABC12D"); !errors.Is(err, ErrRateLimited) || inner.calls != asked {
		t.Errorf("asked during the back-off: %v, %d calls", err, inner.calls)
	}
	now = now.Add(2 * time.Second)
	if _, err := l.Lookup(context.Background(), "ABC12D"); !errors.Is(err, ErrRateLimited) || inner.calls != asked+1 {
		t.Errorf("after the back-off: %v, %d calls", err, inner.calls)
	}
	// Said again: the back-off doubles.
	now = now.Add(31 * time.Second)
	if _, err := l.Lookup(context.Background(), "ABC12D"); inner.calls != asked+1 {
		t.Errorf("the back-off did not double: %v, %d calls", err, inner.calls)
	}
}
