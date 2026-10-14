package server

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/RedaEkengren/FreeSMS/internal/database"
	"github.com/jackc/pgx/v5"
)

// Booked at the counter, seen on the week, taken in when the car comes --
// and the booking then points at the job it became.
func TestABookingIsTakenInFromThePlanner(t *testing.T) {
	ts, _ := testServer(t)
	desk := signIn(t, ts, advisorEmail)
	post := func(path string, form url.Values) (int, string) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, ts.URL+path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", ts.URL)
		client := *desk
		client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode, resp.Header.Get("Location")
	}
	get := func(path string) string {
		t.Helper()
		resp, err := desk.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s answered %d", path, resp.StatusCode)
		}
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}

	status, where := post("/calendar", url.Values{"date": {"2026-10-15"}, "time": {"09:00"}, "minutes": {"90"},
		"registration": {"BKG 123"}, "what": {"Service och bromsar"}, "customer_name": {"Bo Kund"}})
	if status != http.StatusSeeOther || !strings.HasPrefix(where, "/bookings/") {
		t.Fatalf("booking answered %d, %q", status, where)
	}
	if body := get("/calendar?week=2026-10-15"); !strings.Contains(body, "BKG 123") || !strings.Contains(body, "cs-4 cd-3") {
		t.Error("the week does not show the booking at 09:00 for an hour and a half")
	}
	if body := get("/calendar?q=bkg123"); !strings.Contains(body, where) {
		t.Error("searching the registration does not find the booking")
	}

	// The car comes: the intake is filled in from the booking.
	id := strings.TrimPrefix(where, "/bookings/")
	intake := get("/jobs/new?booking=" + id)
	if !strings.Contains(intake, `value="BKG 123"`) || !strings.Contains(intake, `name="booking_id" value="`+id+`"`) {
		t.Fatal("taking it in does not carry the booking's car and id")
	}
	status, job := post("/jobs/new", url.Values{"registration": {"BKG 123"}, "complaint": {"Service och bromsar"}, "booking_id": {id}})
	if status != http.StatusSeeOther || !regexp.MustCompile(`^/jobs/[0-9a-f-]{36}$`).MatchString(job) {
		t.Fatalf("taking it in answered %d, %q", status, job)
	}
	if body := get(where); !strings.Contains(body, `href="`+job+`"`) {
		t.Error("the booking does not point at the job it became")
	}
}

// A shut day and the lifts are the owner's to set; the front desk sees them
// on the week it books into, and a technician cannot set either.
func TestCapacityIsSetAndShownOnThePlanner(t *testing.T) {
	ts, _ := testServer(t)
	post := func(c *http.Client, form url.Values) int {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/calendar/capacity", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", ts.URL)
		client := *c
		client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	owner, desk, tech := signIn(t, ts, ownerEmail), signIn(t, ts, advisorEmail), signIn(t, ts, techEmail)
	shut := url.Values{"action": {"shut"}, "date": {"2026-10-16"}, "reason": {"Inventering"}}
	if status := post(tech, shut); status != http.StatusForbidden {
		t.Errorf("a technician shutting the shop answered %d", status)
	}
	if status := post(desk, shut); status != http.StatusForbidden {
		t.Errorf("the front desk shutting the shop answered %d", status)
	}
	if status := post(owner, shut); status != http.StatusSeeOther {
		t.Fatalf("the owner shutting a day answered %d", status)
	}
	if status := post(owner, url.Values{"action": {"lifts"}, "lifts": {"two"}}); status != http.StatusBadRequest {
		t.Errorf("lifts as a word answered %d", status)
	}
	if status := post(owner, url.Values{"action": {"lifts"}, "lifts": {"2"}}); status != http.StatusSeeOther {
		t.Errorf("setting the lifts answered %d", status)
	}

	resp, err := desk.Get(ts.URL + "/calendar?week=2026-10-16")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "Inventering") {
		t.Error("the front desk's week does not say the shop is shut")
	}
	if strings.Contains(string(body), `value="shut"`) {
		t.Error("the front desk is offered a form only the owner may use")
	}
}

// The same booking sent twice -- two tabs, or the offline queue replaying a
// submission whose answer was lost -- lands on the same job. A booking
// cancelled while the form was open comes back as a walk-in to decide on.
func TestTakingABookingInTwiceGoesToTheSameJob(t *testing.T) {
	ts, pool := testServer(t)
	desk := signIn(t, ts, advisorEmail)
	post := func(form url.Values) (int, string) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/jobs/new", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", ts.URL)
		client := *desk
		client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if loc := resp.Header.Get("Location"); loc != "" {
			return resp.StatusCode, loc
		}
		return resp.StatusCode, string(b)
	}
	ctx := context.Background()
	newBooking := func() string {
		t.Helper()
		var id string
		if err := database.InShop(ctx, pool, shopA, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `
				INSERT INTO bookings (shop_id, starts_at, ends_at, registration, what, created_by)
				VALUES ($1, now(), now() + interval '1 hour', 'BKG 123', 'Service', $2) RETURNING id`,
				shopA, advisorUser).Scan(&id)
		}); err != nil {
			t.Fatal(err)
		}
		return id
	}

	id := newBooking()
	form := url.Values{"registration": {"BKG 123"}, "complaint": {"Service"}, "booking_id": {id}}
	s1, first := post(form)
	s2, second := post(form)
	if s1 != http.StatusSeeOther || s2 != http.StatusSeeOther || first != second {
		t.Errorf("two submissions answered %d %q and %d %q; want the same job", s1, first, s2, second)
	}
	var jobs int
	database.InShop(ctx, pool, shopA, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM work_orders WHERE complaint = 'Service'`).Scan(&jobs)
	})
	if jobs != 1 {
		t.Errorf("%d jobs from one booking", jobs)
	}

	cancelled := newBooking()
	database.InShop(ctx, pool, shopA, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE bookings SET status = 'cancelled' WHERE id = $1`, cancelled)
		return err
	})
	form.Set("booking_id", cancelled)
	status, body := post(form)
	if status != http.StatusConflict || !strings.Contains(body, "cancelled") || strings.Contains(body, `name="booking_id"`) {
		t.Errorf("a cancelled booking answered %d:\n%s", status, firstLines(body, 30))
	}
	form.Set("booking_id", "not-a-booking")
	if status, _ := post(form); status != http.StatusConflict {
		t.Errorf("a malformed booking answered %d", status)
	}
}

// The intake of a booked car shows what the booking knew and asks the desk
// to confirm it; what is confirmed is on the job straight away.
func TestTheIntakeOfABookingAsksWhoPaysAndWhoDoesTheWork(t *testing.T) {
	ts, pool := testServer(t)
	desk := signIn(t, ts, advisorEmail)
	ctx := context.Background()
	const tech = "aaaaaaaa-0000-0000-0000-000000000002"
	var booking, sameNumber, sameName string
	if err := database.InShop(ctx, pool, shopA, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `
			INSERT INTO bookings (shop_id, starts_at, ends_at, registration, customer_name, customer_phone, technician_id, what, created_by)
			VALUES ($1, now(), now() + interval '1 hour', 'NEW 777', 'Greta Gäst', '070-777 66 55', $2, 'Service', $3) RETURNING id`,
			shopA, tech, advisorUser).Scan(&booking); err != nil {
			return err
		}
		for _, c := range []struct {
			name, phone string
			id          *string
		}{{"Greta Telefon", "070-777 66 55", &sameNumber}, {"Greta Gäst", "070-000 11 22", &sameName}} {
			var person string
			if err := tx.QueryRow(ctx, `INSERT INTO people (shop_id, display_name, phone) VALUES ($1, $2, $3) RETURNING id`,
				shopA, c.name, c.phone).Scan(&person); err != nil {
				return err
			}
			if err := tx.QueryRow(ctx, `INSERT INTO customers (shop_id, kind, person_id) VALUES ($1, 'private', $2) RETURNING id`,
				shopA, person).Scan(c.id); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	_, form := page(t, desk, ts.URL+"/jobs/new?booking="+booking)
	for _, want := range []string{"Greta Gäst", "070-777 66 55", `value="new"`,
		`value="existing:` + sameNumber + `"`, `value="existing:` + sameName + `"`, "same number", "similar name",
		`<option value="` + tech + `" selected>`} {
		if !strings.Contains(form, want) {
			t.Errorf("the intake form lacks %q", want)
		}
	}
	if strings.Contains(form, `value="new" checked`) || strings.Contains(form, `existing:`+sameNumber+`" checked`) {
		t.Error("a customer was chosen for the desk")
	}

	status, job := func() (int, string) {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/jobs/new", strings.NewReader(url.Values{
			"registration": {"NEW 777"}, "complaint": {"Service"}, "booking_id": {booking},
			"customer": {"existing:" + sameNumber}, "technician_id": {tech}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", ts.URL)
		client := *desk
		client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode, resp.Header.Get("Location")
	}()
	if status != http.StatusSeeOther {
		t.Fatalf("taking it in answered %d", status)
	}
	if _, body := page(t, desk, ts.URL+job); !strings.Contains(body, "Greta Telefon") {
		t.Error("the job does not show the customer who was confirmed")
	}
	var assigned string
	database.InShop(ctx, pool, shopA, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT assigned_to::text FROM work_orders WHERE id = $1`, strings.TrimPrefix(job, "/jobs/")).Scan(&assigned)
	})
	if assigned != tech {
		t.Errorf("the job is assigned to %q, want the booked technician", assigned)
	}
}
