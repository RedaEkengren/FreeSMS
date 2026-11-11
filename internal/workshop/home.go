package workshop

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/database"
	"github.com/RedaEkengren/FreeSMS/internal/money"
)

// The home page (#157): one page for everybody, answering "what do I need
// to do now?" and "how is the business doing?". The modules the shop runs
// decide which cards can appear, the role which may, and actions come
// before information. Each card is the small typed result of one query
// asked under the caller's scope when the page is read -- never a total
// kept somewhere, nor one shared between people who may see different
// things -- and its source checks the role before it asks.

// CardKind says whether a card is something to do or something to know.
type CardKind string

// The kinds of card.
const (
	CardAction      CardKind = "action"
	CardInformation CardKind = "information"
)

// Card is one figure on the home page.
type Card struct {
	Module access.Module
	Kind   CardKind
	// What it is, as a catalogue key; with %d, it takes Count.
	Title string
	Count int
	// An amount, for a card about money; Money says it is one.
	Minor int64
	Money bool
	Link  string
}

// Amount is the card's money, formatted.
func (c Card) Amount() string { return money.Format(c.Minor) }

// CardSource is how a module puts a card on the home page: its module, the
// roles that may see it, and the query that reads it. Read is asked only of
// a role May allows, in a module the shop runs.
type CardSource struct {
	Module access.Module
	Kind   CardKind
	Title  string
	Link   string
	May    func(access.Roles) bool
	Read   func(ctx context.Context, tx pgx.Tx, scope access.Scope) (count int, minor int64, err error)
	// JobsWhere, instead of Read, is a count of the workshop's jobs where
	// it holds, a condition on work_orders w. Every such card the caller
	// may see is counted in one statement, one FILTER each: one round trip
	// on the workshop's wifi, not one per card.
	JobsWhere string
	// RentalWhere is JobsWhere for Rental: a count of the rental bookings
	// where it holds, a condition on rental_bookings b, its block k and
	// the shop's today t (rentalToday), every one in one statement.
	RentalWhere string
	// StockWhere is JobsWhere for Dealer: a count of the open stock units
	// where it holds, a condition on stock_units u, every one in one
	// statement.
	StockWhere string
	Money      bool
}

// cardSources are the home page's cards, in the order they come. The
// workshop's sum up the board or the job list beneath them on the same
// page, so they link nowhere; a module's card that has a page of its own
// links to it. Dealer
// and Rental add theirs, through the same type, with the issues that build
// what they count.
var cardSources = []CardSource{
	{Module: access.Workshop, Kind: CardAction, Title: "%d car ready",
		May: access.Roles.SeesCustomerPersonalData, JobsWhere: `w.state = 'ready'`},
	{Module: access.Workshop, Kind: CardAction, Title: "%d job waiting for the customer's decision",
		May: access.Roles.SeesCustomerPersonalData, JobsWhere: `w.state = 'awaiting_approval'`},
	{Module: access.Workshop, Kind: CardAction, Title: "%d job waiting for parts",
		May: access.Roles.WorksInTheWorkshop, JobsWhere: `w.state = 'awaiting_parts'`},
	{Module: access.Workshop, Kind: CardAction, Title: "%d job past its promised time",
		May:       access.Roles.WorksInTheWorkshop,
		JobsWhere: `w.promised_at < clock_timestamp() AND w.state NOT IN ('ready', 'invoiced', 'closed', 'cancelled')`},
	{Module: access.Workshop, Kind: CardInformation, Title: "%d job in progress",
		May: access.Roles.RunsTheShop, JobsWhere: openJob},
	{Module: access.Workshop, Kind: CardInformation, Title: "Work ready to invoice, before VAT",
		May: access.Roles.RunsTheShop, Read: readyToInvoiceMinor, Money: true},
	// Dealer: the cars (#157). The stock value is a sum of purchase prices,
	// so only for a role that may see them.
	{Module: access.Dealer, Kind: CardAction, Title: "%d sold car to deliver", Link: "/cars/stock",
		May: access.Roles.SellsVehicles, StockWhere: `u.status = 'sold'`},
	{Module: access.Dealer, Kind: CardAction, Title: "%d reservation runs out within a day", Link: "/cars/stock",
		May: access.Roles.SellsVehicles, StockWhere: reservationRunsOut},
	{Module: access.Dealer, Kind: CardAction, Title: "%d car waiting to be made ready", Link: "/cars/stock",
		May:        func(r access.Roles) bool { return r.SellsVehicles() || r.BuysVehicles() },
		StockWhere: `u.status = 'in_preparation'`},
	{Module: access.Dealer, Kind: CardInformation, Title: "Cars in stock, at purchase price", Link: "/cars/stock",
		May: access.Roles.SeesPurchasePrice, Read: stockAtPurchasePrice, Money: true},
	// Rental
	{Module: access.Rental, Kind: CardAction, Title: "%d rental to hand over today", Link: "/rental",
		May: access.Roles.HandsOverRentals, RentalWhere: rentalHandoverToday},
	{Module: access.Rental, Kind: CardAction, Title: "%d rental due back today", Link: "/rental",
		May: access.Roles.HandsOverRentals, RentalWhere: rentalReturnToday},
	{Module: access.Rental, Kind: CardAction, Title: "%d rental out past its return", Link: "/rental",
		May: access.Roles.HandsOverRentals, RentalWhere: rentalOverdue},
	{Module: access.Rental, Kind: CardAction, Title: "%d rental with damage nobody has assessed", Link: "/rental",
		May: access.Roles.HandsOverRentals, RentalWhere: rentalDamageToAssess},
	{Module: access.Rental, Kind: CardInformation, Title: "%d rental unit out now", Link: "/rental",
		May: access.Roles.RunsTheShop, RentalWhere: rentalOutNow},
	{Module: access.Rental, Kind: CardInformation, Title: "Rental invoiced this month, before VAT", Link: "/rental",
		May: access.Roles.RunsTheShop, Read: rentalInvoicedThisMonth, Money: true},
}

// countJobs counts the workshop's jobs for every card that counts them, in
// one statement: the counts in the cards' order.
func countJobs(ctx context.Context, tx pgx.Tx, cards []CardSource) ([]int, error) {
	if len(cards) == 0 {
		return nil, nil
	}
	cols := make([]string, len(cards))
	for i, c := range cards {
		cols[i] = `count(*) FILTER (WHERE ` + c.JobsWhere + `)`
	}
	counts := make([]int, len(cards))
	dest := make([]any, len(cards))
	for i := range counts {
		dest[i] = &counts[i]
	}
	err := tx.QueryRow(ctx, `SELECT `+strings.Join(cols, ", ")+` FROM work_orders w`).Scan(dest...)
	return counts, err
}

// readyToInvoiceMinor is what the lines an invoice made now would carry on
// the jobs that are ready come to before VAT, a line at a time as the
// invoice rounds them; the shop's surcharges are left out.
func readyToInvoiceMinor(ctx context.Context, tx pgx.Tx, _ access.Scope) (int, int64, error) {
	rows, err := tx.Query(ctx, `
		SELECT (l.quantity * 1000)::bigint, l.unit_price_minor, l.cost_bearer, l.approved_at, l.declined_at, l.withdrawn_at,
		       l.customer_decision_id IS NOT NULL
		FROM work_order_lines l JOIN work_orders w ON w.id = l.work_order_id WHERE w.state = 'ready'`)
	if err != nil {
		return 0, 0, err
	}
	defer rows.Close()
	var total int64
	n := 0
	for rows.Next() {
		var l Line
		if err := rows.Scan(&l.QuantityMilli, &l.UnitPriceMinor, &l.CostBearer, &l.ApprovedAt, &l.DeclinedAt, &l.WithdrawnAt, &l.FromAnswer); err != nil {
			return 0, 0, err
		}
		if l.Billable() {
			total += l.NetMinor()
			n++
		}
	}
	return n, total, rows.Err()
}

// Home is what the home page shows the caller.
type Home struct {
	Actions     []Card
	Information []Card
	// The first of what is waiting for the caller, and how many more.
	Waiting     []ActionItem
	MoreWaiting int
	// A card or a part of the queue could not be read: the rest is shown.
	Incomplete bool
}

// homeWaiting is how many of the queue's items the home page shows.
const homeWaiting = 5

// HomeFor reads the caller's home page: each card a module the shop runs
// offers and the caller's role may see, and the top of the one queue. A
// card whose query fails is left out, and the page says something is
// missing; a card with nothing to count is left out too, so a module with
// no data yet shows nothing. Every role may ask: each card checks its own.
func HomeFor(ctx context.Context, pool *pgxpool.Pool, scope access.Scope) (Home, []error) {
	var h Home
	var failed []error
	var shown, counted, rented, stocked []CardSource
	for _, c := range cardSources {
		if !scope.Uses(c.Module) || !c.May(scope.Role) {
			continue
		}
		shown = append(shown, c)
		if c.JobsWhere != "" {
			counted = append(counted, c)
		}
		if c.RentalWhere != "" {
			rented = append(rented, c)
		}
		if c.StockWhere != "" {
			stocked = append(stocked, c)
		}
	}
	// Every card is counted in one transaction: the workshop's jobs in one
	// statement, the rental's in one, and the rest one statement each. One
	// round trip to open it, not one per card; if it fails the cards fail
	// together and the page says some are missing.
	type reading struct {
		n     int
		minor int64
	}
	var jobs, rentals, stock []int
	read := map[string]reading{}
	if len(shown) > 0 {
		err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			if len(counted) > 0 {
				if jobs, err = countJobs(ctx, tx, counted); err != nil {
					return err
				}
			}
			if len(rented) > 0 {
				if rentals, err = countRentals(ctx, tx, rented); err != nil {
					return err
				}
			}
			if len(stocked) > 0 {
				if stock, err = countStock(ctx, tx, stocked); err != nil {
					return err
				}
			}
			for _, c := range shown {
				if c.Read == nil {
					continue
				}
				n, minor, err := c.Read(ctx, tx, scope)
				if err != nil {
					return fmt.Errorf("card %q: %w", c.Title, err)
				}
				read[c.Title] = reading{n, minor}
			}
			return nil
		})
		if err != nil {
			jobs, rentals, stock, read = nil, nil, nil, map[string]reading{}
			failed = append(failed, fmt.Errorf("the cards counted together: %w", err))
		}
	}
	for _, c := range shown {
		var n int
		var minor int64
		switch {
		case c.JobsWhere != "":
			// In the order counted, which is the order shown.
			if len(jobs) == 0 {
				continue
			}
			n, jobs = jobs[0], jobs[1:]
		case c.RentalWhere != "":
			if len(rentals) == 0 {
				continue
			}
			n, rentals = rentals[0], rentals[1:]
		case c.StockWhere != "":
			if len(stock) == 0 {
				continue
			}
			n, stock = stock[0], stock[1:]
		default:
			r, ok := read[c.Title]
			if !ok {
				continue
			}
			n, minor = r.n, r.minor
		}
		if n == 0 {
			continue
		}
		card := Card{Module: c.Module, Kind: c.Kind, Title: c.Title, Count: n, Minor: minor, Money: c.Money, Link: c.Link}
		if c.Kind == CardAction {
			h.Actions = append(h.Actions, card)
		} else {
			h.Information = append(h.Information, card)
		}
	}
	items, err := WaitingFor(ctx, pool, scope)
	var incomplete *IncompleteError
	switch {
	case errors.As(err, &incomplete):
		failed = append(failed, incomplete.Failed...)
	case err != nil:
		failed = append(failed, err)
	}
	if len(items) > homeWaiting {
		h.MoreWaiting = len(items) - homeWaiting
		items = items[:homeWaiting]
	}
	h.Waiting = items
	h.Incomplete = failed != nil
	return h, failed
}

// reservationRunsOut holds for a stock unit with a reservation standing
// that runs out within a day or already has: the same facts as the queue's.
const reservationRunsOut = `EXISTS (SELECT 1 FROM vehicle_reservations r WHERE r.stock_unit_id = u.id
	AND r.ended_at IS NULL AND r.expires_at <= clock_timestamp() + interval '1 day')`

// countStock counts the open stock units for every card that counts them, in
// one statement: the counts in the cards' order.
func countStock(ctx context.Context, tx pgx.Tx, cards []CardSource) ([]int, error) {
	if len(cards) == 0 {
		return nil, nil
	}
	cols := make([]string, len(cards))
	for i, c := range cards {
		cols[i] = `count(*) FILTER (WHERE ` + c.StockWhere + `)`
	}
	counts := make([]int, len(cards))
	dest := make([]any, len(cards))
	for i := range counts {
		dest[i] = &counts[i]
	}
	err := tx.QueryRow(ctx, `SELECT `+strings.Join(cols, ", ")+` FROM stock_units u WHERE u.closed_at IS NULL`).Scan(dest...)
	return counts, err
}

// stockAtPurchasePrice is the open stock's cars and what they were bought
// for, in kronor, minor units. A car with no purchase on record (a
// consignment) adds no price. Asked only of a role that sees purchase
// prices; it is a sum made when the page is read, never kept.
func stockAtPurchasePrice(ctx context.Context, tx pgx.Tx, _ access.Scope) (int, int64, error) {
	var n int
	var minor int64
	err := tx.QueryRow(ctx, `
		SELECT count(*), coalesce(sum(p.tax_purchase_price_minor), 0)::bigint
		FROM stock_units u LEFT JOIN vehicle_purchases p ON p.stock_unit_id = u.id
		WHERE u.closed_at IS NULL AND u.status IN ('in_preparation', 'for_sale', 'reserved')`).Scan(&n, &minor)
	return n, minor, err
}
