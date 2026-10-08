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
	"bookings", "booking_events", "staff_absences", "shop_closures", "rotas", "rota_hours", "shift_changes",
}

// exportQueries are tables exported with chosen columns, because one of
// theirs must never leave: a password's hash, a link's token.
var exportQueries = map[string]string{
	"users":             `SELECT id, shop_id, person_id, role, active, deactivated_at, locale, created_at FROM users`,
	"inspection_shares": `SELECT id, shop_id, inspection_id, created_by, created_at, expires_at, revoked_at FROM inspection_shares`,
	"job_links":         `SELECT id, shop_id, work_order_id, created_by, created_at, expires_at, revoked_at FROM job_links`,
}

// exportExcluded are the tables that do not go, and why.
var exportExcluded = map[string]string{
	"sessions":          "sign-in sessions: credentials, and nothing a new system needs",
	"login_attempts":    "kept ninety days to slow down guessing; not the shop's history",
	"idempotency_keys":  "request keys for safe retries; plumbing",
	"drafts":            "half-typed forms; not records",
	"schema_migrations": "this software's own bookkeeping",
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

manifest.json says when this was taken, by which release, and how many
rows each file holds.
`
