package workshop_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/RedaEkengren/RedaSMS/internal/access"
	"github.com/RedaEkengren/RedaSMS/internal/database"
	"github.com/RedaEkengren/RedaSMS/internal/workshop"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func addPart(t *testing.T, pool *pgxpool.Pool, number, name, unit string, costMinor int64) string {
	t.Helper()
	var id string
	err := database.InShop(context.Background(), pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			INSERT INTO parts (shop_id, number, name, unit, cost_minor, minimum_quantity)
			VALUES ($1, $2, $3, $4, $5, 2) RETURNING id`,
			shopID, number, name, unit, costMinor).Scan(&id)
	})
	if err != nil {
		t.Fatalf("add part: %v", err)
	}
	return id
}

func partsDeskScope() access.Scope {
	return access.Scope{ShopID: shopID, UserID: userID, Role: access.RoleParts}
}

func onHand(t *testing.T, pool *pgxpool.Pool, partID string) workshop.Part {
	t.Helper()
	parts, err := workshop.Parts(context.Background(), pool, partsDeskScope())
	if err != nil {
		t.Fatalf("Parts: %v", err)
	}
	for _, p := range parts {
		if p.ID == partID {
			return p
		}
	}
	t.Fatalf("part %s not in the list", partID)
	return workshop.Part{}
}

// Quantity on hand is the sum of movements, never a column written over.
func TestStockIsTheSumOfItsMovements(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	part := addPart(t, pool, "BP-100", "Brake pad set, front", "each", 45000)

	for _, m := range []workshop.Movement{
		{Kind: "received", Quantity: 10},
		{Kind: "consumed", Quantity: -3},
		{Kind: "returned", Quantity: -2},
	} {
		if err := workshop.Move(ctx, pool, partsDeskScope(), m, part, "", ""); err != nil {
			t.Fatalf("Move(%s): %v", m.Kind, err)
		}
	}

	p := onHand(t, pool, part)
	if p.OnHand != 5 {
		t.Errorf("on hand = %v, want 5", p.OnHand)
	}
	if p.Short() != false {
		t.Errorf("five against a minimum of two reads as short")
	}
}

// Oil is not a count.
func TestFluidsAreStockedByVolume(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	part := addPart(t, pool, "OIL-530", "Engine oil 5W-30", "litre", 6500)

	if err := workshop.Move(ctx, pool, partsDeskScope(),
		workshop.Movement{Kind: "received", Quantity: 20}, part, "", ""); err != nil {
		t.Fatalf("receive: %v", err)
	}
	if err := workshop.Move(ctx, pool, partsDeskScope(),
		workshop.Movement{Kind: "consumed", Quantity: -4.5}, part, "", ""); err != nil {
		t.Fatalf("consume: %v", err)
	}

	if got := onHand(t, pool, part).OnHand; got != 15.5 {
		t.Errorf("on hand = %v, want 15.5 litres", got)
	}
}

// Reserved is neither sold nor free.
func TestReservingDoesNotReduceStockButReducesWhatIsAvailable(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	part := addPart(t, pool, "BP-100", "Brake pads", "each", 45000)
	job := newJob(t, pool)

	workshop.Move(ctx, pool, partsDeskScope(), workshop.Movement{Kind: "received", Quantity: 10}, part, "", "")
	if err := workshop.ReserveForJob(ctx, pool, partsDeskScope(), job, part, 4); err != nil {
		t.Fatalf("ReserveForJob: %v", err)
	}

	p := onHand(t, pool, part)
	if p.OnHand != 10 {
		t.Errorf("on hand = %v, want 10 -- reserving does not take it off the shelf", p.OnHand)
	}
	if p.Available() != 6 {
		t.Errorf("available = %v, want 6", p.Available())
	}
}

// The case decided on the shop floor: a part put aside for a job that is then
// declined has to stop being reserved.
func TestDecliningAJobReleasesItsReservations(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	part := addPart(t, pool, "BP-100", "Brake pads", "each", 45000)
	job := newJob(t, pool)

	workshop.Move(ctx, pool, partsDeskScope(), workshop.Movement{Kind: "received", Quantity: 10}, part, "", "")
	workshop.ReserveForJob(ctx, pool, partsDeskScope(), job, part, 4)

	move(t, pool, job, workshop.StateEstimated, workshop.StateAwaitingApproval, workshop.StateDeclined)

	p := onHand(t, pool, part)
	if p.Reserved != 0 {
		t.Errorf("reserved = %v after the job was declined, want 0", p.Reserved)
	}
	if p.Available() != 10 {
		t.Errorf("available = %v, want all 10 back", p.Available())
	}
	if p.OnHand != 10 {
		t.Errorf("on hand = %v; declining must not consume anything", p.OnHand)
	}
}

// Fitting a part must not leave it both reserved and consumed.
func TestConsumingReleasesTheReservation(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	part := addPart(t, pool, "BP-100", "Brake pads", "each", 45000)
	job := newJob(t, pool)

	workshop.Move(ctx, pool, partsDeskScope(), workshop.Movement{Kind: "received", Quantity: 10}, part, "", "")
	workshop.ReserveForJob(ctx, pool, partsDeskScope(), job, part, 4)
	if err := workshop.ConsumeForJob(ctx, pool, partsDeskScope(), job, part, "", 4); err != nil {
		t.Fatalf("ConsumeForJob: %v", err)
	}

	p := onHand(t, pool, part)
	if p.OnHand != 6 {
		t.Errorf("on hand = %v, want 6", p.OnHand)
	}
	if p.Reserved != 0 {
		t.Errorf("reserved = %v, want 0", p.Reserved)
	}
	if p.Available() != 6 {
		t.Errorf("available = %v, want 6 -- not 2, which is what double counting would give", p.Available())
	}
}

// A flag could say "this is gone". Only a reason can say what a year of wrong
// orders cost.
func TestWriteOffsTotalByReason(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	part := addPart(t, pool, "BP-100", "Brake pads", "each", 45000)

	workshop.Move(ctx, pool, partsDeskScope(), workshop.Movement{Kind: "received", Quantity: 10}, part, "", "")
	for _, c := range []struct {
		reason string
		qty    float64
	}{
		{workshop.ReasonWrongPart, 2},
		{workshop.ReasonWrongPart, 1},
		{workshop.ReasonDamaged, 1},
	} {
		if err := workshop.Move(ctx, pool, partsDeskScope(),
			workshop.Movement{Kind: "written_off", Quantity: -c.qty, Reason: c.reason},
			part, "", ""); err != nil {
			t.Fatalf("write off: %v", err)
		}
	}

	if got := onHand(t, pool, part).OnHand; got != 6 {
		t.Errorf("on hand = %v, want 6", got)
	}

	from := time.Now().Add(-time.Hour)
	totals, err := workshop.WriteOffs(ctx, pool, advisor(), from, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("WriteOffs: %v", err)
	}
	if len(totals) != 2 {
		t.Fatalf("got %d reasons, want 2", len(totals))
	}
	// Wrongly ordered is the larger, so it sorts first.
	if totals[0].Reason != workshop.ReasonWrongPart {
		t.Errorf("first reason is %s, want the most expensive", totals[0].Reason)
	}
	if totals[0].Quantity != 3 {
		t.Errorf("wrongly ordered quantity = %v, want 3", totals[0].Quantity)
	}
	if totals[0].CostMinor != 135000 {
		t.Errorf("wrongly ordered cost = %d, want 135000 -- three at 450.00", totals[0].CostMinor)
	}
}

func TestAWriteOffNeedsAReasonAndNothingElseTakesOne(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	part := addPart(t, pool, "BP-100", "Brake pads", "each", 45000)

	if err := workshop.Move(ctx, pool, partsDeskScope(),
		workshop.Movement{Kind: "written_off", Quantity: -1}, part, "", ""); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a write-off with no reason was accepted: %v", err)
	}
	if err := workshop.Move(ctx, pool, partsDeskScope(),
		workshop.Movement{Kind: "received", Quantity: 1, Reason: workshop.ReasonDamaged},
		part, "", ""); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a receipt with a write-off reason was accepted: %v", err)
	}
}

// A negative figure is a symptom, not something to refuse. Refusing does not
// put the part back on the shelf.
func TestNegativeStockIsRecordedAndFlagged(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	part := addPart(t, pool, "BP-100", "Brake pads", "each", 45000)

	if err := workshop.Move(ctx, pool, partsDeskScope(),
		workshop.Movement{Kind: "consumed", Quantity: -1}, part, "", ""); err != nil {
		t.Fatalf("consuming from nothing was refused: %v", err)
	}
	p := onHand(t, pool, part)
	if p.OnHand != -1 {
		t.Errorf("on hand = %v, want -1", p.OnHand)
	}
	if !p.Negative() {
		t.Error("the part does not read as negative")
	}
}

// A five-krona clip and a five-thousand-krona turbo do not carry the same
// markup.
func TestPriceBandsApplyTheRightMarkup(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()

	upTo := func(n int64) *int64 { return &n }
	for _, b := range []struct {
		upTo   *int64
		markup int
	}{
		{upTo(10000), 10000}, // under 100.00: double it
		{upTo(100000), 5000}, // under 1000.00: half again
		{nil, 2000},          // above that: a fifth
	} {
		if err := workshop.SavePriceBand(ctx, pool, advisor(), b.upTo, b.markup); err != nil {
			t.Fatalf("SavePriceBand: %v", err)
		}
	}

	for _, c := range []struct{ cost, want int64 }{
		{5000, 10000},    // 50.00 -> 100.00
		{50000, 75000},   // 500.00 -> 750.00
		{500000, 600000}, // 5000.00 -> 6000.00
	} {
		got, err := workshop.PriceFromCost(ctx, pool, advisor(), c.cost)
		if err != nil {
			t.Fatalf("PriceFromCost: %v", err)
		}
		if got != c.want {
			t.Errorf("PriceFromCost(%d) = %d, want %d", c.cost, got, c.want)
		}
	}
}

// No bands means the cost is the price, so a shop can see it has not set any
// rather than being quietly given one.
func TestNoPriceBandsMeansNoMarkup(t *testing.T) {
	pool := setup(t)
	got, err := workshop.PriceFromCost(context.Background(), pool, advisor(), 50000)
	if err != nil {
		t.Fatalf("PriceFromCost: %v", err)
	}
	if got != 50000 {
		t.Errorf("PriceFromCost = %d, want the cost unchanged", got)
	}
}

// One part answers to several numbers, and the barcode belongs to the
// packaging.
func TestAPartIsFoundByAnyCodeItAnswersTo(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	part := addPart(t, pool, "BP-100", "Brake pads", "each", 45000)

	if err := database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		for _, c := range []struct{ code, kind string }{
			{"7391234567890", "barcode"},
			{"ATE-13046072932", "supplier"},
		} {
			if _, err := tx.Exec(ctx,
				`INSERT INTO part_codes (shop_id, part_id, code, kind) VALUES ($1,$2,$3,$4)`,
				shopID, part, c.code, c.kind); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("add codes: %v", err)
	}

	for _, code := range []string{"BP-100", "7391234567890", "ATE-13046072932"} {
		got, err := workshop.PartByCode(ctx, pool, partsDeskScope(), code)
		if err != nil {
			t.Fatalf("PartByCode(%q): %v", code, err)
		}
		if got.ID != part {
			t.Errorf("PartByCode(%q) found the wrong part", code)
		}
	}
	if _, err := workshop.PartByCode(ctx, pool, partsDeskScope(), "nothing"); !errors.Is(err, workshop.ErrNotFound) {
		t.Errorf("an unknown code = %v, want ErrNotFound", err)
	}
}

func TestStockIsNotForTechnicians(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	if _, err := workshop.Parts(context.Background(), pool, technician()); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("a technician read the stock list: %v", err)
	}
}
