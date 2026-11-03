package server

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// A call to another service that needs a person is on the owner's page,
// and is cancelled there with why; a technician is not shown the page.
func TestACallThatNeedsAPersonIsOnTheOwnersPage(t *testing.T) {
	ts, pool := testServer(t)
	ctx := context.Background()
	owner := access.Scope{ShopID: shopA, UserID: "aaaaaaaa-0000-0000-0000-000000000011",
		Role: access.Of(access.RoleOwner), Modules: access.Modules}
	tpl, err := workshop.SaveDocumentTemplate(ctx, pool, owner, "", workshop.DocSalesContract, "Köpeavtal", "Bilen säljs.")
	if err != nil {
		t.Fatal(err)
	}
	party, err := workshop.PartyOfCustomer(ctx, pool, owner, "aaaaaaaa-0000-0000-0000-000000000004")
	if err != nil {
		t.Fatal(err)
	}
	doc, err := workshop.ProduceDocument(ctx, pool, owner, workshop.NewDocumentRecord{
		Kind: workshop.DocSalesContract, SubjectKind: "sale", SubjectID: "aaaaaaaa-0000-0000-0000-0000000000c8",
		TemplateVersionID: tpl.VersionID, Title: "Köpeavtal", Parties: []workshop.DocumentPartyRole{{PartyID: party, Role: "buyer"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	d, err := workshop.DocumentRecordByID(ctx, pool, owner, doc)
	if err != nil {
		t.Fatal(err)
	}
	call, err := workshop.QueueSignatureRequest(ctx, pool, owner, doc, d.Parties[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if run, err := workshop.RunOutbox(ctx, pool, shopA, nil, 10); err != nil || run.Manual != 1 {
		t.Fatalf("the worker: %+v, %v", run, err)
	}
	client := signIn(t, ts, ownerEmail)
	page := func(c *http.Client) (int, string) {
		resp, err := c.Get(ts.URL + "/outbox")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if status, body := page(client); status != http.StatusOK || !strings.Contains(body, "A signature asked for") || !strings.Contains(body, "To do by hand") {
		t.Fatalf("the owner's page: %d\n%s", status, body)
	}
	if status, _ := page(signIn(t, ts, techEmail)); status != http.StatusForbidden {
		t.Errorf("a technician's: %d, want 403", status)
	}
	if status := postStatus(t, client, ts.URL+"/outbox/"+call+"/cancel", url.Values{"reason": {"Undertecknas på plats"}}); status != http.StatusSeeOther {
		t.Errorf("cancelling it: %d", status)
	}
	if _, body := page(client); !strings.Contains(body, "Nothing is waiting.") {
		t.Errorf("after cancelling:\n%s", body)
	}
}
