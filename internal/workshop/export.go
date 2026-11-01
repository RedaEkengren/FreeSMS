package workshop

import (
	"archive/zip"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/database"
)

// A shop that wants to leave FreeSMS used to have pg_dump -- a database
// nobody else can read -- and the SIE file, which is bookkeeping and not
// history. Being able to leave is the feature: this writes the whole shop
// into one zip of CSV files and photographs, readable without this software.

// ExportTables are the tables that go into the export, each as a CSV of
// every column. Tables not here are in exportExcluded, with the reason; a
// test refuses a table in neither, so a new one is a decision.
var ExportTables = []string{
	"shops", "people", "customers", "vehicles", "vehicle_registrations", "vehicle_ownership",
	"odometer_readings", "work_orders", "work_order_lines", "time_entries", "findings",
	"part_requests", "part_request_receipts", "vehicle_presence", "customer_contacts",
	"inspection_templates", "inspection_template_items", "inspections", "inspection_items",
	"inspection_decisions", "attachments", "invoices", "invoice_lines", "invoice_payments",
	"invoice_series", "accounting_exports", "ledger_accounts", "parts", "part_codes",
	"stock_movements", "price_bands", "labour_times", "erasures", "shop_exports",
	"bookings", "booking_events", "staff_absences", "shop_closures", "rotas", "rota_hours", "shift_changes", "outbound_messages", "message_events",
	"work_order_state_changes", "quotes", "quote_lines", "quote_decisions", "enquiries", "enquiry_lines", "job_assignments", "job_plans", "work_order_line_revisions", "job_tasks", "job_task_events", "record_changes", "customer_people", "job_notes",
	"supplier_orders", "supplier_order_lines", "supplier_line_etas", "supplier_receipts", "supplier_returns", "job_recommendations", "delivery_summaries", "invoice_deliveries", "invoice_corrections", "receivable_cases", "receivable_events", "day_settlements", "day_reviews", "comebacks", "comeback_claims", "comeback_events", "outsourced_work", "outsourced_events", "import_batches", "import_refs", "setup_steps", "shop_module_changes", "user_roles", "protocol_photo_removals", "parties", "party_roles", "resources", "resource_block_jobs", "resource_retirements", "audit_events", "deposits", "deposit_applications", "vehicle_lookups", "cash_invoices", "payment_flows", "payment_flow_checks", "cash_register_settings", "payouts", "bank_statement_lines", "payment_matches", "identity_retention_settings", "outbox", "outbox_events", "document_templates", "document_template_versions", "document_record_parties", "document_retention_settings",
	// What technicians see (#225): the shop's decisions, with who and why.
	"technician_view_changes", "technician_view_exceptions",
	// Dealer: a car's cycles through stock, what was paid for it and the
	// checks before the money left (#160-#162).
	"stock_units", "vehicle_purchases", "stock_checks", "purchase_loans", "stock_check_overrides",
	// What a buyer worked out before bidding (#163).
	"purchase_calculations", "purchase_calculation_costs", "purchase_calculation_acceptances",
}

// exportQueries are tables exported with chosen columns, because one of
// theirs must never leave: a password's hash, a link's token.
var exportQueries = map[string]string{
	"users":             `SELECT id, shop_id, person_id, role, active, deactivated_at, locale, created_at FROM users`,
	"inspection_shares": `SELECT id, shop_id, inspection_id, created_by, created_at, expires_at, revoked_at FROM inspection_shares`,
	"invoice_links":     `SELECT id, shop_id, invoice_id, created_by, created_at, expires_at, revoked_at, revoked_by, first_opened_at, last_opened_at, opens FROM invoice_links`,
	"job_links":         `SELECT id, shop_id, work_order_id, enquiry_id, created_by, created_at, expires_at, revoked_at FROM job_links`,
	// A period as its two ends and a buffer in seconds, which a spreadsheet
	// reads; a range and an interval have no common written form (#149).
	"resource_blocks": `SELECT id, shop_id, resource_id, kind, status, lower(period) AS starts_at, upper(period) AS ends_at,
		extract(epoch FROM buffer_before)::bigint AS buffer_before_seconds, extract(epoch FROM buffer_after)::bigint AS buffer_after_seconds,
		expires_at, source_kind, source_id, request_key, reason, created_at, created_by, ended_at, ended_by FROM resource_blocks`,
	// Hashes as hex, which a reader can compare with the one printed on a
	// paper copy; a link's token never leaves, as an invoice link's (#150).
	"document_records": `SELECT id, shop_id, kind, subject_kind, subject_id, template_version_id, title, body, data,
		encode(content_sha256, 'hex') AS content_sha256, supersedes_id, created_at, created_by, erased_at FROM document_records`,
	"document_signatures": `SELECT id, shop_id, document_id, document_party_id, method, signed_at, recorded_by, provider_ref,
		evidence_key, witnessed_by, encode(document_sha256, 'hex') AS document_sha256 FROM document_signatures`,
	"document_record_links": `SELECT id, shop_id, document_id, created_by, created_at, expires_at, revoked_at, revoked_by,
		first_opened_at, last_opened_at, opens FROM document_record_links`,
	// Sealed as they are held, never opened: the export is not a way round
	// the key (#153). README.txt says how to open them with it.
	"identity_records": `SELECT id, shop_id, party_id, kind, purpose, legal_basis, recorded_by, recorded_at, expires_at,
		key_id, encode(sealed, 'base64') AS sealed_base64, CASE WHEN verification_ref IS NULL THEN NULL ELSE 'held' END AS verification_ref,
		ended_at, ended_by, end_reason, value_removed_at FROM identity_records`,
	"resource_block_changes": `SELECT id, shop_id, block_id, change, lower(old_period) AS old_starts_at, upper(old_period) AS old_ends_at,
		lower(new_period) AS new_starts_at, upper(new_period) AS new_ends_at, by_user, at, reason FROM resource_block_changes`,
	// Dealer: the evidence as document ids separated by spaces, and how long
	// a check stands in seconds, which a spreadsheet reads (#161, #162).
	"vat_eligibility": `SELECT id, shop_id, purchase_id, seller_type, eligible,
		array_to_string(evidence_document_ids::text[], ' ') AS evidence_document_ids, note, supersedes_id, reason,
		recorded_at, recorded_by FROM vat_eligibility`,
	"stock_check_settings": `SELECT id, shop_id, seq, kind, required, extract(epoch FROM max_age)::bigint AS max_age_seconds,
		changed_at, changed_by FROM stock_check_settings`,
}

// exportExcluded are the tables that do not go, and why.
var exportExcluded = map[string]string{
	"sessions":          "sign-in sessions: credentials, and nothing a new system needs",
	"login_attempts":    "kept ninety days to slow down guessing; not the shop's history",
	"idempotency_keys":  "request keys for safe retries; plumbing",
	"drafts":            "half-typed forms; not records",
	"schema_migrations": "this software's own bookkeeping",
	"import_rows":       "the rows of an upload, kept only until it is applied or discarded and wiped then; what a batch did lives in import_batches and import_refs",
}

// ExportExcluded says whether a table is left out, and why.
func ExportExcluded(table string) (string, bool) { r, ok := exportExcluded[table]; return r, ok }

// ExportQuery is the query a table is exported with.
func ExportQuery(table string) string {
	if q, ok := exportQueries[table]; ok {
		return q
	}
	return `SELECT * FROM ` + pgx.Identifier{table}.Sanitize()
}

// ExportedTables is every table that goes into the export, in order.
func ExportedTables() []string {
	out := append([]string{}, ExportTables...)
	for t := range exportQueries {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// ShopExport is one time the whole shop was taken.
type ShopExport struct {
	ID          string
	RequestedBy string
	RequestedAt time.Time
	FinishedAt  *time.Time
	FileName    string
	ByteSize    int64
	Error       string
}

// Done reports the file is written.
func (e ShopExport) Done() bool { return e.FinishedAt != nil && e.Error == "" }

// StartShopExport records that the whole shop is being taken out, and by
// whom. The owner's: it carries every customer the shop has had.
func StartShopExport(ctx context.Context, pool *pgxpool.Pool, scope access.Scope) (string, error) {
	if !scope.Role.RunsTheShop() {
		return "", access.ErrForbidden
	}
	var id string
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `INSERT INTO shop_exports (shop_id, requested_by) VALUES ($1, $2) RETURNING id`,
			scope.ShopID, scope.UserID).Scan(&id)
	})
	return id, err
}

// FinishShopExport records how it ended.
func FinishShopExport(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, id, fileName string, size int64, failure error) error {
	if !scope.Role.RunsTheShop() {
		return access.ErrForbidden
	}
	var msg *string
	if failure != nil {
		m := failure.Error()
		msg = &m
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE shop_exports SET finished_at = now(), file_name = nullif($2, ''), byte_size = $3, error = $4 WHERE id = $1`,
			id, fileName, size, msg)
		return err
	})
}

// ShopExports lists them, newest first.
func ShopExports(ctx context.Context, pool *pgxpool.Pool, scope access.Scope) ([]ShopExport, error) {
	if !scope.Role.RunsTheShop() {
		return nil, access.ErrForbidden
	}
	var out []ShopExport
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT e.id, coalesce(p.display_name, ''), e.requested_at, e.finished_at,
			       coalesce(e.file_name, ''), coalesce(e.byte_size, 0), coalesce(e.error, '')
			FROM shop_exports e
			LEFT JOIN users u ON u.id = e.requested_by
			LEFT JOIN people p ON p.id = u.person_id
			ORDER BY e.requested_at DESC`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e ShopExport
			if err := rows.Scan(&e.ID, &e.RequestedBy, &e.RequestedAt, &e.FinishedAt, &e.FileName, &e.ByteSize, &e.Error); err != nil {
				return err
			}
			out = append(out, e)
		}
		return rows.Err()
	})
	return out, err
}

// ShopExportByID reads one, for serving its file.
func ShopExportByID(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, id string) (ShopExport, error) {
	if !scope.Role.RunsTheShop() {
		return ShopExport{}, access.ErrForbidden
	}
	if !looksLikeUUID(id) {
		return ShopExport{}, ErrNotFound
	}
	all, err := ShopExports(ctx, pool, scope)
	if err != nil {
		return ShopExport{}, err
	}
	for _, e := range all {
		if e.ID == id {
			return e, nil
		}
	}
	return ShopExport{}, ErrNotFound
}

// PhotoOpener is where photographs are read from.
type PhotoOpener interface {
	Open(key string) (io.ReadCloser, error)
}

// WriteShopExport writes the whole shop to w as a zip: a CSV per table, the
// photographs, a manifest and a README saying how to read it. It streams,
// a table a row at a time, so ten years of history is a file and not memory.
func WriteShopExport(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, w io.Writer, photos PhotoOpener, release string) error {
	if !scope.Role.RunsTheShop() {
		return access.ErrForbidden
	}
	z := zip.NewWriter(w)
	counts := map[string]int{}
	var keys []string
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		for _, table := range ExportedTables() {
			n, err := writeTable(ctx, tx, z, table)
			if err != nil {
				return fmt.Errorf("export %s: %w", table, err)
			}
			counts[table+".csv"] = n
		}
		rows, err := tx.Query(ctx, `SELECT storage_key FROM attachments ORDER BY created_at`)
		if err != nil {
			return err
		}
		keys, err = pgx.CollectRows(rows, pgx.RowTo[string])
		return err
	})
	if err != nil {
		return err
	}

	// The photographs, which nothing can regenerate.
	missing := 0
	for _, key := range keys {
		r, err := photos.Open(key)
		if err != nil {
			missing++
			continue
		}
		f, err := z.Create("photos/" + key + ".jpg")
		if err == nil {
			_, err = io.Copy(f, r)
		}
		r.Close()
		if err != nil {
			return fmt.Errorf("export photo %s: %w", key, err)
		}
	}

	manifest := map[string]any{
		"exported_at":    time.Now().UTC().Format(time.RFC3339),
		"software":       "FreeSMS",
		"release":        release,
		"shop_id":        scope.ShopID,
		"rows":           counts,
		"photos":         len(keys) - missing,
		"photos_missing": missing,
	}
	f, err := z.Create("manifest.json")
	if err != nil {
		return err
	}
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err := enc.Encode(manifest); err != nil {
		return err
	}
	f, err = z.Create("README.txt")
	if err != nil {
		return err
	}
	if _, err := io.WriteString(f, exportReadme); err != nil {
		return err
	}
	return z.Close()
}

func writeTable(ctx context.Context, tx pgx.Tx, z *zip.Writer, table string) (int, error) {
	rows, err := tx.Query(ctx, ExportQuery(table))
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	f, err := z.Create(table + ".csv")
	if err != nil {
		return 0, err
	}
	cw := csv.NewWriter(f)
	fields := rows.FieldDescriptions()
	header := make([]string, len(fields))
	for i, fd := range fields {
		header[i] = fd.Name
	}
	if err := cw.Write(header); err != nil {
		return 0, err
	}
	n := 0
	record := make([]string, len(fields))
	for rows.Next() {
		values, err := rows.Values()
		if err != nil {
			return n, err
		}
		for i, v := range values {
			record[i] = exportValue(v, fields[i].DataTypeOID)
		}
		if err := cw.Write(record); err != nil {
			return n, err
		}
		n++
	}
	if err := rows.Err(); err != nil {
		return n, err
	}
	cw.Flush()
	return n, cw.Error()
}

// exportValue writes one value as somebody else's software reads it: times
// in UTC as ISO 8601, a date as a date, numbers with a point, identifiers
// as UUIDs, arrays and documents as JSON.
func exportValue(v any, oid uint32) string {
	const dateOID = 1082
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case bool:
		return strconv.FormatBool(x)
	case int16, int32, int64, int:
		return fmt.Sprint(x)
	case float32, float64:
		return fmt.Sprint(x)
	case time.Time:
		if oid == dateOID {
			return x.Format("2006-01-02")
		}
		return x.UTC().Format(time.RFC3339Nano)
	case [16]byte:
		return pgtype.UUID{Bytes: x, Valid: true}.String()
	case pgtype.Numeric:
		b, err := x.MarshalJSON()
		if err != nil {
			return ""
		}
		return strings.Trim(string(b), `"`)
	case []byte:
		return string(x)
	default:
		b, err := json.Marshal(x)
		if err != nil {
			return fmt.Sprint(x)
		}
		return string(b)
	}
}

const exportReadme = `The whole of one workshop, exported from FreeSMS.

Every table is a CSV file, UTF-8, comma-separated, with a header row.
Rows join on their id columns: a work order's lines have its id in
work_order_id, an invoice's lines its id in invoice_id, and so on.

How values are written:
  - identifiers are UUIDs;
  - times are UTC, ISO 8601 (2026-10-08T07:30:00Z); a date is 2026-10-08;
  - money is in minor units, öre for SEK, in columns ending _minor:
    129000 is 1 290,00 kr;
  - quantities and other decimals use a point: 1.5;
  - VAT rates are in basis points, in columns ending _bp: 2500 is 25 %;
  - an empty field is no value.

Issued invoices are exported as they were issued, from their own frozen
rows, not recomputed from the work order.

Photographs are in photos/, named by the storage_key in attachments.csv.
An inspection item's photographs are the attachments rows carrying its
id in inspection_item_id.

People who asked to be erased are exported as they are held now:
anonymised, with the erasure recorded in erasures.csv.

Not included, by design: passwords, sign-in sessions, the tokens of
customer links, failed sign-in attempts, half-typed drafts.

Identity records (identity_records.csv) are exported sealed, never in
the clear: sealed_base64 is the nonce (12 bytes) followed by the
AES-256-GCM ciphertext, sealed with the shop's key named in key_id, and
the associated data is shop_id/id/party_id/kind in lower case, separated
by "/". Whoever holds the shop's key can open them; without it nobody
can, which is the point. A reference to a check is shown only as held.

manifest.json says when this was taken, by which release, and how many
rows each file holds.
`
