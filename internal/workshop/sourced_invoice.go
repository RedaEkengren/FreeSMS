package workshop

import (
	"context"
	"fmt"
	"strings"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/database"
	"github.com/RedaEkengren/FreeSMS/internal/money"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// An invoice that is not a workshop job's (#146): a vehicle sale, a rental,
// or a sale entered by hand. Issued by the same machine as the workshop's --
// a number from the shop's series in the transaction that writes the
// document, the seller and the buyer copied onto it, lines frozen with their
// own arithmetic -- and corrected the same way, by credit notes that keep its
// source.
//
// One series for every source. Swedish bookkeeping allows several, each
// gap-free; one is the simplest to prove gap-free, and what a small business
// expects to see. A series per source, as a shop's setting, waits for the
// first module that issues invoices to need it.

// SourcedLine is one line of an invoice that is not a job's, as its source
// hands it over: what it says, how many, at what price, at what VAT. The
// source decides these; they are frozen here and never recomputed.
type SourcedLine struct {
	// How the line is booked: a kind the accounts know (salesAccounts),
	// "part" -- goods sold -- when empty.
	Kind           string
	Description    string
	QuantityMilli  int64
	UnitPriceMinor int64
	VATRateBasis   int
	// A car sold under the margin scheme (#177): kind "vehicle", at its
	// gross price and rate 0, outside the VAT bands. A vehicle sale's only.
	MarginScheme bool
	// Priced with its VAT (a car's price, #177): the VAT is what is inside
	// it at the rate (money.VATInGross) and the net the rest, so the line
	// comes to exactly this; UnitPriceMinor is then not given.
	GrossMinor int64
}

// MarginSchemeStatement is the statement a margin-scheme invoice carries,
// frozen onto it in Swedish whatever the reader's language (#177).
const MarginSchemeStatement = "Vinstmarginalbeskattning för begagnade varor"

// ManualInvoice is a sale entered by hand: something sold that no module
// records, invoiced to a customer.
type ManualInvoice struct {
	CustomerID string
	Lines      []SourcedLine
}

// sourceModule is the module a source belongs to, which the shop must run
// for an invoice of it to be issued (#143). A manual invoice is Core's.
func sourceModule(source string) (access.Module, bool) {
	switch source {
	case SourceWorkshop:
		return access.Workshop, true
	case SourceVehicleSale:
		return access.Dealer, true
	case SourceRental:
		return access.Rental, true
	case SourceManual:
		return "", true
	}
	return "", false
}

// IssueManual issues an invoice for a sale entered by hand.
//
// Whoever may invoice a job may invoice this: the front desk and those who
// run the shop. Core, so it works whatever modules the shop runs. No page
// calls it in this release: the previous release reads every invoice as a
// job's, so the first invoice that is not one is written by the release
// after (see migration 0065).
func IssueManual(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, m ManualInvoice) (Invoice, error) {
	if !scope.Role.SeesCustomerPersonalData() {
		return Invoice{}, access.ErrForbidden
	}
	if !looksLikeUUID(m.CustomerID) {
		return Invoice{}, ErrNotFound
	}
	var inv Invoice
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		inv, err = issueSourcedTx(ctx, tx, scope, sourced{source: SourceManual, customerID: m.CustomerID}, m.Lines)
		return err
	})
	return inv, err
}

// sourced is what an invoice is of: its source, the sale or the rental when
// it is one, and whom it is to.
type sourced struct {
	source        string
	vehicleSaleID string
	rentalID      string
	customerID    string
	// A margin-scheme sale's VAT, for the books and the VAT return, never
	// shown to the buyer (#177).
	marginSchemeVAT int64
}

// issueSourcedTx writes an invoice that is not a job's, inside the caller's
// transaction, refusing a source whose module the shop does not run. The
// operation that calls it checks the role.
func issueSourcedTx(ctx context.Context, tx pgx.Tx, scope access.Scope, of sourced, given []SourcedLine) (Invoice, error) {
	module, known := sourceModule(of.source)
	switch {
	case !known, of.source == SourceWorkshop:
		return Invoice{}, fmt.Errorf("%w: %q is not a source this issues", ErrInvalid, of.source)
	case module != "":
		if err := scope.Require(module); err != nil {
			return Invoice{}, err
		}
	}
	if len(given) == 0 {
		return Invoice{}, ErrNothingToInvoice
	}
	lines := make([]invoiceLine, 0, len(given))
	for _, g := range given {
		g.Description = strings.TrimSpace(g.Description)
		switch {
		case g.Description == "":
			return Invoice{}, fmt.Errorf("%w: every line says what it is", ErrInvalid)
		case g.QuantityMilli <= 0:
			return Invoice{}, fmt.Errorf("%w: a line is of more than nothing; a correction is a credit note", ErrInvalid)
		case g.VATRateBasis < 0 || g.VATRateBasis > 10000:
			return Invoice{}, fmt.Errorf("%w: a VAT rate is between 0 and 100 per cent", ErrInvalid)
		}
		if g.Kind == "" {
			g.Kind = "part"
		}
		// A car sold is a vehicle sale's, and under the margin scheme
		// carries no VAT of its own on the invoice.
		if g.Kind == "vehicle" || g.MarginScheme {
			if of.source != SourceVehicleSale || g.Kind != "vehicle" || (g.MarginScheme && g.VATRateBasis != 0) {
				return Invoice{}, fmt.Errorf("%w: a car is sold on a vehicle sale's invoice, under the margin scheme at no VAT of its own", ErrInvalid)
			}
		} else if _, booked := salesAccounts[g.Kind]; !booked {
			return Invoice{}, fmt.Errorf("%w: a line is labour, a part, sublet work, consumables or a fee", ErrInvalid)
		}
		net := money.LineNet(g.QuantityMilli, g.UnitPriceMinor)
		vat := money.VAT(net, g.VATRateBasis)
		if g.GrossMinor != 0 {
			if g.QuantityMilli != 1000 || g.UnitPriceMinor != 0 {
				return Invoice{}, fmt.Errorf("%w: a line priced with its VAT is one of it", ErrInvalid)
			}
			vat = money.VATInGross(g.GrossMinor, g.VATRateBasis)
			net = g.GrossMinor - vat
			g.UnitPriceMinor = net
		}
		lines = append(lines, invoiceLine{
			Kind: g.Kind, Description: g.Description, QuantityMilli: g.QuantityMilli,
			UnitPriceMinor: g.UnitPriceMinor, VATRateBasis: g.VATRateBasis,
			NetMinor: net, VATMinor: vat,
			UnitCode: unitCode(g.Kind, ""), MarginScheme: g.MarginScheme,
		})
	}
	// The lines' own amounts, summed: each line's VAT is rounded on its
	// own, so this is what Compute gives -- and a line priced with its VAT
	// keeps the VAT that was inside its price.
	var totals money.Totals
	for _, l := range lines {
		totals.NetMinor += l.NetMinor
		totals.VATMinor += l.VATMinor
	}
	totals.GrossMinor = totals.NetMinor + totals.VATMinor

	b, err := buyerTx(ctx, tx, of.customerID)
	if err != nil {
		return Invoice{}, err
	}
	number, err := nextNumber(ctx, tx, scope.ShopID, DefaultSeries)
	if err != nil {
		return Invoice{}, err
	}
	sold, err := readSeller(ctx, tx, scope.ShopID)
	if err != nil {
		return Invoice{}, err
	}
	const insert = `
		INSERT INTO invoices
		  (shop_id, series, number, issued_by,
		   customer_name, customer_address, customer_org_number, customer_vat_number,
		   seller_name, seller_address, seller_org_number, seller_vat_number,
		   seller_phone, seller_email, seller_payment_reference,
		   seller_payment_terms_days, seller_f_tax,
		   net_minor, vat_minor, gross_minor,
		   ` + electronicColumns + `,
		   source, vehicle_sale_id, rental_id, customer_id, margin_scheme_statement, margin_scheme_vat_minor)
		VALUES ($1,$2,$3,NULLIF($4, '')::uuid,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,
		        $21,$22,$23,$24,$25,$26,$27,$28,$29,$30,$31,$32,$33,
		        $34, NULLIF($35, '')::uuid, NULLIF($36, '')::uuid, $37::uuid, $38, $39)
		RETURNING id, issued_at`
	ocr, err := paymentOCR(DefaultSeries, number)
	if err != nil {
		return Invoice{}, err
	}
	args := append([]any{
		scope.ShopID, DefaultSeries, number, scope.UserID,
		b.name, b.address, b.org, b.vat,
		sold.Name, sold.Address, sold.OrgNumber, sold.VATNumber,
		sold.Phone, sold.Email, sold.PaymentReference,
		sold.PaymentTermsDays, sold.FTax,
		totals.NetMinor, totals.VATMinor, totals.GrossMinor,
	}, electronicArgs(sold, b.buyer, ocr)...)
	var statement, schemeVAT any
	for _, l := range lines {
		if l.MarginScheme {
			statement, schemeVAT = MarginSchemeStatement, of.marginSchemeVAT
		}
	}
	args = append(args, of.source, of.vehicleSaleID, of.rentalID, b.customerID, statement, schemeVAT)
	inv := Invoice{Series: DefaultSeries, Number: number, Source: of.source, CustomerName: b.name,
		NetMinor: totals.NetMinor, VATMinor: totals.VATMinor, GrossMinor: totals.GrossMinor}
	if err := tx.QueryRow(ctx, insert, args...).Scan(&inv.ID, &inv.IssuedAt); err != nil {
		return Invoice{}, fmt.Errorf("write invoice: %w", err)
	}
	if err := copyLines(ctx, tx, scope.ShopID, inv.ID, lines, 1); err != nil {
		return Invoice{}, err
	}
	return inv, nil
}
