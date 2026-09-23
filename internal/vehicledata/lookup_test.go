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
