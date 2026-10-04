package workshop_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

func price(v int64) *int64 { return &v }

// A shop set up from nothing adds a part, receives it, counts it, finds it by
// its barcode and puts it on a job -- without writing to the database.
func TestAShopAddsAPartAndUsesIt(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()

	id, err := workshop.SavePart(ctx, pool, partsDeskScope(), workshop.CatalogueEntry{
		Number: "OF-1", Name: "Oil filter", Unit: "each", Location: "A2",
		CostMinor: price(4500), PriceMinor: price(12900), MinimumMilli: 2000,
		Codes: workshop.ParseCodes("7310000000011\n\n7310000000011\nMANN-W712"),
	})
	if err != nil {
		t.Fatalf("SavePart: %v", err)
	}
	if err := workshop.Move(ctx, pool, partsDeskScope(), workshop.Movement{Kind: "received", Quantity: 10}, id, "", ""); err != nil {
		t.Fatalf("receive: %v", err)
	}
	if _, err := workshop.Stocktake(ctx, pool, partsDeskScope(), id, 9000, ""); err != nil {
		t.Fatalf("count: %v", err)
	}

	// The scanner finds it by the barcode on the box.
	found, err := workshop.PartByCode(ctx, pool, partsDeskScope(), "7310000000011")
	if err != nil || found.ID != id {
		t.Fatalf("PartByCode = %+v, %v; want the new part", found, err)
	}
	if found.OnHand != 9 {
		t.Errorf("on hand = %v, want 9", found.OnHand)
	}

	// The counter can put it on a job.
	job := newJob(t, pool)
	offered, err := workshop.PartsForJob(ctx, pool, advisor(), job, "oil")
	if err != nil {
		t.Fatalf("PartsForJob: %v", err)
	}
	if len(offered) != 1 || offered[0].ID != id {
		t.Errorf("the job is offered %+v, want the new part", offered)
	}

	e, _ := workshop.CatalogueEntryByID(ctx, pool, partsDeskScope(), id)
	if strings.Join(e.Codes, ",") != "7310000000011,MANN-W712" || e.Minimum() != "2" {
		t.Errorf("read back codes %v and minimum %s", e.Codes, e.Minimum())
	}
}

// A number or a code belongs to one part. The scanner finds by either, so a
// second part answering to the same one would be found or not by chance.
func TestANumberOrCodeBelongsToOnePart(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	if _, err := workshop.SavePart(ctx, pool, partsDeskScope(), workshop.CatalogueEntry{
		Number: "OF-1", Name: "Oil filter", Unit: "each", Codes: []string{"7310000000011"}}); err != nil {
		t.Fatalf("SavePart: %v", err)
	}

	for name, e := range map[string]workshop.CatalogueEntry{
		"the same number":         {Number: "OF-1", Name: "Another", Unit: "each"},
		"a number that is a code": {Number: "7310000000011", Name: "Another", Unit: "each"},
		"a code that is a number": {Number: "OF-2", Name: "Another", Unit: "each", Codes: []string{"OF-1"}},
		"a code another part has": {Number: "OF-3", Name: "Another", Unit: "each", Codes: []string{"7310000000011"}},
	} {
		_, err := workshop.SavePart(ctx, pool, partsDeskScope(), e)
		if !errors.Is(err, workshop.ErrInvalid) || !strings.Contains(err.Error(), "Oil filter") {
			t.Errorf("%s: %v, want refused, naming the part that has it", name, err)
		}
	}
}

// Changing a part's cost does not reprice the past: the movement keeps the
// cost it moved at.
func TestChangingAPartLeavesItsHistory(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	e := workshop.CatalogueEntry{Number: "OF-1", Name: "Oil filter", Unit: "each", CostMinor: price(4500)}
	id, _ := workshop.SavePart(ctx, pool, partsDeskScope(), e)
	workshop.Move(ctx, pool, partsDeskScope(), workshop.Movement{Kind: "received", Quantity: 10}, id, "", "")

	e.ID, e.CostMinor, e.Name = id, price(6000), "Oil filter, long life"
	if _, err := workshop.SavePart(ctx, pool, partsDeskScope(), e); err != nil {
		t.Fatalf("change: %v", err)
	}
	moves, _ := workshop.MovementsFor(ctx, pool, partsDeskScope(), id)
	if len(moves) != 1 || moves[0].CostMinor == nil || *moves[0].CostMinor != 4500 {
		t.Errorf("the receipt's cost became %v; it moved at 45.00", moves[0].CostMinor)
	}
	got, _ := workshop.CatalogueEntryByID(ctx, pool, partsDeskScope(), id)
	if got.Name != "Oil filter, long life" || *got.CostMinor != 6000 {
		t.Errorf("the change did not take: %+v", got)
	}
}

// Retired, not deleted: gone from the list, the job and the scanner, and its
// history kept, until it is brought back.
func TestARetiredPartIsNotOffered(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	id, _ := workshop.SavePart(ctx, pool, partsDeskScope(), workshop.CatalogueEntry{
		Number: "OF-1", Name: "Oil filter", Unit: "each", Codes: []string{"7310000000011"}})
	workshop.Move(ctx, pool, partsDeskScope(), workshop.Movement{Kind: "received", Quantity: 3}, id, "", "")

	if err := workshop.SetPartActive(ctx, pool, partsDeskScope(), id, false); err != nil {
		t.Fatalf("retire: %v", err)
	}
	if parts, _ := workshop.Parts(ctx, pool, partsDeskScope()); len(parts) != 0 {
		t.Error("a retired part is still listed")
	}
	if offered, _ := workshop.PartsForJob(ctx, pool, advisor(), newJob(t, pool), "oil"); len(offered) != 0 {
		t.Error("a retired part is still offered on a job")
	}
	if _, err := workshop.PartByCode(ctx, pool, partsDeskScope(), "7310000000011"); !errors.Is(err, workshop.ErrNotFound) {
		t.Errorf("the scanner still finds a retired part: %v", err)
	}
	if retired, _ := workshop.RetiredParts(ctx, pool, partsDeskScope()); len(retired) != 1 {
		t.Errorf("retired parts = %d, want the one", len(retired))
	}
	if moves, _ := workshop.MovementsFor(ctx, pool, partsDeskScope(), id); len(moves) != 1 {
		t.Error("retiring took the history with it")
	}

	workshop.SetPartActive(ctx, pool, partsDeskScope(), id, true)
	if parts, _ := workshop.Parts(ctx, pool, partsDeskScope()); len(parts) != 1 || parts[0].OnHand != 3 {
		t.Error("a part brought back is not listed with its stock")
	}
}

// A technician uses parts and does not keep the catalogue; and a part has to
// be a part.
func TestAPartMustBeAPart(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	ctx := context.Background()

	if _, err := workshop.SavePart(ctx, pool, technician(), workshop.CatalogueEntry{
		Number: "X", Name: "X", Unit: "each"}); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("a technician adding a part: %v, want forbidden", err)
	}
	for name, e := range map[string]workshop.CatalogueEntry{
		"no number":     {Name: "Oil filter", Unit: "each"},
		"no name":       {Number: "OF-1", Unit: "each"},
		"no such unit":  {Number: "OF-1", Name: "Oil filter", Unit: "pcs"},
		"negative cost": {Number: "OF-1", Name: "Oil filter", Unit: "each", CostMinor: price(-1)},
	} {
		if _, err := workshop.SavePart(ctx, pool, partsDeskScope(), e); !errors.Is(err, workshop.ErrInvalid) {
			t.Errorf("%s: %v, want invalid", name, err)
		}
	}
}
