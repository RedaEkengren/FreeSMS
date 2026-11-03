// Package vehicledata looks a registration up with whatever service a shop has
// an arrangement with.
//
// No provider ships with this project. Swedish vehicle data comes from
// Transportstyrelsen under an agreement that is the shop's to hold, not the
// software's, so what is here is the shape: an interface, a configurable HTTP
// client, and a default that does nothing.
package vehicledata

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Vehicle is the little that is wanted.
//
// Deliberately short. An endpoint that offers the registered keeper does not
// make that any of the workshop's business -- the customer is standing at the
// counter -- and pulling personal data because it is available is the pattern
// this project was started in reaction to. There is no field for an owner,
// so there is nowhere for one to go (#155).
//
// What it is -- make, model, year, engine -- is a value or nothing. What
// state it is in -- inspection, tax, a driving ban, the last odometer
// reading the register holds, the VIN -- is Known: a provider that does not
// offer a fact is not saying it is empty.
type Vehicle struct {
	Make      string
	Model     string
	ModelYear int16
	Engine    string
	VIN       Known[string]

	// The inspection as the register states it ("approved until
	// 2027-05-31", "driving ban after missed inspection").
	Inspection Known[string]
	// Vehicle tax as the register states it ("paid", "unpaid").
	Tax Known[string]
	// Whether there is a ban on driving it.
	DrivingBan Known[bool]
	// The last odometer reading the register holds, in kilometres.
	OdometerKm Known[int64]
}

// Known is a fact a provider may give or not: Provided false is "not
// provided", which is not the same as provided and empty -- a provider that
// says there is no driving ban has said something one that offers no such
// field has not.
type Known[T comparable] struct {
	Value    T
	Provided bool
}

// Of is a fact provided.
func Of[T comparable](v T) Known[T] { return Known[T]{Value: v, Provided: true} }

// Empty reports whether nothing useful came back.
func (v Vehicle) Empty() bool {
	return v.Make == "" && v.Model == "" && v.ModelYear == 0 && v.Engine == "" && v.VIN.Value == "" &&
		!v.Inspection.Provided && !v.Tax.Provided && !v.DrivingBan.Provided && !v.OdometerKm.Provided
}

// ErrNotConfigured is returned by the default lookup.
//
// Distinct from "no such vehicle": one means the shop has not set this up, the
// other means the registration is not known. Three outcomes, not two.
var ErrNotConfigured = errors.New("vehicledata: no lookup service is configured")

// ErrNotFound means the service answered and does not know that registration.
// A foreign or an unregistered vehicle is this, and it is normal.
var ErrNotFound = errors.New("vehicledata: no such registration")

// ErrRateLimited means the service, or this side's own limit, says to ask
// again later. It is a lookup failed: the person types what they know.
var ErrRateLimited = errors.New("vehicledata: the service is rate limiting us")

// Lookup is what the application depends on.
type Lookup interface {
	Lookup(ctx context.Context, registration string) (Vehicle, error)
}

// None is the default: it answers immediately that it is not configured.
type None struct{}

func (None) Lookup(context.Context, string) (Vehicle, error) {
	return Vehicle{}, ErrNotConfigured
}

// HTTPLookup calls a JSON service.
//
// Generic on purpose. A shop with an agreement points this at their endpoint
// and names the fields; nothing about one provider is baked in, and no
// credential ships with the source.
type HTTPLookup struct {
	URL    string // with {registration} where the plate goes
	Token  string // sent as a bearer token when set
	Client *http.Client

	// Field names in the response, so one provider's "fabrikat" and another's
	// "make" both work without a code change.
	Fields FieldNames
}

// FieldNames maps this system's idea of a vehicle onto a provider's JSON.
// A field left empty is not read.
type FieldNames struct {
	Make, Model, ModelYear, Engine, VIN     string
	Inspection, Tax, DrivingBan, OdometerKm string
}

// DefaultFields are the names used when none are configured.
func DefaultFields() FieldNames {
	return FieldNames{Make: "make", Model: "model", ModelYear: "model_year", Engine: "engine", VIN: "vin",
		Inspection: "inspection", Tax: "tax", DrivingBan: "driving_ban", OdometerKm: "odometer_km"}
}

// timeout is short on purpose.
//
// Somebody is standing at the counter with keys in their hand. Two seconds of
// waiting is the most this is worth, and a service that cannot answer in two
// seconds has not answered.
const timeout = 2 * time.Second

func (h HTTPLookup) Lookup(ctx context.Context, registration string) (Vehicle, error) {
	if h.URL == "" {
		return Vehicle{}, ErrNotConfigured
	}
	// Normalised before it goes into a URL, the same way the rest of the
	// system normalises a plate. A counter types "abc 12 d"; a provider
	// expects ABC12D, and sending the spaces produces a 404 that reads as
	// "no such vehicle".
	registration = normalise(registration)
	if registration == "" {
		return Vehicle{}, ErrNotFound
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	endpoint := strings.ReplaceAll(h.URL, "{registration}", url.PathEscape(registration))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return Vehicle{}, fmt.Errorf("vehicledata: build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if h.Token != "" {
		req.Header.Set("Authorization", "Bearer "+h.Token)
	}

	client := h.Client
	if client == nil {
		client = &http.Client{Timeout: timeout}
	}
	resp, err := client.Do(req)
	if err != nil {
		return Vehicle{}, fmt.Errorf("vehicledata: %w", err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusNotFound:
		return Vehicle{}, ErrNotFound
	case resp.StatusCode == http.StatusTooManyRequests:
		// Worth naming: a shop pasting a list must learn it has been throttled
		// rather than seeing every lookup fail for no stated reason.
		return Vehicle{}, ErrRateLimited
	case resp.StatusCode != http.StatusOK:
		return Vehicle{}, fmt.Errorf("vehicledata: the service answered %s", resp.Status)
	}

	var raw map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return Vehicle{}, fmt.Errorf("vehicledata: unreadable answer: %w", err)
	}

	fields := h.Fields
	if fields == (FieldNames{}) {
		fields = DefaultFields()
	}

	v := Vehicle{
		Make:       text(raw[fields.Make]),
		Model:      text(raw[fields.Model]),
		Engine:     text(raw[fields.Engine]),
		VIN:        knownText(raw, fields.VIN),
		Inspection: knownText(raw, fields.Inspection),
		Tax:        knownText(raw, fields.Tax),
	}
	if year, ok := raw[fields.ModelYear].(float64); ok && year > 1885 && year < 2100 {
		v.ModelYear = int16(year)
	}
	if b, ok := raw[fields.DrivingBan].(bool); ok && fields.DrivingBan != "" {
		v.DrivingBan = Of(b)
	}
	if km, ok := raw[fields.OdometerKm].(float64); ok && fields.OdometerKm != "" && km >= 0 && km < 10_000_000 {
		v.OdometerKm = Of(int64(km))
	}
	if v.Empty() {
		return Vehicle{}, ErrNotFound
	}
	return v, nil
}

// normalise strips what people vary and services do not accept.
func normalise(s string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(s) {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func text(v any) string {
	s, _ := v.(string)
	return strings.TrimSpace(s)
}

// knownText is a text field: provided when the answer has it at all, as a
// string or as null (the provider saying there is none).
func knownText(raw map[string]any, name string) Known[string] {
	if name == "" {
		return Known[string]{}
	}
	v, ok := raw[name]
	if !ok {
		return Known[string]{}
	}
	if _, isText := v.(string); !isText && v != nil {
		return Known[string]{}
	}
	return Of(text(v))
}

// Limited asks its provider no faster than the shop's agreement allows: a
// bucket of calls refilled at PerMinute, and after the provider says it is
// rate limiting, no call at all for a back-off that doubles each time it
// says so again, up to a quarter of an hour. A call it holds back fails at
// once with ErrRateLimited rather than waiting: somebody at the counter
// types what they know, and a list of lookups goes through the outbox,
// which retries later (#155).
type Limited struct {
	Inner     Lookup
	PerMinute int

	mu      sync.Mutex
	tokens  float64
	last    time.Time
	until   time.Time
	backoff time.Duration
	now     func() time.Time
}

// NewLimited wraps l. perMinute below one is one.
func NewLimited(l Lookup, perMinute int) *Limited {
	return &Limited{Inner: l, PerMinute: max(perMinute, 1)}
}

const (
	firstBackoff = 30 * time.Second
	maxBackoff   = 15 * time.Minute
)

// Lookup asks the provider, if the limit allows.
func (l *Limited) Lookup(ctx context.Context, registration string) (Vehicle, error) {
	if !l.take() {
		return Vehicle{}, ErrRateLimited
	}
	v, err := l.Inner.Lookup(ctx, registration)
	l.mu.Lock()
	defer l.mu.Unlock()
	switch {
	case errors.Is(err, ErrRateLimited):
		l.backoff = min(max(firstBackoff, 2*l.backoff), maxBackoff)
		l.until = l.clock().Add(l.backoff)
	case err == nil || errors.Is(err, ErrNotFound):
		l.backoff = 0
	}
	return v, err
}

func (l *Limited) clock() time.Time {
	if l.now != nil {
		return l.now()
	}
	return time.Now()
}

// take spends a call from the bucket, refilled since the last.
func (l *Limited) take() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.clock()
	if now.Before(l.until) {
		return false
	}
	capacity := float64(max(l.PerMinute/6, 1))
	if l.last.IsZero() {
		l.tokens = capacity
	} else {
		l.tokens = min(capacity, l.tokens+now.Sub(l.last).Minutes()*float64(l.PerMinute))
	}
	l.last = now
	if l.tokens < 1 {
		return false
	}
	l.tokens--
	return true
}
