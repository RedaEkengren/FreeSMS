package workshop

import (
	"context"
	"errors"
	"fmt"

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
	Money  bool
}

// cardSources are the home page's cards, in the order they come. The
// workshop's sum up the board or the job list beneath them on the same
// page, so they link nowhere; a module's card that has a page of its own
// links to it. Dealer
// and Rental add theirs, through the same type, with the issues that build
// what they count.
var cardSources = []CardSource{
	{Module: access.Workshop, Kind: CardAction, Title: "%d car ready",
		May: access.Roles.SeesCustomerPersonalData, Read: jobsIn(`w.state = 'ready'`)},
	{Module: access.Workshop, Kind: CardAction, Title: "%d job waiting for the customer's decision",
		May: access.Roles.SeesCustomerPersonalData, Read: jobsIn(`w.state = 'awaiting_approval'`)},
	{Module: access.Workshop, Kind: CardAction, Title: "%d job waiting for parts",
		May: access.Roles.WorksInTheWorkshop, Read: jobsIn(`w.state = 'awaiting_parts'`)},
	{Module: access.Workshop, Kind: CardAction, Title: "%d job past its promised time",
		May:  access.Roles.WorksInTheWorkshop,
		Read: jobsIn(`w.promised_at < clock_timestamp() AND w.state NOT IN ('ready', 'invoiced', 'closed', 'cancelled')`)},
	{Module: access.Workshop, Kind: CardInformation, Title: "%d job in progress",
		May: access.Roles.RunsTheShop, Read: jobsIn(openJob)},
	{Module: access.Workshop, Kind: CardInformation, Title: "Work ready to invoice, before VAT",
		May: access.Roles.RunsTheShop, Read: readyToInvoiceMinor, Money: true},
}

// jobsIn counts the jobs where says, a condition on work_orders w.
func jobsIn(where string) func(context.Context, pgx.Tx, access.Scope) (int, int64, error) {
	return func(ctx context.Context, tx pgx.Tx, _ access.Scope) (int, int64, error) {
		var n int
		err := tx.QueryRow(ctx, `SELECT count(*) FROM work_orders w WHERE `+where).Scan(&n)
		return n, 0, err
	}
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
	for _, c := range cardSources {
		if !scope.Uses(c.Module) || !c.May(scope.Role) {
			continue
		}
		var n int
		var minor int64
		err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			n, minor, err = c.Read(ctx, tx, scope)
			return err
		})
		if err != nil {
			failed = append(failed, fmt.Errorf("card %q: %w", c.Title, err))
			continue
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
