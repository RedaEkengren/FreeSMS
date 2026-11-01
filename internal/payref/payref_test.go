package payref

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// Bankgirot's own worked examples: the bankgiro numbers 991-2346 and
// 5555-5551 in its modulus 10 description, and the OCR reference 12345682
// in its user manual -- reference 123456, length digit 8, check digit 2.
func TestTheCheckDigitIsBankgirotsModulus10(t *testing.T) {
	for digits, want := range map[string]byte{"991234": '6', "5555555": '1', "1234568": '2', "0": '0', "1": '8'} {
		if got := CheckDigit(digits); got != want {
			t.Errorf("CheckDigit(%q) = %c, want %c", digits, got, want)
		}
	}
	if got, err := OCR(123456); err != nil || got != "12345682" {
		t.Errorf("OCR(123456) = %q, %v; want Bankgirot's 12345682", got, err)
	}
	for _, n := range []int64{1, 9, 10, 99, 1234, 99999999} {
		ref, err := OCR(n)
		if err != nil {
			t.Fatal(err)
		}
		if want := byte('0' + len(ref)%10); ref[len(ref)-2] != want {
			t.Errorf("OCR(%d) = %s: the length digit is not its length", n, ref)
		}
		if back, err := CheckOCR(ref); err != nil || back != ref {
			t.Errorf("CheckOCR(OCR(%d)) = %q, %v", n, back, err)
		}
	}
	if _, err := OCR(0); err == nil {
		t.Error("OCR(0) made a reference")
	}
}

// A reference typed with spaces or a hyphen is read; one digit wrong, two
// swapped, or something that is not digits is refused, each for its reason.
func TestATypedReferenceIsCheckedBeforeAnythingIsLookedUp(t *testing.T) {
	if got, err := CheckOCR(" 1234 5682 "); err != nil || got != "12345682" {
		t.Errorf("spaces: %q, %v", got, err)
	}
	for _, typed := range []string{"12345683", "12345628", "21345682"} {
		if _, err := CheckOCR(typed); !errors.Is(err, ErrCheckDigit) {
			t.Errorf("%q: %v, want the check digit refused", typed, err)
		}
	}
	for _, typed := range []string{"", "7", "1234A682", strings.Repeat("1", 26)} {
		if _, err := CheckOCR(typed); !errors.Is(err, ErrNotAReference) {
			t.Errorf("%q: %v, want not a reference", typed, err)
		}
	}
}

// The Swish code pays exactly what is owed to the shop's number, nothing
// changeable, with a message Swish will take: at most 50 characters, none
// it refuses, and no semicolon to break the code apart.
func TestTheSwishCodeIsLockedAndItsMessageFits(t *testing.T) {
	got, err := Swish("123 456 78 90", 123450, "Faktura 12345682")
	if err != nil || got != "C1234567890;1234,50;Faktura 12345682;0" {
		t.Errorf("Swish = %q, %v", got, err)
	}
	if got, _ := Swish("1234567890", 50000, "x"); got != "C1234567890;500;x;0" {
		t.Errorf("whole kronor: %q", got)
	}
	long, _ := Swish("1234567890", 100, strings.Repeat("ab;<>&", 30)+"Åäö")
	msg := strings.Split(long, ";")
	if len(msg) != 4 || len([]rune(msg[2])) > SwishMessageMax || strings.ContainsAny(msg[2], "<>&") {
		t.Errorf("a long message with characters Swish refuses: %q", long)
	}
	for _, bad := range []struct {
		number string
		amount int64
	}{{"070-123", 100}, {"12345678901", 100}, {"1234567890", 0}, {"1234567890", -5}} {
		if _, err := Swish(bad.number, bad.amount, ""); err == nil {
			t.Errorf("Swish(%q, %d) made a code", bad.number, bad.amount)
		}
	}
}

// The bankgiro code is UsingQR's JSON, keys in the specification's order,
// with its amount written as the specification says, and refused without
// what a payment must have.
func TestTheBankgiroCodeIsUsingQRs(t *testing.T) {
	issued := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	giro := GiroOf("bankgiro", "Bankgiro 5555-5551")
	if giro != (Giro{Type: "BG", Account: "5555-5551"}) {
		t.Fatalf("GiroOf: %+v", giro)
	}
	got, err := UsingQR("Verkstaden i Åby AB", "556677-8899", "12345682", issued, issued.AddDate(0, 0, 30), 123450, giro)
	want := `{"uqr":1,"tp":1,"nme":"Verkstaden i Åby AB","cid":"556677-8899","iref":"12345682","idt":"20261101","ddt":"20261201","due":1234.5,"pt":"BG","acc":"5555-5551"}`
	if err != nil || got != want {
		t.Errorf("UsingQR =\n%s, %v\nwant\n%s", got, err, want)
	}
	if !json.Valid([]byte(got)) {
		t.Error("not JSON")
	}
	if got, _ := UsingQR("A", "1", "18", issued, issued, 50000, giro); !strings.Contains(got, `"due":500,`) {
		t.Errorf("500.00 is written 500: %s", got)
	}
	if g := GiroOf("iban", "SE45 5000 0000 0583 9825 7466"); g != (Giro{}) {
		t.Errorf("an IBAN read as a giro: %+v", g)
	}
	if g := GiroOf("plusgiro", "PG 176099-0"); g != (Giro{Type: "PG", Account: "176099-0"}) {
		t.Errorf("a plusgiro: %+v", g)
	}
	// A space groups a number's digits but does not join it to the next
	// number: what follows a whole giro is something else.
	for printed, want := range map[string]string{
		"Bankgiro 5050-1055 08 123 45":      "5050-1055",
		"Bankgiro 5050-1055, tel 08-123 45": "5050-1055",
		"PG 17 60 99-0":                     "176099-0",
		"BG 5050-1055-08":                   "5050-1055",
		"1234567890 08 123 45":              "1234567890",
	} {
		if g := GiroOf("bankgiro", printed); g.Account != want {
			t.Errorf("%q read as %q, want %q", printed, g.Account, want)
		}
	}
	if _, err := UsingQR("A", "", "18", issued, issued, 100, giro); err == nil {
		t.Error("made a code with no company number")
	}
}
