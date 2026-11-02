package server

import (
	"context"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/config"
	"github.com/RedaEkengren/FreeSMS/internal/database"
	"github.com/RedaEkengren/FreeSMS/internal/testsupport"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
	"github.com/RedaEkengren/FreeSMS/migrations"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The golden output of a workshop-only installation (#142).
//
// Every Core change from M6 on -- an invoice with a source, an inspection
// with a subject, parties, resources, modules -- reaches tables the workshop
// already reads. "The workshop is unchanged" is measured here rather than
// asserted: the seeded shop (scripts/seed.sql), taken to an issued invoice, a
// payment and a credit note, is rendered page by page and compared with
// files kept in testdata/golden. Ids, instants, dates, the drawing of a
// payment code (which encodes the dates) and the static files' version
// strings are normalised; nothing else is.
//
// A difference fails. If it is meant, the files are written again with
//
//	go test ./internal/server -run Golden -golden.update
//
// in a commit of its own that says why the workshop's output changed.
// Regenerating them to make a red test green is the thing this exists to
// stop.
var updateGolden = flag.Bool("golden.update", false, "write the workshop's golden output instead of comparing with it")

// writeBeforeM6 writes the database the release before M6 populated. Run
// once, at that release; see TestWorkshopGoldenOutputAfterAnUpgrade.
var writeBeforeM6 = flag.Bool("golden.before-m6", false, "write the pre-M6 database fixture from this release")

const (
	goldenDir = "testdata/golden"

	// The seeded shop and its two people (scripts/seed.sql).
	seedShopID     = "11111111-1111-1111-1111-111111111111"
	seedTechnician = "33333333-3333-3333-3333-333333333333"
	seedOwner      = "33333333-3333-3333-3333-333333333334"
	seedPassword   = "workshop"
)

// goldenPage is one page, as somebody reads it.
type goldenPage struct {
	file   string
	as     string // email of the reader
	method string
	path   string
	form   url.Values
}

// goldenWorld is what the scenario made that a page addresses.
type goldenWorld struct {
	invoice, credit string
	from, to        string // the period the figures and the export cover
}

func (w goldenWorld) pages() []goldenPage {
	owner, tech := "anna@example.test", "reda@example.test"
	period := url.Values{"from": {w.from}, "to": {w.to}}
	return []goldenPage{
		{file: "invoice.html", as: owner, path: "/invoices/" + w.invoice},
		{file: "credit-note.html", as: owner, path: "/invoices/" + w.credit},
		{file: "invoice.xml", as: owner, path: "/invoices/" + w.invoice + "/einvoice.xml"},
		{file: "credit-note.xml", as: owner, path: "/invoices/" + w.credit + "/einvoice.xml"},
		{file: "accounting.se", as: owner, method: http.MethodPost, path: "/accounting/export",
			form: url.Values{"from": {w.from}, "to": {w.to}, "again": {"yes"}}},
		{file: "board-owner.html", as: owner, path: "/board"},
		{file: "start-technician.html", as: tech, path: "/"},
		{file: "waiting-owner.html", as: owner, path: "/mine"},
		{file: "waiting-technician.html", as: tech, path: "/mine"},
		{file: "figures.html", as: owner, path: "/figures?" + period.Encode()},
	}
}

// The seeded shop, at this release, renders as it did.
func TestWorkshopGoldenOutput(t *testing.T) {
	pool := testsupport.FreshPool(t)
	ctx := context.Background()
	if err := database.Migrate(ctx, pool, migrations.FS); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	seed, err := os.ReadFile("../../scripts/seed.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, string(seed)); err != nil {
		t.Fatalf("seed: %v", err)
	}
	world := goldenScenario(t, pool)
	compareGolden(t, pool, world, false)
}

// The release before M6 wrote a database; this release, migrated over it,
// renders the same pages from it -- the invoices it issued included (#142).
//
// testdata/golden/before-m6.sql is that database: the seeded shop taken
// through goldenScenario by the release before the first M6 migration, and
// written out row by row with -golden.before-m6. It is not regenerated: a
// fixture written by today's code would prove nothing about yesterday's
// rows. Its instants are moved forward on loading by however long ago it was
// taken, so that "uncollected for 4 days" still reads 4.
func TestWorkshopGoldenOutputAfterAnUpgrade(t *testing.T) {
	pool := testsupport.FreshPool(t)
	ctx := context.Background()
	if err := database.Migrate(ctx, pool, migrationsUpTo(t, lastBeforeM6)); err != nil {
		t.Fatalf("migrate to %04d: %v", lastBeforeM6, err)
	}
	loadBeforeM6(t, pool)
	if err := database.Migrate(ctx, pool, migrations.FS); err != nil {
		t.Fatalf("migrate forward over the release before M6: %v", err)
	}
	var world goldenWorld
	if err := pool.QueryRow(ctx, `
		SELECT (SELECT id FROM invoices WHERE credit_of_id IS NULL ORDER BY number LIMIT 1),
		       (SELECT id FROM invoices WHERE credit_of_id IS NOT NULL ORDER BY number LIMIT 1)`,
	).Scan(&world.invoice, &world.credit); err != nil {
		t.Fatalf("the documents the release before issued: %v", err)
	}
	// Its invoices were issued before 0091 and carry no payment reference,
	// which is not backfilled (#234): the one thing they show differently.
	compareGolden(t, pool, world.withPeriod(t, pool), true)
}

// Writes testdata/golden/before-m6.sql. Only with -golden.before-m6, and
// only at a release whose newest migration is lastBeforeM6.
func TestWriteTheDatabaseBeforeM6(t *testing.T) {
	if !*writeBeforeM6 {
		t.Skip("writes the pre-M6 fixture; run with -golden.before-m6 at that release")
	}
	pool := testsupport.FreshPool(t)
	ctx := context.Background()
	if err := database.Migrate(ctx, pool, migrations.FS); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	var newest int64
	if err := pool.QueryRow(ctx, `SELECT max(version) FROM schema_migrations`).Scan(&newest); err != nil || newest != lastBeforeM6 {
		t.Fatalf("this release is at %04d (%v); the fixture is the database of %04d", newest, err, lastBeforeM6)
	}
	seed, err := os.ReadFile("../../scripts/seed.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, string(seed)); err != nil {
		t.Fatalf("seed: %v", err)
	}
	goldenScenario(t, pool)
	if err := os.WriteFile(filepath.Join(goldenDir, "before-m6.sql"), []byte(dumpRows(t, pool)), 0o644); err != nil {
		t.Fatal(err)
	}
}

// lastBeforeM6 is the newest migration of the release before M6.
const lastBeforeM6 = 62

// migrationsUpTo is the migrations a release at version had.
func migrationsUpTo(t *testing.T, version int) fs.FS {
	t.Helper()
	names, err := fs.Glob(migrations.FS, "*.sql")
	if err != nil {
		t.Fatal(err)
	}
	out := fstest.MapFS{}
	for _, name := range names {
		n, err := strconv.Atoi(name[:4])
		if err != nil || n > version {
			continue
		}
		body, err := fs.ReadFile(migrations.FS, name)
		if err != nil {
			t.Fatal(err)
		}
		out[name] = &fstest.MapFile{Data: body}
	}
	return out
}

// dumpRows writes every row of every table but the migration ledger as SQL a
// later test can load into the same schema: one INSERT per row, the row as
// JSON, so that no column type needs quoting rules of its own. Sorted, so
// that writing it twice gives the same file. The first line says when it was
// taken.
func dumpRows(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	ctx := context.Background()
	var taken time.Time
	if err := pool.QueryRow(ctx, `SELECT now()`).Scan(&taken); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "-- taken: %s\n", taken.UTC().Format(time.RFC3339Nano))
	b.WriteString("-- The seeded shop as the release before M6 wrote it, through goldenScenario.\n")
	b.WriteString("-- Written by TestWriteTheDatabaseBeforeM6 (internal/server/golden_test.go); not edited by hand.\n")

	rows, err := pool.Query(ctx, `
		SELECT c.relname,
		       string_agg(quote_ident(a.attname), ', ' ORDER BY a.attnum)
		  FROM pg_class c
		  JOIN pg_namespace n ON n.oid = c.relnamespace AND n.nspname = 'public'
		  JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum > 0
		                     AND NOT a.attisdropped AND a.attgenerated = ''
		 WHERE c.relkind = 'r' AND c.relname <> 'schema_migrations'
		 GROUP BY c.relname ORDER BY c.relname`)
	if err != nil {
		t.Fatal(err)
	}
	type table struct{ name, columns string }
	var tables []table
	for rows.Next() {
		var tb table
		if err := rows.Scan(&tb.name, &tb.columns); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, tb)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for _, tb := range tables {
		ident := `"` + tb.name + `"`
		rows, err := pool.Query(ctx, `SELECT row_to_json(x)::text AS r FROM `+ident+` x ORDER BY r`)
		if err != nil {
			t.Fatalf("%s: %v", tb.name, err)
		}
		for rows.Next() {
			var row string
			if err := rows.Scan(&row); err != nil {
				t.Fatal(err)
			}
			fmt.Fprintf(&b, "INSERT INTO %s (%s) SELECT %s FROM json_populate_record(NULL::%s, %s);\n",
				ident, tb.columns, tb.columns, ident, quoteLiteral(row))
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
	}
	seqs, err := pool.Query(ctx, `SELECT sequencename, last_value FROM pg_sequences
		WHERE schemaname = 'public' AND last_value IS NOT NULL ORDER BY sequencename`)
	if err != nil {
		t.Fatal(err)
	}
	for seqs.Next() {
		var name string
		var last int64
		if err := seqs.Scan(&name, &last); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&b, "SELECT setval(%s, %d);\n", quoteLiteral(`"`+name+`"`), last)
	}
	if err := seqs.Err(); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func quoteLiteral(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// loadBeforeM6 loads the fixture and moves every instant in it forward by
// however long ago it was taken, and every date by as many days on the
// shop's calendar: the database as it would be had the release before M6
// written it a moment ago. Triggers are off while it loads -- the rows are
// already what the triggers allowed, and an issued invoice must be written
// back as it was -- which is what a restore does too.
func loadBeforeM6(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	body, err := os.ReadFile(filepath.Join(goldenDir, "before-m6.sql"))
	if err != nil {
		t.Fatal(err)
	}
	first, _, _ := strings.Cut(string(body), "\n")
	taken, err := time.Parse(time.RFC3339Nano, strings.TrimPrefix(first, "-- taken: "))
	if err != nil {
		t.Fatalf("before-m6.sql does not say when it was taken: %v", err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SET LOCAL session_replication_role = replica`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, string(body)); err != nil {
		t.Fatalf("load before-m6.sql: %v", err)
	}
	cols, err := tx.Query(ctx, `
		SELECT quote_ident(table_name), quote_ident(column_name), data_type
		  FROM information_schema.columns
		 WHERE table_schema = 'public' AND table_name <> 'schema_migrations'
		   AND is_generated = 'NEVER'
		   AND data_type IN ('timestamp with time zone', 'timestamp without time zone', 'date')
		 ORDER BY 1, 2`)
	if err != nil {
		t.Fatal(err)
	}
	var shifts []string
	for cols.Next() {
		var table, column, kind string
		if err := cols.Scan(&table, &column, &kind); err != nil {
			t.Fatal(err)
		}
		by := "now() - $1::timestamptz"
		if kind == "date" {
			by = "((now() AT TIME ZONE 'Europe/Stockholm')::date - ($1::timestamptz AT TIME ZONE 'Europe/Stockholm')::date)"
		}
		shifts = append(shifts, fmt.Sprintf(`UPDATE %s SET %s = %s + (%s)`, table, column, column, by))
	}
	if err := cols.Err(); err != nil {
		t.Fatal(err)
	}
	for _, q := range shifts {
		if _, err := tx.Exec(ctx, q, taken); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

// goldenScenario takes the seeded shop to the documents the golden pages
// show: the shop's particulars filled in, the brake job done and invoiced,
// part of it paid and one line credited. Through the domain, as the counter
// would, so that what a later release does to the workshop's own operations
// shows in the output too.
func goldenScenario(t *testing.T, pool *pgxpool.Pool) goldenWorld {
	t.Helper()
	ctx := context.Background()
	// A workshop-only shop, as every shop is unless it says otherwise.
	workshopOnly := []access.Module{access.Workshop}
	owner := access.Scope{ShopID: seedShopID, UserID: seedOwner, Role: access.Of(access.RoleOwner), Modules: workshopOnly}
	tech := access.Scope{ShopID: seedShopID, UserID: seedTechnician, Role: access.Of(access.RoleTechnician), Modules: workshopOnly}

	if err := workshop.SaveDetails(ctx, pool, owner, workshop.ShopDetails{
		Name: "Verkstaden", AddressLine1: "Verkstadsgatan 1", PostalCode: "123 45", City: "Stockholm",
		OrgNumber: "556677-8899", VATNumber: "SE556677889901", Phone: "08-123 456 78",
		Email: "verkstaden@example.test", PaymentReference: "Bankgiro 123-4567", PaymentScheme: "bankgiro",
		EndpointID: "0007:5566778899", PaymentTermsDays: 30, FTax: true, Locale: "sv", Currency: "SEK",
	}); err != nil {
		t.Fatalf("shop details: %v", err)
	}
	const customer = "44444444-4444-4444-4444-444444444444"
	card, err := workshop.CustomerByID(ctx, pool, owner, customer)
	if err != nil {
		t.Fatalf("customer: %v", err)
	}
	if _, err := workshop.EditCustomer(ctx, pool, owner, customer, card.Revision, workshop.CustomerEdit{
		Name: card.Name, Phone: card.Phone, Email: card.Email,
		AddressLine1: card.AddressLine1, PostalCode: "123 45", City: card.City, Country: "SE",
		EndpointID: "0007:5560160680", BuyerReference: "Reda",
	}); err != nil {
		t.Fatalf("edit customer: %v", err)
	}

	const brakes = "77777777-7777-7777-7777-777777777777"
	for _, to := range []workshop.State{workshop.StateInProgress, workshop.StateReady} {
		if err := workshop.SetState(ctx, pool, tech, brakes, to); err != nil {
			t.Fatalf("brakes to %s: %v", to, err)
		}
	}
	inv, err := workshop.Issue(ctx, pool, owner, brakes)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	part := int64(100000)
	if _, err := workshop.RecordPayment(ctx, pool, owner, inv.ID, workshop.NewPayment{
		AmountMinor: &part, Method: "swish", PaidOn: time.Now(), Reference: "Swish 1",
	}); err != nil {
		t.Fatalf("payment: %v", err)
	}
	credit, err := workshop.IssueCredit(ctx, pool, owner, inv.ID, workshop.CreditRequest{
		Lines: []workshop.CreditLine{{Position: 2}}, Reason: "Fel belägg, byttes under garanti",
	})
	if err != nil {
		t.Fatalf("credit: %v", err)
	}
	return goldenWorld{invoice: inv.ID, credit: credit.ID}.withPeriod(t, pool)
}

// withPeriod is the window the figures and the export are asked for: from a
// week before the shop's earliest invoice to today, on the shop's calendar.
// Asked for rather than defaulted, so that a run on the first of a month reads
// the same figures as a run on the fifteenth.
func (w goldenWorld) withPeriod(t *testing.T, pool *pgxpool.Pool) goldenWorld {
	t.Helper()
	var from, to time.Time
	if err := pool.QueryRow(context.Background(),
		`SELECT (min(issued_at) AT TIME ZONE 'Europe/Stockholm')::date - 7,
		        (now() AT TIME ZONE 'Europe/Stockholm')::date
		   FROM invoices`).Scan(&from, &to); err != nil {
		t.Fatalf("period: %v", err)
	}
	w.from, w.to = from.Format("2006-01-02"), to.Format("2006-01-02")
	return w
}

// compareGolden renders every golden page and compares it with its file.
// issuedBefore0091 compares documents issued before payment references
// existed: the golden output's reference and code are left out of what is
// expected, and must not be on the page.
func compareGolden(t *testing.T, pool *pgxpool.Pool, world goldenWorld, issuedBefore0091 bool) {
	t.Helper()
	// The golden output is a workshop-only shop's, and is compared as one.
	var runs []string
	if err := pool.QueryRow(context.Background(),
		`SELECT ARRAY(SELECT module FROM shop_modules WHERE shop_id = $1)`, seedShopID).Scan(&runs); err != nil {
		t.Fatalf("what the seeded shop runs: %v", err)
	}
	if len(runs) != 1 || runs[0] != string(access.Workshop) {
		t.Fatalf("the seeded shop runs %v; the golden output is a workshop-only shop's", runs)
	}
	ts := goldenServer(t, pool)
	clients := map[string]*http.Client{}
	for _, p := range world.pages() {
		client, ok := clients[p.as]
		if !ok {
			client = goldenSignIn(t, ts, p.as)
			clients[p.as] = client
		}
		got := normaliseGolden(fetchGolden(t, ts, client, p))
		path := filepath.Join(goldenDir, p.file)
		if *updateGolden {
			if err := os.MkdirAll(goldenDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("%s: %v (write it with -golden.update)", p.file, err)
			continue
		}
		want = goldenQR.ReplaceAll(want, []byte("${1}<modules>${2}"))
		if issuedBefore0091 {
			want = goldenPaymentCode.ReplaceAll(want, nil)
			// The electronic invoice is paid with its number instead,
			// the only reference it has.
			if id := goldenDocumentID.FindSubmatch(want); id != nil {
				want = goldenPaymentID.ReplaceAll(want, []byte("<cbc:PaymentID>"+string(id[1])+"</cbc:PaymentID>"))
			}
			if goldenPaymentCode.MatchString(got) {
				t.Errorf("%s: a document issued before 0091 shows a payment reference", p.file)
			}
		}
		if got != string(want) {
			t.Errorf("%s differs from the workshop's golden output:\n%s", p.file, firstDifference(string(want), got))
		}
	}
}

func goldenServer(t *testing.T, pool *pgxpool.Pool) *httptest.Server {
	t.Helper()
	cfg := &config.Config{
		BaseURL: "http://localhost", DefaultLocale: "en", Release: "golden",
		AttachmentsDir: t.TempDir(), ExportDir: t.TempDir(),
	}
	srv, err := New(pool, slog.New(slog.NewTextHandler(io.Discard, nil)), cfg, seedShopID)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	handler, err := srv.routes()
	if err != nil {
		t.Fatalf("routes: %v", err)
	}
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	return ts
}

func goldenSignIn(t *testing.T, ts *httptest.Server, email string) *http.Client {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	form := url.Values{"email": {email}, "password": {seedPassword}}
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", ts.URL)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("sign in %s: %v", email, err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("sign in %s returned %d", email, resp.StatusCode)
	}
	return client
}

func fetchGolden(t *testing.T, ts *httptest.Server, client *http.Client, p goldenPage) string {
	t.Helper()
	method := p.method
	if method == "" {
		method = http.MethodGet
	}
	var body io.Reader
	if p.form != nil {
		body = strings.NewReader(p.form.Encode())
	}
	req, _ := http.NewRequest(method, ts.URL+p.path, body)
	if p.form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	req.Header.Set("Origin", ts.URL)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s: %v", p.file, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s %s returned %d:\n%s", method, p.path, resp.StatusCode, b)
	}
	return string(b)
}

var (
	goldenUUID    = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)
	goldenInstant = regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})`)
	goldenDate    = regexp.MustCompile(`\b\d{4}-\d{2}-\d{2}\b`)
	goldenSIEDate = regexp.MustCompile(`\b20\d{6}\b`)
	goldenClock   = regexp.MustCompile(`\b\d{2}:\d{2}\b`)
	// A date as a person reads it, in English or Swedish: "1 nov 2026",
	// "5 Oct", and the weekday that may stand before either.
	goldenDay = regexp.MustCompile(`\b\d{1,2} (Jan|Feb|Mar|Apr|May|Jun|Jul|Aug|Sep|Oct|Nov|Dec|` +
		`jan|feb|mar|apr|maj|jun|jul|aug|sep|okt|nov|dec)\b( \d{4})?`)
	goldenWeekday = regexp.MustCompile(`(^|[^\pL])(Mon|Tue|Wed|Thu|Fri|Sat|Sun|mån|tis|ons|tor|fre|lör|sön)( \d{1,2})?([^\pL]|$)`)
	goldenAsset   = regexp.MustCompile(`\?v=[0-9a-f]+`)
	// The modules of a payment code: it encodes the invoice's date and due
	// date, so it is drawn differently every day, as a date is written.
	// What it says is paycode_test.go's and payref's to check; the page
	// keeps its kind, size and caption.
	goldenQR = regexp.MustCompile(`(<figure class="paycode"><svg[^>]*>.*?<path fill="#000" d=")[^"]*(")`)
	// The payment reference and code on an invoice (#234), in Swedish.
	goldenDocumentID  = regexp.MustCompile(`\n  <cbc:ID>([A-Z]+-[0-9]+)</cbc:ID>`)
	goldenPaymentID   = regexp.MustCompile(`<cbc:PaymentID>[0-9]+</cbc:PaymentID>`)
	goldenPaymentCode = regexp.MustCompile(`(?s)\n  <p>Betalningsreferens \(OCR\).*?</p>(\n  <figure class="paycode">.*?</figure>)?`)
)

// normaliseGolden replaces what varies between two runs of the same release
// -- ids, instants, dates in any of the ways a page writes them, times of
// day, a payment code's drawing, the static files' versions -- and
// nothing else. Each distinct id keeps its own placeholder, so a link that
// pointed at the invoice still points at the same thing as the invoice.
func normaliseGolden(s string) string {
	ids := map[string]string{}
	s = goldenUUID.ReplaceAllStringFunc(s, func(id string) string {
		id = strings.ToLower(id)
		if _, ok := ids[id]; !ok {
			ids[id] = fmt.Sprintf("<id-%d>", len(ids)+1)
		}
		return ids[id]
	})
	s = goldenInstant.ReplaceAllString(s, "<instant>")
	s = goldenDate.ReplaceAllString(s, "<date>")
	s = goldenSIEDate.ReplaceAllString(s, "<date>")
	s = goldenClock.ReplaceAllString(s, "<hh:mm>")
	s = goldenDay.ReplaceAllString(s, "<day>")
	s = goldenWeekday.ReplaceAllString(s, "$1<weekday>$4")
	s = goldenAsset.ReplaceAllString(s, "?v=<version>")
	s = goldenQR.ReplaceAllString(s, "${1}<modules>${2}")
	return s
}

// firstDifference shows where two texts part, with a little of each.
func firstDifference(want, got string) string {
	wl, gl := strings.Split(want, "\n"), strings.Split(got, "\n")
	for i := 0; i < len(wl) || i < len(gl); i++ {
		var w, g string
		if i < len(wl) {
			w = wl[i]
		}
		if i < len(gl) {
			g = gl[i]
		}
		if w != g {
			return fmt.Sprintf("line %d\n  golden: %q\n  now:    %q", i+1, w, g)
		}
	}
	return "(lengths differ)"
}
