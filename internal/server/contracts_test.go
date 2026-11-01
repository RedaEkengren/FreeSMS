package server

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// A contract (#150) through the pages: the owner reads and prints it, makes
// the customer's link and is shown it once; the customer opens it with no
// session; a technician is not shown it at all.
func TestAContractIsPrintedAndOpenedOnTheCustomersLink(t *testing.T) {
	ts, pool := testServer(t)
	ctx := context.Background()
	owner := access.Scope{ShopID: shopA, UserID: "aaaaaaaa-0000-0000-0000-000000000011",
		Role: access.Of(access.RoleOwner), Modules: access.Modules}
	tpl, err := workshop.SaveDocumentTemplate(ctx, pool, owner, "", workshop.DocSalesContract, "Köpeavtal",
		"Bilen säljs för {price}.\nInga kända fel.")
	if err != nil {
		t.Fatal(err)
	}
	party, err := workshop.PartyOfCustomer(ctx, pool, owner, "aaaaaaaa-0000-0000-0000-000000000004")
	if err != nil {
		t.Fatal(err)
	}
	id, err := workshop.ProduceDocument(ctx, pool, owner, workshop.NewDocumentRecord{
		Kind: workshop.DocSalesContract, SubjectKind: "sale", SubjectID: "aaaaaaaa-0000-0000-0000-0000000000c9",
		TemplateVersionID: tpl.VersionID, Title: "Köpeavtal ABC 123",
		Parties: []workshop.DocumentPartyRole{{PartyID: party, Role: "buyer"}},
		Data:    map[string]string{"price": "120 000 kr"},
	})
	if err != nil {
		t.Fatal(err)
	}

	client := signIn(t, ts, ownerEmail)
	get := func(c *http.Client, path string) (int, string) {
		t.Helper()
		resp, err := c.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	status, body := get(client, "/contracts/"+id)
	if status != http.StatusOK || !strings.Contains(body, "Bilen säljs för 120 000 kr.") || !strings.Contains(body, "Not signed by anybody yet") {
		t.Fatalf("the owner's page: %d\n%s", status, body)
	}
	if status, _ := get(signIn(t, ts, techEmail), "/contracts/"+id); status != http.StatusForbidden {
		t.Errorf("a technician opening a contract: %d, want 403", status)
	}

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/contracts/"+id+"/link", strings.NewReader(url.Values{}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", ts.URL)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	link := regexp.MustCompile(`/c/[A-Za-z0-9_-]{20,}`).FindString(string(b))
	if resp.StatusCode != http.StatusOK || link == "" {
		t.Fatalf("making the link: %d, no link shown", resp.StatusCode)
	}
	status, body = get(http.DefaultClient, link)
	if status != http.StatusOK || !strings.Contains(body, "Bilen säljs för 120 000 kr.") || strings.Contains(body, "Make a link for the customer") {
		t.Errorf("the customer's link: %d\n%s", status, body)
	}
	if status, _ := get(http.DefaultClient, link+"x"); status != http.StatusNotFound {
		t.Errorf("a wrong link: %d, want 404", status)
	}
}
