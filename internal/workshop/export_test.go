package workshop_test

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"io"
	"regexp"
	"strings"
	"testing"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/database"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
	"github.com/jackc/pgx/v5"
)

// A shop that wanted to leave had pg_dump, which nobody else can read.

type fakePhotos map[string][]byte

func (f fakePhotos) Open(key string) (io.ReadCloser, error) {
	b, ok := f[key]
	if !ok {
		return nil, errors.New("no such photo")
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

// Every table is in the export or left out with a reason: a new table is a
// decision, not something that silently stays behind or silently leaves.
func TestEveryTableIsExportedOrLeftOutForAReason(t *testing.T) {
	pool := setup(t)
	exported := map[string]bool{}
	for _, tbl := range workshop.ExportedTables() {
		exported[tbl] = true
	}
	var tables []string
	database.InShop(context.Background(), pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT table_name FROM information_schema.tables
			WHERE table_schema = 'public' AND table_type = 'BASE TABLE'`)
		if err != nil {
			return err
		}
		tables, err = pgx.CollectRows(rows, pgx.RowTo[string])
		return err
	})
	if len(tables) < 30 {
		t.Fatalf("found %d tables; the query has stopped working", len(tables))
	}
	for _, tbl := range tables {
		if _, out := workshop.ExportExcluded(tbl); !exported[tbl] && !out {
			t.Errorf("table %s is neither exported nor left out with a reason", tbl)
		}
	}
}

// Nothing that is a credential leaves: no column that names one.
func TestNoExportedColumnIsACredential(t *testing.T) {
	pool := setup(t)
	secret := regexp.MustCompile(`password|token|secret|hash`)
	database.InShop(context.Background(), pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		for _, tbl := range workshop.ExportedTables() {
			rows, err := tx.Query(ctx, workshop.ExportQuery(tbl)+` LIMIT 0`)
			if err != nil {
				t.Errorf("%s: %v", tbl, err)
				continue
			}
			for _, fd := range rows.FieldDescriptions() {
				if secret.MatchString(fd.Name) {
					t.Errorf("%s.%s leaves in the export", tbl, fd.Name)
				}
			}
			rows.Close()
		}
		return nil
	})
}

func readZip(t *testing.T, b []byte) map[string][]byte {
	t.Helper()
	z, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatalf("not a zip: %v", err)
	}
	out := map[string][]byte{}
	for _, f := range z.File {
		r, _ := f.Open()
		out[f.Name], _ = io.ReadAll(r)
		r.Close()
	}
	return out
}

func csvRows(t *testing.T, b []byte) []map[string]string {
	t.Helper()
	recs, err := csv.NewReader(bytes.NewReader(b)).ReadAll()
	if err != nil || len(recs) == 0 {
		t.Fatalf("not a CSV with a header: %v", err)
	}
	var out []map[string]string
	for _, r := range recs[1:] {
		m := map[string]string{}
		for i, h := range recs[0] {
			m[h] = r[i]
		}
		out = append(out, m)
	}
	return out
}

func TestTheWholeShopLeavesReadableWithoutFreeSMS(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	job, invoiceID := invoicedJob(t, pool)

	// A photograph on an inspection item.
	var photoKey = "abcdef0123456789abcdef0123456789"
	database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO attachments (shop_id, work_order_id, storage_key, content_type, byte_size)
			VALUES ($1, $2, $3, 'image/jpeg', 4)`, shopID, job, photoKey)
		return err
	})
	// And somebody who asked to be erased.
	if err := workshop.ErasePerson(ctx, pool, owner(), "22222222-2222-2222-2222-222222222223", "Begärde radering"); err != nil {
		t.Fatalf("ErasePerson: %v", err)
	}

	var buf bytes.Buffer
	if err := workshop.WriteShopExport(ctx, pool, owner(), &buf, fakePhotos{photoKey: []byte("JPEG")}, "test"); err != nil {
		t.Fatalf("WriteShopExport: %v", err)
	}
	files := readZip(t, buf.Bytes())

	for _, want := range []string{"work_orders.csv", "work_order_lines.csv", "invoices.csv", "invoice_lines.csv",
		"customers.csv", "vehicles.csv", "users.csv", "stock_movements.csv", "README.txt", "manifest.json"} {
		if _, ok := files[want]; !ok {
			t.Errorf("%s is not in the export", want)
		}
	}
	if _, ok := files["sessions.csv"]; ok {
		t.Error("sign-in sessions left in the export")
	}
	if string(files["photos/"+photoKey+".jpg"]) != "JPEG" {
		t.Error("the photograph is not in the export as it was stored")
	}

	// The invoice as issued, from its own rows: 1 000,00 kr net, 25 % on top.
	invoices := csvRows(t, files["invoices.csv"])
	if len(invoices) != 1 || invoices[0]["id"] != invoiceID || invoices[0]["gross_minor"] != "125000" {
		t.Errorf("invoices.csv = %v", invoices)
	}
	if !strings.Contains(string(files["users.csv"]), "service_advisor") || strings.Contains(string(files["users.csv"]), "password") {
		t.Error("users.csv is missing the staff or carries a password")
	}
	if strings.Contains(string(files["people.csv"]), "A Customer") {
		t.Error("an erased person's name is in the export")
	}
	if !strings.Contains(string(files["erasures.csv"]), "Begärde radering") {
		t.Error("the erasure is not recorded in the export")
	}
}

// The owner's alone, and every time is on record.
func TestOnlyTheOwnerTakesTheWholeShopAndItIsRecorded(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	if _, err := workshop.StartShopExport(ctx, pool, advisor()); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("the front desk started an export: %v", err)
	}
	if err := workshop.WriteShopExport(ctx, pool, advisor(), io.Discard, fakePhotos{}, "test"); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("the front desk wrote an export: %v", err)
	}
	id, err := workshop.StartShopExport(ctx, pool, owner())
	if err != nil {
		t.Fatal(err)
	}
	if err := workshop.FinishShopExport(ctx, pool, owner(), id, "freesms-x.zip", 1234, nil); err != nil {
		t.Fatal(err)
	}
	list, err := workshop.ShopExports(ctx, pool, owner())
	if err != nil || len(list) != 1 || !list[0].Done() || list[0].RequestedBy != "An Advisor" {
		t.Errorf("exports = %+v, %v; want one, done, by the person who took it", list, err)
	}
}
