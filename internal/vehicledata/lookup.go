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
	"time"
)

// Vehicle is the little that is wanted.
//
// Deliberately short. An endpoint that offers the registered keeper does not
// make that any of the workshop's business -- the customer is standing at the
// counter -- and pulling personal data because it is available is the pattern
// this project was started in reaction to.
type Vehicle struct {
	Make      string
	Model     string
	ModelYear int16
	Engine    string
	VIN       string
}

// Empty reports whether nothing useful came back.
func (v Vehicle) Empty() bool {
	return v.Make == "" && v.Model == "" && v.ModelYear == 0 && v.Engine == "" && v.VIN == ""
}

// ErrNotConfigured is returned by the default lookup.
//
// Distinct from "no such vehicle": one means the shop has not set this up, the
// other means the registration is not known. Three outcomes, not two.
var ErrNotConfigured = errors.New("vehicledata: no lookup service is configured")

// ErrNotFound means the service answered and does not know that registration.
var ErrNotFound = errors.New("vehicledata: no such registration")

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
type FieldNames struct {
	Make, Model, ModelYear, Engine, VIN string
}

// DefaultFields are the names used when none are configured.
func DefaultFields() FieldNames {
	return FieldNames{Make: "make", Model: "model", ModelYear: "model_year", Engine: "engine", VIN: "vin"}
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
		return Vehicle{}, fmt.Errorf("vehicledata: the service is rate limiting us")
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
		Make:   text(raw[fields.Make]),
		Model:  text(raw[fields.Model]),
		Engine: text(raw[fields.Engine]),
		VIN:    text(raw[fields.VIN]),
	}
	if year, ok := raw[fields.ModelYear].(float64); ok && year > 1885 && year < 2100 {
		v.ModelYear = int16(year)
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
