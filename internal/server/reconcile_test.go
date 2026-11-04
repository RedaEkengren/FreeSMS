package server

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// The owner imports the bank's statement twice and gets its lines once,
// types one in, and the page shows them unmatched; the front desk is
// refused the page (#152).
func TestTheOwnerReconcilesTheBanksStatement(t *testing.T) {
	ts, _ := testServer(t)
	owner := signIn(t, ts, ownerEmail)
	file := "Bokföringsdag;Belopp;Text;Id\n2026-10-30;1 250,00;OCR 4711;B-1\n2026-10-31;-300,50;Bankavgift;B-2\n"
	upload := func() {
		t.Helper()
		var body bytes.Buffer
		w := multipart.NewWriter(&body)
		part, _ := w.CreateFormFile("file", "statement.csv")
		part.Write([]byte(file))
		w.Close()
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/reconcile/import", &body)
		req.Header.Set("Content-Type", w.FormDataContentType())
		req.Header.Set("Origin", ts.URL)
		resp, err := owner.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("importing: %d %s", resp.StatusCode, readBody(t, resp))
		}
	}
	upload()
	upload()
	if resp := postModuleForm(t, owner, ts.URL, "/reconcile/lines", url.Values{"booked_on": {"2026-11-01"}, "amount": {"99"}, "reference": {"Swish 123"}}); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("typing a line: %d %s", resp.StatusCode, readBody(t, resp))
	}
	resp, err := owner.Get(ts.URL + "/reconcile")
	if err != nil {
		t.Fatal(err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK || strings.Count(body, "OCR 4711") != 1 || !strings.Contains(body, "Bankavgift") || !strings.Contains(body, "Swish 123") {
		t.Errorf("the reconciliation page: %d\n%s", resp.StatusCode, body)
	}
	desk := signIn(t, ts, advisorEmail)
	if resp, _ := desk.Get(ts.URL + "/reconcile"); resp.StatusCode != http.StatusForbidden {
		t.Errorf("the front desk's view of the reconciliation: %d", resp.StatusCode)
	}
}
