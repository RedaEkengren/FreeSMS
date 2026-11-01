// Package payref makes what a customer pays an invoice with (#234): a
// payment reference with a check digit, and the string a QR code carries
// so a banking or Swish app fills in the payment without anybody typing.
//
// Everything here is built from what is frozen on the issued invoice and
// from the amount still owed, so the same invoice with the same payments
// gives the same code. Money is minor units, never a float.
package payref

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// The OCR reference follows Bankgirot's rules for an OCR-referensnummer
// (Bankgiro Inbetalningar, användarmanual, October 2023, sections 5.2.1 to
// 5.2.3, https://www.bankgirot.se/globalassets/dokument/anvandarmanualer/bankgiroinbetalningar_anvandarmanual_sv.pdf):
// 2 to 25 digits, the last a check digit by the modulus 10 method, and,
// where it is used, a length digit second to last giving the whole length
// modulo 10. The check digit is computed over the reference and the length
// digit together. The modulus 10 method is Bankgirot's own description
// (https://www.bankgirot.se/globalassets/dokument/anvandarmanualer/10-modul.pdf):
// from the right, ignoring the check digit, weights 2, 1, 2, ...; a two-digit
// product has its digits added; the check digit takes the sum up to the next
// ten. It is the Luhn algorithm.
//
// FreeSMS writes the invoice number, a length digit and the check digit. A
// length digit costs nothing where the shop's agreement checks only the
// check digit ("mjuk kontroll"), and is required where it checks a variable
// length, which Bankgirot recommends first.

// MaxReference is the longest OCR reference Bankgirot accepts.
const MaxReference = 25

var (
	// ErrNotAReference is something that cannot be an OCR reference at all.
	ErrNotAReference = errors.New("payref: an OCR reference is 2 to 25 digits")
	// ErrCheckDigit is a reference whose check digit is wrong: mistyped.
	ErrCheckDigit = errors.New("payref: the reference's check digit is wrong")
)

// CheckDigit is the modulus 10 check digit of digits, which must be digits.
func CheckDigit(digits string) byte {
	sum := 0
	for i := 0; i < len(digits); i++ {
		d := int(digits[len(digits)-1-i] - '0')
		if i%2 == 0 {
			d *= 2
			if d > 9 {
				d -= 9
			}
		}
		sum += d
	}
	return byte('0' + (10-sum%10)%10)
}

// OCR is an invoice number as an OCR reference: the number, a length digit
// and a check digit.
func OCR(number int64) (string, error) {
	if number <= 0 {
		return "", fmt.Errorf("payref: invoice number %d is not one", number)
	}
	n := strconv.FormatInt(number, 10)
	length := len(n) + 2
	if length > MaxReference {
		return "", ErrNotAReference
	}
	body := n + strconv.Itoa(length%10)
	return body + string(CheckDigit(body)), nil
}

// DefaultSeries is the invoice series whose references are OCR's: the
// number alone, as every reference issued before there could be another.
const DefaultSeries = "A"

// OCRFor is the reference of invoice number in series. The default
// series' is OCR(number). Another series' starts with a 0, which a number
// never does, so it cannot be one of the default series'; then two digits
// for each character of the series -- A to Z are 10 to 35, 0 to 9 are 36
// to 45, none starting with 0 -- and a 0 to end them, then the number, the
// length digit and the check digit. B-57 is 0 11 0 57, then 8 and 2:
// 01105782. So the same number in two series is two references, and an
// invoice is found by its reference whatever its series. A series of
// anything but capital letters and digits has no reference.
func OCRFor(series string, number int64) (string, error) {
	if series == DefaultSeries {
		return OCR(number)
	}
	if number <= 0 {
		return "", fmt.Errorf("payref: invoice number %d is not one", number)
	}
	if series == "" {
		return "", fmt.Errorf("payref: an invoice series has a name")
	}
	body := "0"
	for _, r := range series {
		switch {
		case r >= 'A' && r <= 'Z':
			body += strconv.Itoa(int(r-'A') + 10)
		case r >= '0' && r <= '9':
			body += strconv.Itoa(int(r-'0') + 36)
		default:
			return "", fmt.Errorf("payref: series %q is not capital letters and digits", series)
		}
	}
	body += "0" + strconv.FormatInt(number, 10)
	length := len(body) + 2
	if length > MaxReference {
		return "", ErrNotAReference
	}
	body += strconv.Itoa(length % 10)
	return body + string(CheckDigit(body)), nil
}

// CheckOCR reads a reference as somebody typed or a bank sent it: spaces
// and hyphens are dropped, and what is left must be 2 to 25 digits whose
// last is the right check digit. It returns the reference as stored.
func CheckOCR(typed string) (string, error) {
	s := strings.Map(func(r rune) rune {
		if r == ' ' || r == '-' {
			return -1
		}
		return r
	}, strings.TrimSpace(typed))
	if len(s) < 2 || len(s) > MaxReference {
		return "", ErrNotAReference
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return "", ErrNotAReference
		}
	}
	if CheckDigit(s[:len(s)-1]) != s[len(s)-1] {
		return "", ErrCheckDigit
	}
	return s, nil
}

// Swish's prefilled payment code. Swish documents a prefilled code through
// its QR API (https://developer.swish.nu/api/qr-codes), whose message is at
// most 50 characters; the string the code itself carries is not published
// by Swish, and is as documented by those who have read it
// (https://github.com/lindskogen/swish-qr-format): "C" and the payee, then
// the amount, the message and which of them may be changed, separated by
// semicolons -- 0 for none. The amount is written with a decimal comma, as
// Invelle's codes are. Built here, never through Swish's API: an invoice
// page asks nobody else, least of all with the customer's details.

// SwishMessageMax is the longest message Swish takes.
const SwishMessageMax = 50

// swishMessage keeps what a Swish message may hold -- letters, digits,
// spaces and :.,?!()-" -- and drops the rest, the semicolon the code is
// separated by among them, at most 50 characters.
func swishMessage(m string) string {
	var b strings.Builder
	n := 0
	for _, r := range m {
		ok := r == ' ' || (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			strings.ContainsRune("åäöÅÄÖéÉüÜ:.,?!()-\"", r)
		if !ok || n == SwishMessageMax {
			continue
		}
		b.WriteRune(r)
		n++
	}
	return strings.TrimSpace(b.String())
}

// SwishNumber reads a Swish number as a shop types it: digits only, ten of
// them; "" for something that is not one.
func SwishNumber(typed string) string {
	s := strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		if r == ' ' || r == '-' {
			return -1
		}
		return 'x'
	}, strings.TrimSpace(typed))
	if len(s) != 10 || strings.ContainsRune(s, 'x') {
		return ""
	}
	return s
}

// Swish is the string a Swish code carries to pay amountMinor to number
// with message, nothing of it changeable.
func Swish(number string, amountMinor int64, message string) (string, error) {
	n := SwishNumber(number)
	if n == "" {
		return "", fmt.Errorf("payref: %q is not a Swish number", number)
	}
	if amountMinor <= 0 {
		return "", fmt.Errorf("payref: nothing to pay")
	}
	amount := strconv.FormatInt(amountMinor/100, 10)
	if ore := amountMinor % 100; ore != 0 {
		amount += fmt.Sprintf(",%02d", ore)
	}
	return "C" + n + ";" + amount + ";" + swishMessage(message) + ";0", nil
}

// The bankgiro code is UsingQR's (UsingQR format specification, revision
// 2, https://www.qrkod.info/specification.pdf): a JSON object, UTF-8 with
// its ECI, with the keys a domestic Swedish payment must have -- version,
// type 1, the seller's name and company number, the invoice reference, the
// due date and amount, the payment type and the account -- and the invoice
// date, which it may. Amounts have a decimal point and nothing that does
// not count: 500.00 is 500, 500.10 is 500.1. Dates are YYYYMMDD.

// UsingQRECI is the ECI designator the code must carry: UTF-8.
const UsingQRECI = 26

// Giro is a bankgiro or a plusgiro, the account a UsingQR code pays to.
type Giro struct {
	// "BG" or "PG".
	Type    string
	Account string
}

// GiroOf reads the account out of what a shop printed as its payment
// details, for the scheme it said it is: the digits and the hyphen of the
// first number in it. Zero for any other scheme, or no number.
//
// A space may group a number's digits -- a plusgiro is often written
// "12 34 56-7" -- but does not join two numbers: once the hyphen before
// the check part has been written, or ten digits, the most a giro has,
// a space and more digits are something else, such as the telephone
// number printed after it. Anything other than a digit, a hyphen or a
// space ends the number too.
func GiroOf(scheme, printed string) Giro {
	var t string
	switch scheme {
	case "bankgiro":
		t = "BG"
	case "plusgiro":
		t = "PG"
	default:
		return Giro{}
	}
	var b strings.Builder
	digits, hyphen, spaced := 0, false, false
	for _, r := range printed {
		switch {
		case r >= '0' && r <= '9':
			if spaced && (hyphen || digits >= 10) {
				goto done
			}
			b.WriteRune(r)
			digits++
			spaced = false
		case r == '-' && b.Len() > 0:
			if hyphen {
				goto done
			}
			b.WriteRune(r)
			hyphen, spaced = true, false
		case r == ' ' && b.Len() > 0:
			spaced = true
		case b.Len() > 0:
			goto done
		}
	}
done:
	acc := strings.Trim(b.String(), "-")
	if n := len(strings.ReplaceAll(acc, "-", "")); n < 2 || n > 10 {
		return Giro{}
	}
	return Giro{Type: t, Account: acc}
}

// UsingQR is the bankgiro code's JSON for an invoice.
func UsingQR(seller, orgNumber, reference string, issued, due time.Time, amountMinor int64, giro Giro) (string, error) {
	if giro.Type == "" || strings.TrimSpace(seller) == "" || strings.TrimSpace(orgNumber) == "" || reference == "" || amountMinor <= 0 {
		return "", fmt.Errorf("payref: a bankgiro code needs the seller, its company number, a reference, an account and something owed")
	}
	if !utf8.ValidString(seller) {
		return "", fmt.Errorf("payref: the seller's name is not text")
	}
	amount := strconv.FormatInt(amountMinor/100, 10)
	if ore := amountMinor % 100; ore != 0 {
		amount += strings.TrimRight(fmt.Sprintf(".%02d", ore), "0")
	}
	// In the specification's order; encoding/json would sort a map's keys.
	field := func(k string, v any) string {
		b, _ := json.Marshal(v)
		return fmt.Sprintf("%q:%s", k, b)
	}
	return "{" + strings.Join([]string{
		`"uqr":1`, `"tp":1`,
		field("nme", strings.TrimSpace(seller)),
		field("cid", strings.TrimSpace(orgNumber)),
		field("iref", reference),
		field("idt", issued.Format("20060102")),
		field("ddt", due.Format("20060102")),
		`"due":` + amount,
		field("pt", giro.Type),
		field("acc", giro.Account),
	}, ",") + "}", nil
}
