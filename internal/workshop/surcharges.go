package workshop

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/database"
	"github.com/RedaEkengren/FreeSMS/internal/money"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Surcharges are what a shop adds to an invoice besides the work and the
// parts: förbrukningsmaterial, a share of the labour, and a
// faktureringsavgift, a flat fee per invoice.
type Surcharges struct {
	// Basis points of the labour (500 = 5%). Zero charges nothing.
	ConsumablesBasis int
	// A ceiling on the consumables per invoice, when the shop has one.
	ConsumablesCapMinor *int64
	// Charged once on every original invoice. Zero charges nothing.
	InvoiceFeeMinor int64
}

// ConsumablesPercent is the basis as a person writes it: 5,0.
func (s Surcharges) ConsumablesPercent() string {
	return fmt.Sprintf("%d.%d", s.ConsumablesBasis/100, (s.ConsumablesBasis%100)/10)
}

// Charged reports whether there is anything to add at all.
func (s Surcharges) Charged() bool { return s.ConsumablesBasis > 0 || s.InvoiceFeeMinor > 0 }

// SurchargeLine is one line the surcharges add, as a page shows it before
// the invoice exists.
type SurchargeLine struct {
	Kind         string
	Description  string
	NetMinor     int64
	VATRateBasis int
}

func readSurcharges(ctx context.Context, tx pgx.Tx) (Surcharges, string, error) {
	var s Surcharges
	var locale string
	err := tx.QueryRow(ctx, `
		SELECT consumables_basis, consumables_cap_minor, invoice_fee_minor, locale FROM shops LIMIT 1`).
		Scan(&s.ConsumablesBasis, &s.ConsumablesCapMinor, &s.InvoiceFeeMinor, &locale)
	if err != nil {
		return s, "", fmt.Errorf("read surcharges: %w", err)
	}
	return s, locale, nil
}

// SurchargesFor reads the shop's settings, for the front desk and the owner.
func SurchargesFor(ctx context.Context, pool *pgxpool.Pool, scope access.Scope) (Surcharges, error) {
	if !scope.Role.SeesCustomerPersonalData() {
		return Surcharges{}, access.ErrForbidden
	}
	var s Surcharges
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		s, _, err = readSurcharges(ctx, tx)
		return err
	})
	return s, err
}

// SaveSurcharges changes them, for whoever runs the shop. Invoices already
// issued keep what they were charged: it is on them as lines.
func SaveSurcharges(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, s Surcharges) error {
	if !scope.Role.RunsTheShop() {
		return access.ErrForbidden
	}
	switch {
	case s.ConsumablesBasis < 0 || s.ConsumablesBasis > 2500:
		return fmt.Errorf("%w: förbrukningsmaterial between 0 and 25 per cent of the labour", ErrInvalid)
	case s.ConsumablesCapMinor != nil && *s.ConsumablesCapMinor < 0, s.InvoiceFeeMinor < 0:
		return fmt.Errorf("%w: an amount cannot be below nothing", ErrInvalid)
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE shops SET consumables_basis = $2, consumables_cap_minor = $3, invoice_fee_minor = $4
			WHERE id = $1`, scope.ShopID, s.ConsumablesBasis, s.ConsumablesCapMinor, s.InvoiceFeeMinor)
		return err
	})
}

// surchargeLines works out what the surcharges add to these lines -- the
// ones the customer pays for -- as lines of their own. The one place it is
// worked out: Issue writes what it returns, and the job page shows the same,
// so the page and the invoice cannot disagree.
//
//   - Förbrukningsmaterial is a share of the labour net, not of the whole
//     order: charging it on parts is wrong and a customer will notice. It is
//     VAT-rated like the labour it follows, so a share is taken at each
//     labour rate. A ceiling cuts the total down, each rate in proportion,
//     the last öre to the largest so the lines add up to the ceiling exactly.
//   - The invoicing fee is charged on an original invoice only. Crediting an
//     invoice gives the original's fee back; it does not charge a new one.
func surchargeLines(s Surcharges, locale string, lines []invoiceLine, original bool) []invoiceLine {
	var out []invoiceLine
	t := surchargeText(locale)

	if s.ConsumablesBasis > 0 {
		labour := map[int]int64{}
		for _, l := range lines {
			if l.Kind == "labour" {
				labour[l.VATRateBasis] += l.NetMinor
			}
		}
		rates := make([]int, 0, len(labour))
		for r := range labour {
			rates = append(rates, r)
		}
		sort.Sort(sort.Reverse(sort.IntSlice(rates)))

		shares := make([]int64, len(rates))
		var total int64
		for i, r := range rates {
			shares[i] = money.Share(labour[r], s.ConsumablesBasis)
			total += shares[i]
		}
		if cap := s.ConsumablesCapMinor; cap != nil && total > *cap {
			var cut, largest int64
			big := 0
			for i := range shares {
				if shares[i] > largest {
					largest, big = shares[i], i
				}
				shares[i] = money.Scale(shares[i], *cap, total)
				cut += shares[i]
			}
			shares[big] += *cap - cut
		}
		pct := s.ConsumablesPercent()
		if locale == "sv" {
			pct = strings.Replace(pct, ".", ",", 1)
		}
		text := t("Consumables, %s%% of the labour", pct)
		for i, r := range rates {
			if shares[i] == 0 {
				continue
			}
			out = append(out, chargeLine("consumables", text, shares[i], r))
		}
	}
	if original && s.InvoiceFeeMinor > 0 {
		out = append(out, chargeLine("fee", t("Invoicing fee"), s.InvoiceFeeMinor, 2500))
	}
	return out
}

func chargeLine(kind, text string, net int64, rate int) invoiceLine {
	return invoiceLine{
		Kind: kind, Description: text, QuantityMilli: 1000, UnitPriceMinor: net,
		VATRateBasis: rate, NetMinor: net, VATMinor: money.VAT(net, rate),
	}
}

// PreviewSurcharges is what the surcharges would add to a job invoiced now.
func PreviewSurcharges(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, jobID string) ([]SurchargeLine, error) {
	if !scope.Role.SeesCustomerPersonalData() {
		return nil, access.ErrForbidden
	}
	if !looksLikeUUID(jobID) {
		return nil, ErrNotFound
	}
	var out []SurchargeLine
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		s, locale, err := readSurcharges(ctx, tx)
		if err != nil || !s.Charged() {
			return err
		}
		lines, err := chargeableLines(ctx, tx, jobID)
		if err != nil || len(lines) == 0 {
			return err
		}
		for _, l := range surchargeLines(s, locale, lines, true) {
			out = append(out, SurchargeLine{Kind: l.Kind, Description: l.Description, NetMinor: l.NetMinor, VATRateBasis: l.VATRateBasis})
		}
		return nil
	})
	return out, err
}

// The words on a surcharge line are the system's, not the shop's, and are
// written in the shop's language: they go onto a document the customer
// reads and that is never re-rendered in another.
func surchargeText(locale string) func(string, ...any) string {
	c := catalogues()
	if c == nil {
		// The catalogues are embedded, so this is not expected; the English
		// key still says something sensible on an invoice.
		return func(key string, args ...any) string { return fmt.Sprintf(key, args...) }
	}
	return c.For(locale).T
}

// TotalsWith is TotalsFor with the surcharges a job would be invoiced with,
// so the total the front desk reads out is the total the invoice will print.
func TotalsWith(lines []Line, extra []SurchargeLine) money.Totals {
	priced := make([]money.Line, 0, len(lines)+len(extra))
	for _, l := range lines {
		priced = append(priced, money.Line{QuantityMilli: l.QuantityMilli, UnitPriceMinor: l.UnitPriceMinor,
			VATRateBasis: l.VATRateBasis, ChargedToCustomer: l.CostBearer == "customer"})
	}
	for _, l := range extra {
		priced = append(priced, money.Line{QuantityMilli: 1000, UnitPriceMinor: l.NetMinor,
			VATRateBasis: l.VATRateBasis, ChargedToCustomer: true})
	}
	return money.Compute(priced)
}
