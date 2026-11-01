package workshop_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/database"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// What a technician sees (#225).

// openedFor is what the session reads for a person: the fields open for
// them now, by the shop's profile or an exception of their own.
func openedFor(t *testing.T, pool *pgxpool.Pool, user string) []string {
	t.Helper()
	var out []string
	if err := pool.QueryRow(context.Background(), `
		SELECT coalesce(array_agg(DISTINCT field ORDER BY field), '{}') FROM technician_fields_open
		WHERE shop_id = $1 AND (user_id IS NULL OR user_id = $2)`, shopID, user).Scan(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

// techAs is a technician's scope with what is open for them now, as the
// session would carry it.
func techAs(t *testing.T, pool *pgxpool.Pool, user string) access.Scope {
	s := technician()
	s.UserID = user
	s.Opened = openedFor(t, pool, user)
	return s
}

// linePrices is what JobByID hands the caller of a job's prices.
func linePrices(t *testing.T, pool *pgxpool.Pool, scope access.Scope, job string) (sum int64) {
	t.Helper()
	_, lines, err := workshop.JobByID(context.Background(), pool, scope, job)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range lines {
		sum += l.UnitPriceMinor
		if l.EstimatedMinor != nil {
			sum += *l.EstimatedMinor
		}
	}
	return sum
}

// By default a technician is handed none of the six fields: no line price
// from JobByID, nothing from TechnicianJobFor, even on a job assigned to
// them. The counter's view of the same job is unchanged.
func TestByDefaultATechnicianIsHandedNoneOfTheFields(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	addTechnician(t, pool)
	job := readyToInvoice(t, pool)
	if _, err := pool.Exec(ctx, `UPDATE work_orders SET assigned_to = $2 WHERE id = $1`, job, techUserID); err != nil {
		t.Fatal(err)
	}
	tech := techAs(t, pool, techUserID)
	if len(tech.Opened) != 0 {
		t.Fatalf("opened by default: %v", tech.Opened)
	}
	for _, f := range access.TechnicianFields {
		if tech.SeesOnJob(f) {
			t.Errorf("a technician sees %s by default", f)
		}
		if !advisor().SeesOnJob(f) {
			t.Errorf("the counter does not see %s", f)
		}
	}
	if n := linePrices(t, pool, tech, job); n != 0 {
		t.Errorf("a technician was handed line prices adding up to %d", n)
	}
	if n := linePrices(t, pool, advisor(), job); n == 0 {
		t.Error("the counter lost the line prices")
	}
	if got, err := workshop.TechnicianJobFor(ctx, pool, tech, job); err != nil || got != (workshop.TechnicianJob{}) {
		t.Errorf("handed by default: %+v, %v", got, err)
	}
	if _, err := workshop.TechnicianView(ctx, pool, tech); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("a technician read the settings: %v", err)
	}
	if err := workshop.SetTechnicianField(ctx, pool, advisor(), access.FieldLinePrices, true, "Varför inte"); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("the counter opened a field: %v", err)
	}
	for _, f := range []string{"personal_number", "id_document", "payment_details", "customer_register", "finance"} {
		if err := workshop.SetTechnicianField(ctx, pool, owner(), f, true, "Försök"); !errors.Is(err, workshop.ErrInvalid) {
			t.Errorf("opened %q: %v, want refused", f, err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO technician_view_changes (shop_id, field, shown, reason, changed_by) VALUES ($1, $2, true, 'x', $3)`,
			shopID, f, userID); err == nil {
			t.Errorf("the schema took %q as a field", f)
		}
	}
}

// Opening one field for every technician shows that field and no other;
// an exception for one person does not reach another; one that has run
// out hides the field again with nothing to sweep it; and the customer is
// shown only on a job assigned to the technician, so a job taken away
// takes the name with it.
func TestOpeningAFieldShowsThatFieldOnlyToWhomItIsOpened(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	addTechnician(t, pool)
	job := readyToInvoice(t, pool)
	other, err := workshop.AddStaff(ctx, pool, owner(), "Andra Mekanikern", "andra@example.test", access.RoleTechnician, "a long enough password")
	if err != nil {
		t.Fatal(err)
	}
	if err := workshop.SetTechnicianField(ctx, pool, owner(), access.FieldLinePrices, true, ""); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("opened with no reason: %v", err)
	}
	if err := workshop.SetTechnicianField(ctx, pool, owner(), access.FieldLinePrices, true, "Ackord"); err != nil {
		t.Fatal(err)
	}
	for _, user := range []string{techUserID, other} {
		tech := techAs(t, pool, user)
		if !slices.Equal(tech.Opened, []string{access.FieldLinePrices}) {
			t.Errorf("open for %s: %v, want the prices alone", user, tech.Opened)
		}
		if n := linePrices(t, pool, tech, job); n == 0 {
			t.Errorf("%s was not handed the prices opened for every technician", user)
		}
		if got, _ := workshop.TechnicianJobFor(ctx, pool, tech, job); got != (workshop.TechnicianJob{}) {
			t.Errorf("%s was handed more than the prices: %+v", user, got)
		}
	}

	// One person's exception: the name, on a job assigned to them.
	if err := workshop.GrantTechnicianField(ctx, pool, owner(), techUserID, access.FieldCustomerName, "Ringer kunder själv", nil); err != nil {
		t.Fatal(err)
	}
	if err := workshop.GrantTechnicianField(ctx, pool, owner(), userID, access.FieldCustomerName, "Inte mekaniker", nil); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("an exception for somebody who is not a technician: %v", err)
	}
	if got, _ := workshop.TechnicianJobFor(ctx, pool, techAs(t, pool, techUserID), job); got.CustomerName != "" {
		t.Errorf("the name on a job not assigned to them: %q", got.CustomerName)
	}
	if _, err := pool.Exec(ctx, `UPDATE work_orders SET assigned_to = $2 WHERE id = $1`, job, techUserID); err != nil {
		t.Fatal(err)
	}
	if got, _ := workshop.TechnicianJobFor(ctx, pool, techAs(t, pool, techUserID), job); got.CustomerName != "A Customer" || got.CustomerPhone != "" {
		t.Errorf("the exception on their own job: %+v, want the name and not the number", got)
	}
	if _, err := pool.Exec(ctx, `UPDATE work_orders SET assigned_to = $2 WHERE id = $1`, job, other); err != nil {
		t.Fatal(err)
	}
	if got, _ := workshop.TechnicianJobFor(ctx, pool, techAs(t, pool, other), job); got.CustomerName != "" {
		t.Errorf("one person's exception reached another: %+v", got)
	}
	if got, _ := workshop.TechnicianJobFor(ctx, pool, techAs(t, pool, techUserID), job); got.CustomerName != "" {
		t.Errorf("the job was taken away and the name stayed: %+v", got)
	}

	// An exception run out.
	if _, err := pool.Exec(ctx, `INSERT INTO technician_view_exceptions (shop_id, user_id, field, granted, until, reason, by_user)
		VALUES ($1, $2, 'invoice_status', true, clock_timestamp() - interval '1 hour', 'Förra veckan', $3)`, shopID, other, userID); err != nil {
		t.Fatal(err)
	}
	if opened := openedFor(t, pool, other); slices.Contains(opened, access.FieldInvoiceStatus) {
		t.Errorf("an exception past its end still applies: %v", opened)
	}
	if err := workshop.GrantTechnicianField(ctx, pool, owner(), other, access.FieldInvoiceStatus, "Igår", new(time.Now().Add(-time.Hour))); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("an exception ending in the past: %v", err)
	}

	// Ended by hand, and the profile closed again.
	if err := workshop.EndTechnicianGrant(ctx, pool, owner(), techUserID, access.FieldCustomerName, "Klart"); err != nil {
		t.Fatal(err)
	}
	if err := workshop.EndTechnicianGrant(ctx, pool, owner(), techUserID, access.FieldCustomerName, "Igen"); !errors.Is(err, workshop.ErrNotFound) {
		t.Errorf("ended twice: %v", err)
	}
	if err := workshop.SetTechnicianField(ctx, pool, owner(), access.FieldLinePrices, false, "Slut på ackord"); err != nil {
		t.Fatal(err)
	}
	if opened := openedFor(t, pool, techUserID); len(opened) != 0 {
		t.Errorf("open after the profile closed and the exception ended: %v", opened)
	}
	view, err := workshop.TechnicianView(ctx, pool, owner())
	if err != nil || len(view.Profile) != len(access.TechnicianFields) || len(view.Exceptions) != 0 || len(view.Technicians) != 2 {
		t.Errorf("the settings: %+v, %v", view, err)
	}

	// Insert-only, as the application.
	for _, stmt := range []string{`UPDATE technician_view_changes SET shown = true`, `DELETE FROM technician_view_exceptions`} {
		if err := asApp(ctx, pool, stmt); err == nil {
			t.Errorf("%q as the application was let through", stmt)
		}
	}
}

// A technician who is not shown the customer's number still gets them
// rung: the request reaches the front desk with the job's findings.
func TestATechnicianAsksTheDeskToContactTheCustomer(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	addTechnician(t, pool)
	job := newJob(t, pool)
	if err := workshop.AskDeskToContact(ctx, pool, technician(), job, " "); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("asked with nothing to ask: %v", err)
	}
	if err := workshop.AskDeskToContact(ctx, pool, technician(), job, "Får vi byta bakre beläggen också?"); err != nil {
		t.Fatal(err)
	}
	found, err := workshop.FindingsFor(ctx, pool, advisor(), job)
	if err != nil || len(found) != 1 || !found[0].AsksContact || found[0].Handled() {
		t.Errorf("the desk's findings: %+v, %v", found, err)
	}
}

// asApp runs stmt as the application, in the shop.
func asApp(ctx context.Context, pool *pgxpool.Pool, stmt string) error {
	return database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, stmt)
		return err
	})
}

// A technician shown the job's prices is shown the total the invoice will
// print, surcharges included, and the amounts on the timeline's lines --
// the same field, read the same way. Not shown the prices, neither.
func TestATechnicianShownThePricesSeesTheTotalTheInvoicePrints(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	addTechnician(t, pool)
	if err := workshop.SaveSurcharges(ctx, pool, owner(), workshop.Surcharges{InvoiceFeeMinor: 4900}); err != nil {
		t.Fatal(err)
	}
	job := inProgress(t, pool, "Replace brake discs")
	desk, err := workshop.PreviewSurcharges(ctx, pool, advisor(), job)
	if err != nil || len(desk) != 1 {
		t.Fatalf("the counter's preview: %+v, %v", desk, err)
	}
	amounts := func(scope access.Scope) (n int) {
		t.Helper()
		events, err := workshop.TimelineFor(ctx, pool, scope, job)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range events {
			if e.Kind == "scope" && e.AmountMinor != nil {
				n++
			}
		}
		return n
	}

	closed := techAs(t, pool, techUserID)
	if _, err := workshop.PreviewSurcharges(ctx, pool, closed, job); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("a technician not shown the prices was handed the surcharges: %v", err)
	}
	if n := amounts(closed); n != 0 {
		t.Errorf("a technician not shown the prices sees %d amounts on the timeline", n)
	}

	if err := workshop.SetTechnicianField(ctx, pool, owner(), access.FieldLinePrices, true, "Ackord"); err != nil {
		t.Fatal(err)
	}
	open := techAs(t, pool, techUserID)
	got, err := workshop.PreviewSurcharges(ctx, pool, open, job)
	if err != nil || !slices.Equal(got, desk) {
		t.Errorf("the technician's surcharges %+v (%v), the counter's %+v", got, err, desk)
	}
	if n := amounts(open); n == 0 {
		t.Error("a technician shown the prices sees no amount on the timeline's lines")
	}
}

// Whether a job is invoiced is whether an invoice was issued for it and
// stands, not whether it came to anything: a job invoiced at nothing --
// a check done for free -- is invoiced, and with nothing owed, paid. An invoice taken back
// whole by a credit note no longer stands.
func TestAJobInvoicedAtNothingIsInvoiced(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	addTechnician(t, pool)
	job := newJob(t, pool)
	if err := workshop.AddLine(ctx, pool, advisor(), job, workshop.NewLine{Kind: "labour", Description: "Kontroll utan kostnad",
		QuantityMilli: 1000, UnitPriceMinor: 0, VATRateBasis: 2500}); err != nil {
		t.Fatal(err)
	}
	move(t, pool, job, workshop.StateEstimated, workshop.StateAwaitingApproval,
		workshop.StateApproved, workshop.StateInProgress, workshop.StateReady)
	if err := workshop.SetTechnicianField(ctx, pool, owner(), access.FieldInvoiceStatus, true, "Vet när bilen är klar"); err != nil {
		t.Fatal(err)
	}
	status := func(job string) string {
		t.Helper()
		got, err := workshop.TechnicianJobFor(ctx, pool, techAs(t, pool, techUserID), job)
		if err != nil {
			t.Fatal(err)
		}
		return got.InvoiceStatus
	}
	if s := status(job); s != "not invoiced" {
		t.Errorf("before the invoice: %q", s)
	}
	inv, err := workshop.Issue(ctx, pool, advisor(), job)
	if err != nil {
		t.Fatal(err)
	}
	if inv.GrossMinor != 0 {
		t.Fatalf("the free check was invoiced at %d", inv.GrossMinor)
	}
	if s := status(job); s != "paid" {
		t.Errorf("invoiced at nothing: %q, want paid", s)
	}
	other := readyToInvoice(t, pool)
	paid, err := workshop.Issue(ctx, pool, advisor(), other)
	if err != nil {
		t.Fatal(err)
	}
	if s := status(other); s != "invoiced" {
		t.Errorf("invoiced and owed: %q", s)
	}
	if _, err := workshop.CreditNote(ctx, pool, advisor(), paid.ID); err != nil {
		t.Fatal(err)
	}
	if s := status(other); s != "not invoiced" {
		t.Errorf("credited whole: %q, want not invoiced", s)
	}
}

// Erasing a technician takes away the reasons their exceptions were
// opened and ended with, which can name them, as it takes the reasons of
// the events about them: that a field was opened for them, by whom and
// when stays, and the rows are still never otherwise changed.
func TestErasureTakesTheReasonsOfATechniciansExceptions(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	addTechnician(t, pool)
	if err := workshop.GrantTechnicianField(ctx, pool, owner(), techUserID, access.FieldCustomerPhone, "Täcker för Anna medan hon är föräldraledig", nil); err != nil {
		t.Fatal(err)
	}
	var person string
	if err := pool.QueryRow(ctx, `UPDATE users SET active = false, deactivated_at = now() WHERE id = $1 RETURNING person_id::text`, techUserID).Scan(&person); err != nil {
		t.Fatal(err)
	}
	if err := workshop.ErasePerson(ctx, pool, advisor(), &recordingRemover{}, person, "Begäran"); err != nil {
		t.Fatal(err)
	}
	events, err := workshop.AuditTrail(ctx, pool, owner(), "user", techUserID)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, e := range events {
		if e.KeptIn == "technician_view_exceptions" {
			found = true
			if e.Reason != "" || !e.ReasonErased || e.Actor == "" || e.To != access.FieldCustomerPhone {
				t.Errorf("after the erasure: %+v", e)
			}
		}
	}
	if !found {
		t.Error("the erasure took the exception away")
	}
	if n := countRows(t, pool, `SELECT count(*) FROM technician_view_exceptions WHERE reason LIKE '%Anna%'`); n != 0 {
		t.Errorf("%d exceptions still say why in words that name somebody", n)
	}
	view, err := workshop.TechnicianView(ctx, pool, owner())
	if err != nil {
		t.Errorf("the settings page after the erasure: %v", err)
	}
	// The settings page says the reason was erased, rather than show a
	// blank one the data cannot hold.
	shown := false
	for _, e := range view.Exceptions {
		if e.UserID == techUserID {
			shown = true
			if e.Reason != "" || !e.ReasonErased {
				t.Errorf("the settings page shows the erased exception as %+v", e)
			}
		}
	}
	if !shown {
		t.Error("the settings page lost the exception after the erasure")
	}
	for _, stmt := range []string{`UPDATE technician_view_exceptions SET field = 'hourly_rate'`, `UPDATE technician_view_exceptions SET reason = 'Något annat'`} {
		if err := asApp(ctx, pool, stmt); err == nil {
			t.Errorf("%q as the application was let through", stmt)
		}
	}
}
