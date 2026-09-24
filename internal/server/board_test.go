package server

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/RedaEkengren/FreeSMS/internal/database"
	"github.com/jackc/pgx/v5"
)

// The board exists to show who the car belongs to, which is exactly what the
// technician's screens must not.
func TestAdvisorSeesCustomerNameOnTheBoard(t *testing.T) {
	ts, _ := testServer(t)
	client := signIn(t, ts, advisorEmail)

	body := get(t, client, ts.URL+"/", http.StatusOK)
	if !strings.Contains(body, customerName) {
		t.Errorf("the board does not show the customer's name; an advisor cannot answer the telephone without it")
	}
	if !strings.Contains(body, "Not sent to the customer yet") {
		t.Errorf("the board does not say what the vehicle is waiting for:\n%s", body)
	}
}

// Same address, different screen. A bookmark keeps working when a role changes.
func TestHomeDependsOnRole(t *testing.T) {
	ts, _ := testServer(t)

	advisor := get(t, signIn(t, ts, advisorEmail), ts.URL+"/", http.StatusOK)
	if !strings.Contains(advisor, customerName) {
		t.Error("an advisor's home page is not the counter board")
	}

	tech := get(t, signIn(t, ts, techEmail), ts.URL+"/", http.StatusOK)
	if strings.Contains(tech, customerName) {
		t.Error("a technician's home page shows the customer's name")
	}
}

// Asking for the board directly must be refused for a technician, not merely
// hidden from their navigation.
func TestTechnicianIsRefusedTheBoard(t *testing.T) {
	ts, _ := testServer(t)
	body := get(t, signIn(t, ts, techEmail), ts.URL+"/board", http.StatusForbidden)
	if strings.Contains(body, customerName) {
		t.Error("the refusal page leaked the customer's name")
	}
}

func TestTechnicianCannotTakeInAVehicle(t *testing.T) {
	ts, _ := testServer(t)
	client := signIn(t, ts, techEmail)

	form := url.Values{"registration": {"NEW 001"}, "complaint": {"Knocking"}}
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/jobs/new", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", ts.URL)

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403", resp.StatusCode)
	}
}

// A car arriving with a plate nobody has seen before still has to be taken in.
func TestTakingInAnUnknownVehicleOpensAJob(t *testing.T) {
	ts, pool := testServer(t)
	client := signIn(t, ts, advisorEmail)

	form := url.Values{
		"registration": {"xyz 98 z"},
		"odometer_km":  {"18 500 km"},
		"complaint":    {"Knocking from the rear over bumps"},
	}
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/jobs/new", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", ts.URL)

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 303\n%s", resp.StatusCode, body)
	}

	// The registration is stored as typed and matched normalised, and the
	// odometer survived "18 500 km".
	var registration, normalised string
	var km int64
	err = database.InShop(context.Background(), pool, shopA, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx,
			`SELECT registration, normalised FROM vehicle_registrations WHERE normalised = 'XYZ98Z'`).
			Scan(&registration, &normalised); err != nil {
			return err
		}
		return tx.QueryRow(ctx,
			`SELECT km FROM odometer_readings ORDER BY created_at DESC LIMIT 1`).Scan(&km)
	})
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if registration != "xyz 98 z" {
		t.Errorf("registration stored as %q, want it kept as typed", registration)
	}
	if km != 18500 {
		t.Errorf("odometer = %d, want 18500", km)
	}
}

func get(t *testing.T, client *http.Client, url string, wantStatus int) string {
	t.Helper()
	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("get %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != wantStatus {
		t.Fatalf("%s returned %d, want %d\n%s", url, resp.StatusCode, wantStatus, body)
	}
	return string(body)
}
