package server

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

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

// A vehicle nobody has seen before is taken in with no customer, because
// migration 0005 made the column nullable rather than push the front desk
// into inventing one. It must still be on the board. The whole existing suite
// missed an inner join here for weeks because every other fixture seeds a
// customer, so this test opens the job the way the front desk does.
func TestAVehicleWithNoCustomerIsStillOnTheBoard(t *testing.T) {
	ts, pool := testServer(t)
	client := signIn(t, ts, advisorEmail)

	form := url.Values{
		"registration": {"NOC 001"},
		"complaint":    {"Will not start"},
	}
	post(t, client, ts.URL+"/jobs/new", form)

	// Nothing linked a customer, which is the case under test.
	var customers int
	if err := database.InShop(context.Background(), pool, shopA, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT count(*) FROM work_orders w
			  JOIN vehicle_registrations r ON r.vehicle_id = w.vehicle_id
			 WHERE r.normalised = 'NOC001' AND w.customer_id IS NOT NULL`).Scan(&customers)
	}); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if customers != 0 {
		t.Fatalf("the fixture gave the job a customer, so it does not test anything")
	}

	body := get(t, client, ts.URL+"/board", http.StatusOK)
	if !strings.Contains(body, "NOC 001") {
		t.Errorf("a vehicle with no customer is missing from the board; it is on the ramp and the counter cannot see it:\n%s", body)
	}
	if !strings.Contains(body, "No customer recorded yet") {
		t.Errorf("the board does not say the customer is missing, so nobody will fill it in before invoicing:\n%s", body)
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

// The customer has no account, no setting and no way to ask for another
// language, so the shop's is the only honest signal.
func TestTheCustomersPageFollowsTheShopsLanguage(t *testing.T) {
	ts, pool := testServer(t)
	ctx := context.Background()

	// Build an inspection with a finding and a link to it.
	advisor := signIn(t, ts, advisorEmail)
	form := url.Values{"template_id": {seedTemplate(t, pool)}}
	req, _ := http.NewRequest(http.MethodPost,
		ts.URL+"/jobs/aaaaaaaa-0000-0000-0000-00000000000f/inspect", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", ts.URL)
	resp, err := advisor.Do(req)
	if err != nil {
		t.Fatalf("start inspection: %v", err)
	}
	resp.Body.Close()
	inspection := strings.TrimPrefix(resp.Header.Get("Location"), "/inspections/")
	if inspection == "" {
		t.Fatalf("no inspection was started: %d", resp.StatusCode)
	}

	// A finding, or there is nothing for the customer to decide and the page
	// correctly shows no buttons at all.
	var itemID string
	if err := database.InShop(ctx, pool, shopA, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			UPDATE inspection_items SET status = 'fail', note = 'Belägg nere på plåten'
			WHERE inspection_id = $1 RETURNING id`, inspection).Scan(&itemID)
	}); err != nil {
		t.Fatalf("mark the item: %v", err)
	}

	// The shop does business in Swedish.
	if err := database.InShop(ctx, pool, shopA, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE shops SET locale = 'sv' WHERE id = $1`, shopA)
		return err
	}); err != nil {
		t.Fatalf("set the shop's language: %v", err)
	}

	share := post(t, advisor, ts.URL+"/inspections/"+inspection+"/share", url.Values{})
	// The page prints the link using BASE_URL, which is not where the test
	// server is listening; only the path matters here.
	body := get(t, &http.Client{}, ts.URL+sharePath(t, share), http.StatusOK)
	if !strings.Contains(body, `lang="sv"`) {
		t.Error("the customer's page does not declare Swedish")
	}
	if !strings.Contains(body, "Ja, gör det") {
		t.Errorf("the page is not in Swedish:\n%s", firstLines(body, 40))
	}
	if strings.Contains(body, "Yes, do it") {
		t.Error("the page still carries the English button")
	}
}

// A link that no longer works is refused in the shop's language too.
func TestAClosedLinkIsRefusedInTheShopsLanguage(t *testing.T) {
	ts, pool := testServer(t)
	if err := database.InShop(context.Background(), pool, shopA, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE shops SET locale = 'sv' WHERE id = $1`, shopA)
		return err
	}); err != nil {
		t.Fatalf("set the shop's language: %v", err)
	}

	body := get(t, &http.Client{}, ts.URL+"/i/not-a-real-token", http.StatusNotFound)
	if !strings.Contains(body, "Be verkstaden om en ny") {
		t.Errorf("the refusal is not in Swedish:\n%s", firstLines(body, 30))
	}
}

func seedTemplate(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var id string
	err := database.InShop(context.Background(), pool, shopA, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx,
			`INSERT INTO inspection_templates (shop_id, name) VALUES ($1, 'Service check') RETURNING id`,
			shopA).Scan(&id); err != nil {
			return err
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO inspection_template_items (shop_id, template_id, position, label)
			 VALUES ($1, $2, 1, 'Front brakes')`, shopA, id)
		return err
	})
	if err != nil {
		t.Fatalf("seed template: %v", err)
	}
	return id
}

func post(t *testing.T, client *http.Client, url string, form url.Values) string {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://"+req.URL.Host)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("post %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return string(body)
}

// sharePath pulls the customer link's path off the page it was shown on.
func sharePath(t *testing.T, page string) string {
	t.Helper()
	i := strings.Index(page, "/i/")
	if i < 0 {
		t.Fatalf("no link on the page:\n%s", firstLines(page, 40))
	}
	rest := page[i:]
	end := strings.IndexAny(rest, `"< `)
	if end < 0 {
		t.Fatal("the link does not end")
	}
	return rest[:end]
}

func firstLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}

// jobA is the work order shop A's fixture opens.
const jobA = "aaaaaaaa-0000-0000-0000-00000000000f"

// visibleText strips the markup, so that a hidden input carrying a state as a
// form value is not mistaken for a state shown to a person.
func visibleText(html string) string {
	var out strings.Builder
	depth := 0
	for _, r := range html {
		switch {
		case r == '<':
			depth++
		case r == '>':
			if depth > 0 {
				depth--
			}
		case depth == 0:
			out.WriteRune(r)
		}
	}
	return out.String()
}

// The state machine's identifiers are for the column and the code. A person
// reading a screen sees words, and an underscore in visible text is the tell
// that one got out. This caught in_progress on buttons, approved in a status
// line and AWAITING_APPROVAL in a badge, all at once.
func TestNoScreenShowsAStateIdentifier(t *testing.T) {
	ts, _ := testServer(t)
	advisor := signIn(t, ts, advisorEmail)
	tech := signIn(t, ts, techEmail)

	// Walk the order through the states that have the most identifiers in
	// reach: buttons for what is next, a badge for where it is now.
	for _, state := range []string{"estimated", "awaiting_approval", "approved", "in_progress"} {
		post(t, advisor, ts.URL+"/jobs/"+jobA+"/state", url.Values{"state": {state}})
	}

	pages := map[string]*http.Client{
		"/":                 advisor,
		"/board":            advisor,
		"/jobs/" + jobA:     advisor,
		"/vehicles/aaaaaaaa-0000-0000-0000-000000000005": advisor,
	}
	for path, client := range pages {
		body := visibleText(get(t, client, ts.URL+path, http.StatusOK))
		for _, id := range []string{
			"awaiting_approval", "in_progress", "awaiting_parts",
			"AWAITING_APPROVAL", "IN_PROGRESS", "AWAITING_PARTS",
		} {
			if strings.Contains(body, id) {
				t.Errorf("%s shows the identifier %q to a person", path, id)
			}
		}
	}

	// The technician's own list too, which has its own badge.
	body := visibleText(get(t, tech, ts.URL+"/", http.StatusOK))
	if strings.Contains(body, "in_progress") {
		t.Error("the technician's list shows a state identifier")
	}
}

// A form value is posted back and parsed. Locale formatting in one -- a
// non-breaking space between thousands, a comma for the decimal -- returns as
// a parse error, so the machine form and the display form have to stay
// separate and the templates have to pick the right one.
func TestFormValuesStayMachineReadable(t *testing.T) {
	ts, _ := testServer(t)
	advisor := signIn(t, ts, advisorEmail)

	for _, path := range []string{"/jobs/" + jobA, "/labour"} {
		body := get(t, advisor, ts.URL+path, http.StatusOK)
		for _, marker := range []string{`value="1 `, "value=\"1 ", `kr"`, `,00"`} {
			if strings.Contains(body, marker) {
				t.Errorf("%s has a formatted amount in a form value (%q); it will not parse when posted back", path, marker)
			}
		}
	}
}

// The reader's language decides the separators. This is the whole point of
// moving the formatting out of the model.
func TestAmountsFollowTheReadersLanguage(t *testing.T) {
	ts, pool := testServer(t)
	advisor := signIn(t, ts, advisorEmail)

	post(t, advisor, ts.URL+"/jobs/"+jobA+"/lines", url.Values{
		"kind": {"labour"}, "description": {"Replace front pads"},
		"quantity": {"2.5"}, "unit_price": {"895.00"}, "vat_rate": {"25"},
	})

	english := get(t, advisor, ts.URL+"/jobs/"+jobA, http.StatusOK)
	if !strings.Contains(english, "SEK 2,237.50") {
		t.Errorf("an English reader does not see a grouped amount with a currency:\n%s", firstLines(english, 5))
	}

	if err := database.InShop(context.Background(), pool, shopA, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE users SET locale = 'sv' WHERE id = 'aaaaaaaa-0000-0000-0000-00000000000b'`)
		return err
	}); err != nil {
		t.Fatalf("set locale: %v", err)
	}

	swedish := get(t, signIn(t, ts, advisorEmail), ts.URL+"/jobs/"+jobA, http.StatusOK)
	if !strings.Contains(swedish, "2 237,50 kr") {
		t.Errorf("a Swedish reader does not see kronor with a comma:\n%s", firstLines(swedish, 5))
	}
}

// The document has to be reachable and has to be refused, both by HTTP.
func TestAnIssuedInvoiceCanBeLookedAt(t *testing.T) {
	ts, _ := testServer(t)
	advisor := signIn(t, ts, advisorEmail)

	post(t, advisor, ts.URL+"/jobs/"+jobA+"/lines", url.Values{
		"kind": {"labour"}, "description": {"Replace front pads"},
		"quantity": {"2.5"}, "unit_price": {"895.00"}, "vat_rate": {"25"},
	})
	for _, state := range []string{"estimated", "awaiting_approval", "approved", "in_progress", "ready"} {
		post(t, advisor, ts.URL+"/jobs/"+jobA+"/state", url.Values{"state": {state}})
	}
	post(t, advisor, ts.URL+"/jobs/"+jobA+"/invoice", url.Values{})

	// The job page links it rather than printing a reference and nothing else.
	job := get(t, advisor, ts.URL+"/jobs/"+jobA, http.StatusOK)
	i := strings.Index(job, `href="/invoices/`)
	if i < 0 {
		t.Fatalf("the job page does not link the invoice it just issued:\n%s", firstLines(job, 40))
	}
	rest := job[i+len(`href="`):]
	link := rest[:strings.IndexByte(rest, '"')]

	doc := get(t, advisor, ts.URL+link, http.StatusOK)
	for _, want := range []string{"A-1", customerName, "Replace front pads", "SEK\u00a02,237.50"} {
		if !strings.Contains(doc, want) {
			t.Errorf("the invoice does not show %q:\n%s", want, firstLines(doc, 60))
		}
	}

	// A technician with the link in hand is refused by the read, not by a
	// template that leaves the prices out.
	tech := signIn(t, ts, techEmail)
	refusal := get(t, tech, ts.URL+link, http.StatusForbidden)
	if strings.Contains(refusal, customerName) || strings.Contains(refusal, "2,237.50") {
		t.Error("the refusal page leaked the invoice")
	}
}
