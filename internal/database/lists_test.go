package database

import (
	"context"
	"testing"
)

// After every migration, the lists car sales widened (0095, 0096) still
// refuse a value nobody added -- widening a list must never leave it
// allowing everything -- and allow the values added.
func TestTheWidenedListsStillRefuseAnUnknownValue(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	seed(t, pool)
	event := func(kind string) error {
		_, err := pool.Exec(ctx, `
			INSERT INTO audit_events (shop_id, subject_kind, subject_id, event, reason)
			VALUES ($1, $2, gen_random_uuid(), 'x.y', 'r')`, shopID, kind)
		return err
	}
	reading := func(source string) error {
		_, err := pool.Exec(ctx, `
			INSERT INTO odometer_readings (shop_id, vehicle_id, km, source) SELECT $1, id, 1000, $2 FROM vehicles LIMIT 1`, shopID, source)
		return err
	}
	for _, kind := range []string{"stock_unit", "purchase", "work_order"} {
		if err := event(kind); err != nil {
			t.Errorf("an audit event about a %s: %v", kind, err)
		}
	}
	if err := event("no_such_subject"); err == nil {
		t.Error("an audit event about an unknown subject was written")
	}
	for _, source := range []string{"purchase", "drop_off"} {
		if err := reading(source); err != nil {
			t.Errorf("an odometer reading from %s: %v", source, err)
		}
	}
	if err := reading("no_such_source"); err == nil {
		t.Error("an odometer reading from an unknown source was written")
	}
}
