package workshop

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/database"
	"github.com/RedaEkengren/FreeSMS/internal/ubl"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// What an electronic invoice cannot be written without, as the sentences the
// page shows. They are the keys of the catalogue, so an English page reads
// them as they are and a Swedish one reads their translation.
const (
	needSellerDetails  = "the workshop's details, which this invoice was issued without"
	needSellerAddress  = "the workshop's postal code and town"
	needSellerVAT      = "the workshop's VAT number"
	needSellerCompany  = "the workshop's company number"
	needSellerEndpoint = "the workshop's electronic invoice address"
	needSellerPayment  = "how the workshop is paid: bankgiro, plusgiro or IBAN, and the number"
	needSellerCheck    = "the workshop's electronic invoice address: a Swedish company number has ten digits and a check digit that must add up"
	needBuyerCountry   = "the customer's country"
	needBuyerAddress   = "the customer's postal code and town"
	needBuyerEndpoint  = "the customer's electronic invoice address"
	needBuyerCheck     = "the customer's electronic invoice address: a Swedish company number has ten digits and a check digit that must add up"
	needBuyerReference = "the customer's reference"
	needLineCategory   = "a VAT category for a line at 0 per cent"
	needLineUnit       = "a unit on every line"
)

// EInvoice writes an issued invoice or credit note as a Peppol BIS Billing 3.0
// document. It returns either the document or what is missing from the
// invoice for it to be written, never half of one: an invoice that is sent
// onwards with a hole in it is refused at the other end, days later, by
// somebody who cannot ask what was meant.
//
// Everything comes from the frozen document, so it is the same today as the
// day the invoice was issued.
func EInvoice(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, invoiceID string) (xml []byte, missing []string, err error) {
	if !scope.Role.SeesCustomerPersonalData() {
		return nil, nil, access.ErrForbidden
	}
	if !looksLikeUUID(invoiceID) {
		return nil, nil, ErrNotFound
	}
	var doc Document
	var zone string
	err = database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		if err := documentTx(ctx, tx, invoiceID, &doc); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT timezone FROM shops WHERE id = $1`, scope.ShopID).Scan(&zone)
	})
	if err != nil {
		return nil, nil, err
	}
	loc, err := time.LoadLocation(zone)
	if err != nil {
		return nil, nil, fmt.Errorf("read timezone: %w", err)
	}

	in, missing := ublInvoice(doc, loc)
	if len(missing) > 0 {
		return nil, missing, nil
	}
	out, err := in.Marshal()
	if err != nil {
		return nil, nil, err
	}
	return out, nil, nil
}

func ublInvoice(d Document, loc *time.Location) (ubl.Invoice, []string) {
	var missing []string
	need := func(ok bool, what string) {
		if !ok {
			missing = append(missing, what)
		}
	}

	s := d.Seller
	need(s.Known(), needSellerDetails)
	need(s.PostalCode != "" && s.City != "" && s.Country != "", needSellerAddress)
	need(s.VATNumber != "", needSellerVAT)
	need(companyNumber(s.OrgNumber) != "", needSellerCompany)
	sScheme, sValue, sOK := endpoint(s.EndpointID)
	need(sOK, needSellerEndpoint)
	need(!sOK || sScheme != swedishOrgScheme || validOrgNumber(sValue), needSellerCheck)
	account := paymentAccount(s.PaymentScheme, s.PaymentReference)
	need(account != "", needSellerPayment)

	bScheme, bValue, bOK := endpoint(d.CustomerEndpointID)
	need(d.CustomerCountry != "", needBuyerCountry)
	need(d.CustomerPostalCode != "" && d.CustomerCity != "", needBuyerAddress)
	need(bOK, needBuyerEndpoint)
	need(!bOK || bScheme != swedishOrgScheme || validOrgNumber(bValue), needBuyerCheck)
	need(d.CustomerReference != "", needBuyerReference)

	in := ubl.Invoice{
		Credit:         d.IsCreditNote(),
		Reference:      d.Reference(),
		CreditOf:       d.CreditOf,
		IssueDate:      d.IssuedAt.In(loc),
		DueDate:        d.DueIn(loc),
		Currency:       d.Currency,
		BuyerReference: d.CustomerReference,
		Seller: ubl.Party{
			Name: s.Name, Street: s.Street, PostalCode: s.PostalCode, City: s.City, Country: s.Country,
			EndpointScheme: sScheme, EndpointValue: sValue,
			VATID: s.VATNumber, LegalID: companyNumber(s.OrgNumber), LegalScheme: "0007",
		},
		Buyer: ubl.Party{
			Name: d.CustomerName, Street: streetOf(d.CustomerAddress, d.CustomerPostalCode, d.CustomerCity),
			PostalCode: d.CustomerPostalCode, City: d.CustomerCity, Country: d.CustomerCountry,
			EndpointScheme: bScheme, EndpointValue: bValue,
			VATID: d.CustomerVATNumber, LegalID: companyNumber(d.CustomerOrgNumber), LegalScheme: "0007",
		},
		Payment: ubl.PaymentMeans{Kind: s.PaymentScheme, Account: account},
		Terms:   fmt.Sprintf("%d days net", s.PaymentTermsDays),
	}

	var zeroLine, noUnit bool
	for _, l := range d.Lines {
		if l.VATCategory == "" {
			zeroLine = true
		}
		if l.UnitCode == "" {
			noUnit = true
		}
		in.Lines = append(in.Lines, ubl.Line{
			Description: l.Description, UnitCode: l.UnitCode, QuantityMilli: l.QuantityMilli,
			UnitMinor: l.UnitPriceMinor, NetMinor: l.NetMinor, VATMinor: l.VATMinor,
			VATCategory: l.VATCategory, RateBasis: l.VATRateBasis,
		})
	}
	need(!zeroLine, needLineCategory)
	need(!noUnit, needLineUnit)
	return in, missing
}

// endpoint splits "0007:5566778899" into the scheme and the value in it.
func endpoint(id string) (scheme, value string, ok bool) {
	scheme, value, found := strings.Cut(strings.TrimSpace(id), ":")
	scheme, value = strings.TrimSpace(scheme), strings.TrimSpace(value)
	if !found || len(scheme) != 4 || value == "" {
		return "", "", false
	}
	return scheme, value, true
}

var notDigit = regexp.MustCompile(`[^0-9]`)

// companyNumber is a Swedish company number as the network writes it: ten
// digits and no hyphen.
func companyNumber(s string) string {
	n := notDigit.ReplaceAllString(s, "")
	if len(n) == 12 && strings.HasPrefix(n, "16") {
		n = n[2:]
	}
	if !validOrgNumber(n) {
		return ""
	}
	return n
}

// swedishOrgScheme is the Peppol identifier scheme for a Swedish company number.
const swedishOrgScheme = "0007"

// validOrgNumber is true for ten digits whose last is the check digit of the
// Luhn sum over the ten (every second digit from the left doubled, the digits
// of each product added). Peppol refuses a Swedish number that fails it
// (PEPPOL-COMMON-R049), so a typo is caught here, with a sentence, rather than
// by the receiver.
func validOrgNumber(n string) bool {
	if len(n) != 10 {
		return false
	}
	sum := 0
	for i, r := range n {
		if r < '0' || r > '9' {
			return false
		}
		d := int(r - '0')
		if i%2 == 0 {
			d *= 2
			if d > 9 {
				d -= 9
			}
		}
		sum += d
	}
	return sum%10 == 0
}

// paymentAccount is the number out of what the shop typed beside it: "Bankgiro
// 123-4567" is 1234567. An IBAN keeps its letters.
func paymentAccount(scheme, text string) string {
	switch scheme {
	case "bankgiro", "plusgiro":
		return notDigit.ReplaceAllString(text, "")
	case "iban":
		t := strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(text), " ", ""))
		return strings.TrimPrefix(t, "IBAN")
	}
	return ""
}

// streetOf is the part of a one-line address before the town: the address is
// frozen as "street, postal code town" and the street is what is left when the
// town is taken off the end.
func streetOf(address, postal, city string) string {
	town := strings.TrimSpace(postal + " " + city)
	return strings.TrimSuffix(strings.TrimSuffix(strings.TrimSpace(address), town), ", ")
}
