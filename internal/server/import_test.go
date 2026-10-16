package server

import (
	"bytes"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// Starting from a file, through the pages: only the owner reaches it, a
// template comes down with the columns, a Swedish file is read and previewed,
// a possible duplicate needs an answer, and applying twice does nothing twice.
func TestImportingACustomerFileThroughThePages(t *testing.T) {
	ts, pool := testServer(t)
	desk, tech, owner := signIn(t, ts, advisorEmail), signIn(t, ts, techEmail), signIn(t, ts, ownerEmail)
	base := ts.URL

	for _, c := range []*http.Client{desk, tech} {
		if status, _ := page(t, c, base+"/import"); status != http.StatusForbidden {
			t.Errorf("a non-owner opened the import page: %d", status)
		}
		if status, _ := page(t, c, base+"/import/template/customers"); status != http.StatusForbidden {
			t.Errorf("a non-owner downloaded a template: %d", status)
		}
	}
	status, tpl := page(t, owner, base+"/import/template/customers")
	if status != http.StatusOK || !strings.Contains(tpl, "ref;type;name") || !strings.HasPrefix(tpl, "\xef\xbb\xbf") {
		t.Errorf("the template answered %d: %q", status, tpl)
	}
	if status, _ := page(t, owner, base+"/import/template/nonsense"); status != http.StatusNotFound {
		t.Errorf("a template that does not exist answered %d", status)
	}

	upload := func(c *http.Client, kind, name string, file []byte) (*http.Response, error) {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		mw.WriteField("kind", kind)
		fw, _ := mw.CreateFormFile("file", name)
		fw.Write(file)
		mw.Close()
		req, _ := http.NewRequest("POST", base+"/import", &buf)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		req.Header.Set("Origin", base)
		return c.Do(req)
	}
	if resp, err := upload(desk, "customers", "k.csv", []byte("ref;type;name\nK-1;private;X\n")); err != nil || resp.StatusCode != http.StatusForbidden {
		t.Errorf("a non-owner uploaded: %v %v", resp, err)
	}
	if resp, err := upload(owner, "customers", "k.csv", []byte("ref;unit\nK-1;x\n")); err != nil || resp.StatusCode != http.StatusBadRequest {
		t.Errorf("a file without the columns answered %v %v", resp, err)
	}

	file := []byte("kundnr;typ;namn;telefon;epost\nK-1;privat;Åsa Öberg;070-123 45 67;asa@example.se\nK-2;företag;Bilfirman AB;;\n")
	resp, err := upload(owner, "customers", "kunder.csv", file)
	if err != nil || resp.StatusCode != http.StatusSeeOther && resp.StatusCode != http.StatusOK {
		t.Fatalf("upload answered %v %v", resp, err)
	}
	var batch string
	if err := pool.QueryRow(t.Context(), `SELECT id::text FROM import_batches WHERE shop_id = $1`, shopA).Scan(&batch); err != nil {
		t.Fatal(err)
	}
	status, body := page(t, owner, base+"/import/"+batch)
	for _, want := range []string{"Åsa Öberg", "Bilfirman AB", "would be created", "Apply"} {
		if !strings.Contains(body, want) {
			t.Errorf("the preview does not say %q", want)
		}
	}
	// The same file again lands on the same batch.
	if _, err := upload(owner, "customers", "kopia.csv", file); err != nil {
		t.Fatal(err)
	}
	var n int
	pool.QueryRow(t.Context(), `SELECT count(*) FROM import_batches WHERE shop_id = $1`, shopA).Scan(&n)
	if n != 1 {
		t.Errorf("%d batches after the same file twice", n)
	}

	if status := postStatus(t, desk, base+"/import/"+batch, url.Values{"action": {"apply"}}); status != http.StatusForbidden {
		t.Errorf("a non-owner applied: %d", status)
	}
	before := 0
	pool.QueryRow(t.Context(), `SELECT count(*) FROM customers WHERE shop_id = $1`, shopA).Scan(&before)
	if status := postStatus(t, owner, base+"/import/"+batch, url.Values{"action": {"apply"}}); status != http.StatusSeeOther {
		t.Fatalf("applying answered %d", status)
	}
	if status := postStatus(t, owner, base+"/import/"+batch, url.Values{"action": {"apply"}}); status != http.StatusSeeOther {
		t.Errorf("applying again answered %d", status)
	}
	after := 0
	pool.QueryRow(t.Context(), `SELECT count(*) FROM customers WHERE shop_id = $1`, shopA).Scan(&after)
	if after != before+2 {
		t.Errorf("%d customers after applying twice, want %d", after, before+2)
	}
	_, report := page(t, owner, base+"/import/"+batch)
	if !strings.Contains(report, "Applied") || !strings.Contains(report, "removed") {
		t.Errorf("the report: %s", report)
	}
	if strings.Contains(report, "asa@example.se") || strings.Contains(report, "070-123") {
		t.Error("the applied batch still shows what the file said")
	}

	// Another shop's batch is not here.
	status, other := page(t, owner, base+"/import/"+batchB)
	if status == http.StatusOK || strings.Contains(other, secretB) {
		t.Errorf("another shop's batch answered %d", status)
	}
	_ = io.Discard
}
