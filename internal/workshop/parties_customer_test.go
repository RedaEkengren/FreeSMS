package workshop_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// A party that needs an invoice gets its one customer (#177, #199): the
// same one every time, made once from the party's own details, which move
// to it as the front desk keeps them.
func TestAPartyGetsOneCustomer(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()

	// Already a customer: that customer.
	var asCustomer string
	if err := pool.QueryRow(ctx, `SELECT id FROM customers LIMIT 1`).Scan(&asCustomer); err == nil {
		party, err := workshop.PartyOfCustomer(ctx, pool, advisor(), asCustomer)
		if err != nil {
			t.Fatal(err)
		}
		if got, err := workshop.CustomerForParty(ctx, pool, advisor(), party); err != nil || got != asCustomer {
			t.Errorf("a customer's party: %s, %v; want %s", got, err, asCustomer)
		}
	}

	// A private person, no customer yet: one is made, and the party keeps
	// its name, number and email through it.
	kim, err := workshop.CreateParty(ctx, pool, everything(owner()), workshop.NewParty{Kind: "private", Name: "Kim Köpare",
		Phone: "0701234567", Email: "kim@example.test", AddressLine1: "Storgatan 1", PostalCode: "123 45", City: "Täby",
		Role: workshop.PartyBuyer})
	if err != nil {
		t.Fatal(err)
	}
	before, _ := workshop.PartyByID(ctx, pool, sales(), kim)
	made, err := workshop.CustomerForParty(ctx, pool, sales(), kim)
	if err != nil || made == "" {
		t.Fatalf("a customer for the party: %q, %v", made, err)
	}
	after, err := workshop.PartyByID(ctx, pool, sales(), kim)
	if err != nil || after.Name != before.Name || after.Phone != before.Phone || after.Email != before.Email || after.Address != before.Address {
		t.Errorf("the party through its customer: %+v, was %+v (%v)", after, before, err)
	}
	if again, err := workshop.CustomerForParty(ctx, pool, sales(), kim); err != nil || again != made {
		t.Errorf("asked again: %s, %v; want %s", again, err, made)
	}
	c, err := workshop.CustomerByID(ctx, pool, advisor(), made)
	if err != nil || c.Name != "Kim Köpare" || c.City != "Täby" {
		t.Errorf("the customer: %+v, %v", c, err)
	}

	// A company, asked for four times at once: one customer.
	firm, err := workshop.CreateParty(ctx, pool, everything(owner()), workshop.NewParty{Kind: "company", Name: "Åkeri AB",
		OrgNumber: "556677-8899", City: "Solna", Role: workshop.PartyBuyer})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	got := make([]string, 4)
	errs := make([]error, 4)
	for i := range got {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got[i], errs[i] = workshop.CustomerForParty(ctx, pool, sales(), firm)
		}()
	}
	wg.Wait()
	for i := range got {
		if errs[i] != nil || got[i] != got[0] {
			t.Errorf("call %d: %s, %v; want the one customer", i, got[i], errs[i])
		}
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM customers WHERE company_name = 'Åkeri AB' AND org_number = '556677-8899'`).Scan(&n); err != nil || n != 1 {
		t.Errorf("%d customers for the company (%v)", n, err)
	}

	// Whoever deals with nobody's money asks in vain.
	if _, err := workshop.CustomerForParty(ctx, pool, everything(technician()), kim); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("a technician made a customer: %v", err)
	}
	// A party is never moved to another customer.
	if err := asApp(ctx, pool, `UPDATE parties SET customer_id = '`+made+`' WHERE id = '`+firm+`'`); err == nil {
		t.Error("a party was moved to another customer")
	}
}
