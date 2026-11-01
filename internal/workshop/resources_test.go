package workshop_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/database"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The availability ledger (#149). The shop runs every module by its scope,
// as a session in such a shop would carry.

func rentalDesk() access.Scope { return otherRoles(access.Of(access.RoleRentalAgent)) }

func trailer(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	id, err := workshop.CreateResource(context.Background(), pool, rentalDesk(), workshop.NewResource{Name: "Släp 2"})
	if err != nil {
		t.Fatalf("CreateResource: %v", err)
	}
	return id
}

// in2031 is a moment on a day of May 2031, well ahead, in UTC.
func in2031(day, hour int) time.Time { return time.Date(2031, 5, day, hour, 0, 0, 0, time.UTC) }

func rent(pool *pgxpool.Pool, s access.Scope, res string, from, to time.Time) (string, error) {
	return workshop.BookResource(context.Background(), pool, s, workshop.ResourceBooking{
		ResourceID: res, Kind: workshop.BlockRental, From: from, To: to})
}

// Overlaps are refused with a sentence naming the use in the way; the
// buffers are part of what is stored; an open-ended block takes everything
// after it.
func TestABookedResourceRefusesAnOverlapAndSaysWhatHasIt(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	res := trailer(t, pool)
	id, err := workshop.BookResource(ctx, pool, rentalDesk(), workshop.ResourceBooking{
		ResourceID: res, Kind: workshop.BlockRental, From: in2031(1, 10), To: in2031(1, 12),
		BufferBefore: 30 * time.Minute, BufferAfter: time.Hour, Reason: "Flytt"})
	if err != nil {
		t.Fatal(err)
	}
	blocks, err := workshop.ResourceBlocks(ctx, pool, rentalDesk(), res, in2031(1, 0), in2031(2, 0))
	if err != nil || len(blocks) != 1 {
		t.Fatalf("blocks = %+v, %v", blocks, err)
	}
	b := blocks[0]
	if b.ID != id || !b.From.Equal(in2031(1, 10).Add(-30*time.Minute)) || b.To == nil || !b.To.Equal(in2031(1, 13)) {
		t.Errorf("stored as %v to %v; want the buffers in the period", b.From, b.To)
	}
	if b.Reason != "" {
		t.Error("the rental desk was handed why it was booked")
	}
	if _, err := rent(pool, rentalDesk(), res, in2031(1, 12).Add(30*time.Minute), in2031(1, 15)); !errors.Is(err, workshop.ErrResourceTaken) ||
		err.Error() != "workshop: invalid: it is taken then by a rental" {
		t.Errorf("a rental in the cleaning after another: %v", err)
	}
	if _, err := rent(pool, rentalDesk(), res, in2031(1, 13), in2031(1, 15)); err != nil {
		t.Errorf("a rental from the end of the buffer: %v", err)
	}
	if _, err := workshop.BookResource(ctx, pool, rentalDesk(), workshop.ResourceBooking{
		ResourceID: res, Kind: workshop.BlockService, From: in2031(3, 8)}); err != nil {
		t.Fatalf("an open-ended service block: %v", err)
	}
	if _, err := rent(pool, rentalDesk(), res, in2031(20, 8), in2031(20, 9)); !errors.Is(err, workshop.ErrResourceTaken) {
		t.Errorf("a rental after an open-ended block: %v", err)
	}

	// Who books what, and only in a shop that runs its module.
	noRental := rentalDesk()
	noRental.Modules = []access.Module{access.Workshop, access.Dealer}
	for name, c := range map[string]struct {
		s    access.Scope
		kind string
		want error
	}{
		"sales a rental":           {otherRoles(access.Of(access.RoleSales)), workshop.BlockRental, access.ErrForbidden},
		"a technician a service":   {everything(technician()), workshop.BlockService, access.ErrForbidden},
		"the desk with rental off": {noRental, workshop.BlockRental, access.ErrModuleOff},
		"anyone a workshop block":  {owner(), workshop.BlockWorkshop, workshop.ErrInvalid},
		"the desk a test drive":    {rentalDesk(), workshop.BlockTestDrive, access.ErrForbidden},
	} {
		if _, err := workshop.BookResource(ctx, pool, c.s, workshop.ResourceBooking{ResourceID: res, Kind: c.kind,
			From: in2031(25, 8), To: in2031(25, 9)}); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", name, err, c.want)
		}
	}
	if _, err := workshop.CreateResource(ctx, pool, advisor(), workshop.NewResource{Name: "x"}); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("the workshop's front desk made a resource: %v", err)
	}
	if _, err := workshop.ResourceBlocks(ctx, pool, technician(), res, in2031(1, 0), time.Time{}); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("a technician read the ledger: %v", err)
	}
}

// Two bookings of the same time, made at the same moment in two
// transactions: at most one stands.
func TestTwoBookingsAtOnceLeaveAtMostOne(t *testing.T) {
	pool := setup(t)
	res := trailer(t, pool)
	const tries = 8
	var wg sync.WaitGroup
	errs := make([]error, tries)
	for i := range tries {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = rent(pool, rentalDesk(), res, in2031(2, 9), in2031(2, 17))
		}()
	}
	wg.Wait()
	ok := 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case !errors.Is(err, workshop.ErrResourceTaken):
			t.Errorf("a losing booking: %v, want the resource taken", err)
		}
	}
	if ok != 1 {
		t.Errorf("%d of %d bookings of the same time stood, want 1", ok, tries)
	}
}

// A hold runs out, and once it has it blocks nothing, whether or not a
// sweeper has been round: the next booking lets it go.
func TestAHoldThatHasRunOutBlocksNothing(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	res := trailer(t, pool)
	until := time.Now().Add(time.Hour)
	hold, err := workshop.BookResource(ctx, pool, rentalDesk(), workshop.ResourceBooking{
		ResourceID: res, Kind: workshop.BlockRental, From: in2031(4, 9), To: in2031(4, 17), HoldUntil: &until})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rent(pool, rentalDesk(), res, in2031(4, 10), in2031(4, 11)); !errors.Is(err, workshop.ErrResourceTaken) {
		t.Errorf("a booking over a live hold: %v", err)
	}
	// An hour later.
	// As the database's owner: the application may not move an expiry.
	if _, err := pool.Exec(ctx, `UPDATE resource_blocks SET expires_at = now() - interval '1 minute' WHERE id = $1`, hold); err != nil {
		t.Fatal(err)
	}
	blocks, _ := workshop.ResourceBlocks(ctx, pool, rentalDesk(), res, in2031(4, 0), in2031(5, 0))
	if len(blocks) != 1 || blocks[0].Status != workshop.BlockExpired || blocks[0].Active(time.Now()) {
		t.Errorf("a hold run out reads as %+v", blocks)
	}
	if err := workshop.ConfirmBlock(ctx, pool, rentalDesk(), hold); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a hold confirmed after it ran out: %v", err)
	}
	if _, err := rent(pool, rentalDesk(), res, in2031(4, 10), in2031(4, 11)); err != nil {
		t.Errorf("a booking over a hold that ran out: %v", err)
	}
	// One confirmed in time stands.
	until = time.Now().Add(time.Hour)
	live, err := workshop.BookResource(ctx, pool, rentalDesk(), workshop.ResourceBooking{
		ResourceID: res, Kind: workshop.BlockRental, From: in2031(6, 9), To: in2031(6, 17), HoldUntil: &until})
	if err != nil {
		t.Fatal(err)
	}
	if err := workshop.ConfirmBlock(ctx, pool, rentalDesk(), live); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM resource_blocks WHERE id = $1 AND status = 'confirmed'`, live); n != 1 {
		t.Error("the confirmed hold is not a booking")
	}
}

// A request replayed from a phone finds the block it made.
func TestAReplayedBookingMakesNoSecondBlock(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	res := trailer(t, pool)
	book := func() (string, error) {
		return workshop.BookResource(ctx, pool, rentalDesk(), workshop.ResourceBooking{
			ResourceID: res, Kind: workshop.BlockRental, From: in2031(7, 9), To: in2031(7, 17), Key: "phone-7"})
	}
	first, err := book()
	if err != nil {
		t.Fatal(err)
	}
	again, err := book()
	if err != nil || again != first {
		t.Errorf("the replay: %s, %v; want %s", again, err, first)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM resource_blocks WHERE resource_id = $1`, res); n != 1 {
		t.Errorf("%d blocks after a replay, want 1", n)
	}
}

// A job on a vehicle the business controls blocks it for as long as the job
// is open, in the job's own transaction: the car in the workshop is not
// rented until somebody who runs the shop replans it, which is recorded.
// Cancelled, the job lets the car go.
func TestACarInTheWorkshopIsNotRentedWithoutReplanning(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	res, err := workshop.CreateResource(ctx, pool, rentalDesk(), workshop.NewResource{VehicleID: vehicleA})
	if err != nil {
		t.Fatal(err)
	}
	if again, err := workshop.CreateResource(ctx, pool, owner(), workshop.NewResource{VehicleID: vehicleA}); err != nil || again != res {
		t.Errorf("the same vehicle made a resource again: %s, %v; want %s", again, err, res)
	}
	job, err := workshop.TakeIn(ctx, pool, advisor(), "ABC 12D", "Bromsar", nil)
	if err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM resource_blocks WHERE source_id = $1 AND kind = 'workshop' AND upper_inf(period)`, job); n != 1 {
		t.Fatalf("%d open blocks for the job, want 1", n)
	}
	if _, err := rent(pool, rentalDesk(), res, in2031(8, 9), in2031(8, 17)); err == nil || err.Error() != "workshop: invalid: it is taken then by a job in the workshop" {
		t.Errorf("a car in the workshop rented: %v", err)
	}
	// Replanned by somebody who runs the shop, with a reason, and recorded.
	var block string
	if err := database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT id FROM resource_blocks WHERE source_id = $1`, job).Scan(&block)
	}); err != nil {
		t.Fatal(err)
	}
	if err := workshop.OverrideBlock(ctx, pool, rentalDesk(), block, time.Time{}, time.Time{}, "Kunden väntar"); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("the rental desk replanned the workshop: %v", err)
	}
	if err := workshop.OverrideBlock(ctx, pool, owner(), block, time.Time{}, time.Time{}, ""); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a replanning with no reason: %v", err)
	}
	if err := workshop.OverrideBlock(ctx, pool, owner(), block, time.Time{}, time.Time{}, "Bromsarna kan vänta, kunden behöver bilen"); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM resource_block_changes WHERE block_id = $1 AND change = 'ended' AND reason <> ''`, block); n != 1 {
		t.Errorf("%d recorded replannings, want 1", n)
	}
	rental, err := rent(pool, rentalDesk(), res, in2031(8, 9), in2031(8, 17))
	if err != nil {
		t.Fatalf("the car rented after replanning: %v", err)
	}
	// A job opened on it while it is rented out later: refused, naming the
	// rental, in the job's own transaction.
	if _, err := workshop.TakeIn(ctx, pool, advisor(), "ABC 12D", "Lampa", nil); !errors.Is(err, workshop.ErrResourceTaken) ||
		err.Error() != "workshop: invalid: it is taken then by a rental" {
		t.Errorf("a job opened on a car rented out later: %v", err)
	}
	if err := workshop.ReleaseBlock(ctx, pool, rentalDesk(), rental); err != nil {
		t.Fatal(err)
	}

	// A job cancelled lets the car go from then.
	second, err := workshop.TakeIn(ctx, pool, advisor(), "ABC 12D", "Lampa", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := workshop.SetState(ctx, pool, advisor(), second, workshop.StateCancelled); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM resource_blocks WHERE source_id = $1 AND NOT upper_inf(period)`, second); n != 1 {
		t.Error("the cancelled job's block did not end")
	}
	if _, err := rent(pool, rentalDesk(), res, in2031(8, 9), in2031(8, 17)); err != nil {
		t.Errorf("the car rented after its job was cancelled: %v", err)
	}
}

// A resource is taken out of use only when nothing is booked on it ahead.
func TestAResourceWithBookingsAheadIsNotRetired(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	res := trailer(t, pool)
	id, err := rent(pool, rentalDesk(), res, in2031(9, 9), in2031(9, 17))
	if err != nil {
		t.Fatal(err)
	}
	if err := workshop.RetireResource(ctx, pool, rentalDesk(), res); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("retired with a rental ahead: %v", err)
	}
	if err := workshop.ReleaseBlock(ctx, pool, rentalDesk(), id); err != nil {
		t.Fatal(err)
	}
	if err := workshop.RetireResource(ctx, pool, rentalDesk(), res); err != nil {
		t.Fatal(err)
	}
	if _, err := rent(pool, rentalDesk(), res, in2031(10, 9), in2031(10, 17)); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a retired resource booked: %v", err)
	}
}

// A car in the workshop may have several jobs open at once. They share one
// block: the second job joins the first's, and the car is free again when
// the last of them ends, not the first.
func TestACarsOpenJobsShareOneWorkshopBlock(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	res, err := workshop.CreateResource(ctx, pool, rentalDesk(), workshop.NewResource{VehicleID: vehicleA})
	if err != nil {
		t.Fatal(err)
	}
	first, err := workshop.TakeIn(ctx, pool, advisor(), "ABC 12D", "Bromsar", nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := workshop.TakeIn(ctx, pool, advisor(), "ABC 12D", "Avgasröret igen", nil)
	if err != nil {
		t.Fatalf("a second job on a car already in the workshop: %v", err)
	}
	active := `SELECT count(*) FROM resource_blocks WHERE resource_id = $1 AND kind = 'workshop' AND upper_inf(period)`
	if n := countRows(t, pool, active, res); n != 1 {
		t.Fatalf("%d open workshop blocks, want the one both jobs share", n)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM resource_block_jobs WHERE work_order_id IN ($1, $2)`, first, second); n != 2 {
		t.Errorf("%d jobs recorded as sharing the block, want 2", n)
	}
	if err := workshop.SetState(ctx, pool, advisor(), first, workshop.StateCancelled); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, pool, active, res); n != 1 {
		t.Error("the block ended with the first job, while the second still has the car")
	}
	if _, err := rent(pool, rentalDesk(), res, in2031(8, 9), in2031(8, 17)); !errors.Is(err, workshop.ErrResourceTaken) {
		t.Errorf("rented while a job is still open on it: %v", err)
	}
	if err := workshop.SetState(ctx, pool, advisor(), second, workshop.StateCancelled); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, pool, active, res); n != 0 {
		t.Error("the block did not end with the last job")
	}
	if _, err := rent(pool, rentalDesk(), res, in2031(8, 9), in2031(8, 17)); err != nil {
		t.Errorf("rented once every job has ended: %v", err)
	}
}

// A car sold and bought back is the same resource, and bookable again: made
// a resource once more, it comes back into use, and each period it was out
// of use stays as a row.
func TestAResourceTakenOutOfUseCanComeBack(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	res, err := workshop.CreateResource(ctx, pool, rentalDesk(), workshop.NewResource{VehicleID: vehicleA})
	if err != nil {
		t.Fatal(err)
	}
	if err := workshop.RetireResource(ctx, pool, rentalDesk(), res); err != nil {
		t.Fatal(err)
	}
	if _, err := rent(pool, rentalDesk(), res, in2031(11, 9), in2031(11, 17)); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a retired resource booked: %v", err)
	}
	back, err := workshop.CreateResource(ctx, pool, rentalDesk(), workshop.NewResource{VehicleID: vehicleA})
	if err != nil || back != res {
		t.Fatalf("the car bought back: %s, %v; want %s", back, err, res)
	}
	rental, err := rent(pool, rentalDesk(), res, in2031(11, 9), in2031(11, 17))
	if err != nil {
		t.Fatalf("the car bought back is not bookable: %v", err)
	}
	if err := workshop.ReleaseBlock(ctx, pool, rentalDesk(), rental); err != nil {
		t.Fatal(err)
	}
	if _, err := workshop.TakeIn(ctx, pool, advisor(), "ABC 12D", "Service", nil); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM resource_blocks WHERE resource_id = $1 AND kind = 'workshop'`, res); n != 1 {
		t.Errorf("a job on the car bought back made %d workshop blocks, want 1", n)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM resource_retirements WHERE resource_id = $1 AND returned_at IS NOT NULL`, res); n != 1 {
		t.Errorf("%d periods out of use recorded, want the one that ended", n)
	}
}

// A phone retries a booking while its first request is still in flight:
// the retry waits for the first and answers with the block it made, not
// with a refusal of its own booking.
func TestAReplayRacingTheFirstRequestFindsItsBlock(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	res := trailer(t, pool)
	// The first request, written and not yet committed.
	first, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Rollback(ctx)
	var id string
	if err := first.QueryRow(ctx, `
		SELECT set_config('app.current_shop', $1, true)`, shopID).Scan(new(string)); err != nil {
		t.Fatal(err)
	}
	if err := first.QueryRow(ctx, `
		INSERT INTO resource_blocks (shop_id, resource_id, kind, status, period, request_key)
		VALUES ($1, $2, 'rental', 'confirmed', tstzrange($3, $4), 'phone-race') RETURNING id::text`,
		shopID, res, in2031(12, 9), in2031(12, 17)).Scan(&id); err != nil {
		t.Fatal(err)
	}
	type answer struct {
		id  string
		err error
	}
	replay := make(chan answer, 1)
	go func() {
		got, err := workshop.BookResource(ctx, pool, rentalDesk(), workshop.ResourceBooking{
			ResourceID: res, Kind: workshop.BlockRental, From: in2031(12, 9), To: in2031(12, 17), Key: "phone-race"})
		replay <- answer{got, err}
	}()
	// Long enough for the replay to reach the insert and wait on the first.
	time.Sleep(300 * time.Millisecond)
	if err := first.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	a := <-replay
	if a.err != nil || a.id != id {
		t.Errorf("the replay racing the first request: %s, %v; want %s", a.id, a.err, id)
	}
}
