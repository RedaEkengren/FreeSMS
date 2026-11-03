package server

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// A file of charges is shown with what matching says before anything is
// saved, and saved when asked (#198).
func TestAFileOfChargesIsShownBeforeItIsSaved(t *testing.T) {
	ts, pool := testServer(t)
	seedRentalDesk(t, func(t *testing.T, stmts []string) { seedShop(t, pool, shopA, stmts) })
	desk := access.Scope{ShopID: shopA, UserID: "aaaaaaaa-0000-0000-0000-0000000000b2", Role: access.Of(access.RoleRentalAgent), Modules: access.Modules}
	client := signIn(t, ts, rentalEmail)
	file := "kind,registration,when,amount,issuer,reference,due\n" +
		"congestion_tax,ZZZ 999,2026-10-20 08:15,45,Transportstyrelsen,TS-77,2026-11-30\n"
	resp := postModuleForm(t, client, ts.URL, "/rental/fines/import", url.Values{"file_text": {file}})
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "TS-77") || !strings.Contains(body, `name="save"`) {
		t.Fatalf("the preview: %d %s", resp.StatusCode, body)
	}
	if fines, err := workshop.Fines(t.Context(), pool, desk); err != nil || len(fines) != 0 {
		t.Fatalf("the preview saved %d, %v", len(fines), err)
	}
	if resp := postModuleForm(t, client, ts.URL, "/rental/fines/import", url.Values{"file_text": {file}, "save": {"1"}}); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("saving: %d %s", resp.StatusCode, readBody(t, resp))
	}
	fines, err := workshop.Fines(t.Context(), pool, desk)
	if err != nil || len(fines) != 1 || fines[0].Match != workshop.FineUnknownUnit {
		t.Fatalf("saved: %+v, %v", fines, err)
	}
	resp, _ = client.Get(ts.URL + "/rental/fines")
	if body := readBody(t, resp); !strings.Contains(body, "TS-77") || !strings.Contains(body, "/rental/fines/"+fines[0].ID+"/paid") {
		t.Errorf("the page does not list the charge")
	}
}
