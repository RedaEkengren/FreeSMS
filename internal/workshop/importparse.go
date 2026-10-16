package workshop

import (
	"bytes"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"net/mail"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Reading a file somebody exported from their old system.
//
// Nothing here touches the database. A file is bytes of unknown origin: it may
// be UTF-8 or the Windows encoding an older Swedish package wrote, separated by
// semicolons because the decimal separator is a comma, with a thousands
// separator that is a space, and it may contain a cell that a spreadsheet would
// run as a formula. Each of those is decided here, once, and a cell that cannot
// be understood is a problem on its row, never a guess.

// Limits on what is read. An import is a one-off by a trusted person, but the
// reader is a parser of uploaded bytes and is bounded like one.
const (
	maxImportBytes = 5 << 20
	maxImportRows  = 20000
	maxCellLength  = 500
)

// ImportKinds are the things that can be imported, in the order they make
// sense: a vehicle names its owner, a stock row names its part.
var ImportKinds = []string{"customers", "vehicles", "parts", "stock"}

// importColumns are the columns of each kind, in the template's order, and
// which of them a row cannot be without.
var importColumns = map[string][]string{
	"customers": {"ref", "type", "name", "contact", "org_number", "vat_number", "phone", "email", "address", "address2", "postal_code", "city"},
	"vehicles":  {"registration", "vin", "make", "model", "year", "engine", "owner_ref", "odometer_km", "odometer_date"},
	"parts":     {"number", "name", "unit", "cost", "price", "minimum", "location"},
	"stock":     {"part_number", "quantity", "as_of", "unit_cost"},
}

var importRequired = map[string][]string{
	"customers": {"ref", "name"},
	"vehicles":  {"registration"},
	"parts":     {"number", "name"},
	"stock":     {"part_number", "quantity", "as_of"},
}

// importAliases map the headers other systems use to ours. They are matched
// after lower-casing and trimming; anything else is an unknown column and is
// reported, not ignored, so a misspelt header does not silently drop a column.
var importAliases = map[string]string{
	"kundnr": "ref", "kundnummer": "ref", "id": "ref", "kund-id": "ref", "customer_id": "ref", "customer": "ref",
	"typ": "type", "kundtyp": "type",
	"namn": "name", "kundnamn": "name", "företag": "name", "foretag": "name",
	"kontakt": "contact", "kontaktperson": "contact",
	"orgnr": "org_number", "organisationsnummer": "org_number", "org.nr": "org_number", "org": "org_number",
	"momsnr": "vat_number", "vat": "vat_number", "momsregistreringsnummer": "vat_number",
	"telefon": "phone", "tel": "phone", "mobil": "phone",
	"epost": "email", "e-post": "email", "mail": "email",
	"adress": "address", "gatuadress": "address", "adress2": "address2", "adress 2": "address2",
	"postnr": "postal_code", "postnummer": "postal_code", "zip": "postal_code",
	"ort": "city", "stad": "city", "postort": "city",

	"regnr": "registration", "registreringsnummer": "registration", "reg.nr": "registration", "reg": "registration", "plate": "registration",
	"chassinummer": "vin", "vin-nummer": "vin", "chassi": "vin",
	"märke": "make", "marke": "make", "fabrikat": "make", "brand": "make",
	"modell":    "model",
	"årsmodell": "year", "arsmodell": "year", "år": "year", "model_year": "year",
	"motor": "engine",
	"ägare": "owner_ref", "agare": "owner_ref", "ägarens kundnr": "owner_ref", "owner": "owner_ref",
	"mätarställning": "odometer_km", "matarstallning": "odometer_km", "km": "odometer_km", "odometer": "odometer_km",
	"mätardatum": "odometer_date",

	"artikelnummer": "number", "artnr": "number", "art.nr": "number",
	"benämning": "name", "benamning": "name", "beskrivning": "name",
	"enhet":  "unit",
	"inpris": "cost", "inköpspris": "cost", "kostnad": "cost",
	"utpris": "price", "försäljningspris": "price", "pris": "price",
	"minsaldo": "minimum", "min": "minimum", "minimum_quantity": "minimum",
	"lagerplats": "location", "hylla": "location", "plats": "location",

	"artikel": "part_number", "artikelnr": "part_number",
	"antal": "quantity", "saldo": "quantity", "mängd": "quantity",
	"datum": "as_of", "per": "as_of", "räknat": "as_of",
	"inpris_per_enhet": "unit_cost",
}

// importUnits maps what a Swedish system calls a unit to ours.
var importUnits = map[string]string{
	"each": "each", "st": "each", "styck": "each", "pcs": "each", "": "each",
	"litre": "litre", "liter": "litre", "l": "litre",
	"metre": "metre", "meter": "metre", "m": "metre",
	"kilogram": "kilogram", "kg": "kilogram",
	"hour": "hour", "tim": "hour", "timme": "hour", "h": "hour",
}

// ParsedRow is one row of the file as understood.
type ParsedRow struct {
	No       int
	Ref      string
	Values   map[string]string
	Errors   []string
	Warnings []string
}

// ParsedFile is a file as understood.
type ParsedFile struct {
	Kind     string
	Encoding string
	Rows     []ParsedRow
	// Columns of the file that are not ours, so they can be said.
	Unknown []string
}

// windows1252 are the characters 0x80-0x9F of Windows-1252; the rest of it is
// Latin-1. A file from an older package is this and not UTF-8.
var windows1252 = [32]rune{
	'€', 0xFFFD, '‚', 'ƒ', '„', '…', '†', '‡', 'ˆ', '‰', 'Š', '‹', 'Œ', 0xFFFD, 'Ž', 0xFFFD,
	0xFFFD, '‘', '’', '“', '”', '•', '–', '—', '˜', '™', 'š', '›', 'œ', 0xFFFD, 'ž', 'Ÿ',
}

func decodeImport(raw []byte) (string, string, error) {
	switch {
	case len(raw) == 0:
		return "", "", errors.New("the file is empty")
	case len(raw) > maxImportBytes:
		return "", "", fmt.Errorf("the file is larger than %d MB", maxImportBytes>>20)
	case bytes.IndexByte(raw, 0) >= 0:
		return "", "", errors.New("that is not a text file")
	}
	raw = bytes.TrimPrefix(raw, []byte{0xEF, 0xBB, 0xBF})
	if utf8.Valid(raw) {
		return string(raw), "UTF-8", nil
	}
	var b strings.Builder
	for _, c := range raw {
		switch {
		case c >= 0x80 && c <= 0x9F:
			b.WriteRune(windows1252[c-0x80])
		default:
			b.WriteRune(rune(c))
		}
	}
	return b.String(), "Windows-1252", nil
}

func detectDelimiter(text string) rune {
	line := text
	if i := strings.IndexAny(text, "\r\n"); i >= 0 {
		line = text[:i]
	}
	best, bestN := ',', strings.Count(line, ",")
	for _, d := range []rune{';', '\t'} {
		if n := strings.Count(line, string(d)); n > bestN {
			best, bestN = d, n
		}
	}
	return best
}

// ParseImport reads a file of one kind.
func ParseImport(kind string, raw []byte) (ParsedFile, error) {
	cols, ok := importColumns[kind]
	if !ok {
		return ParsedFile{}, fmt.Errorf("%w: %q is not something that can be imported", ErrInvalid, kind)
	}
	text, enc, err := decodeImport(raw)
	if err != nil {
		return ParsedFile{}, fmt.Errorf("%w: %s", ErrInvalid, err)
	}
	r := csv.NewReader(strings.NewReader(text))
	r.Comma = detectDelimiter(text)
	r.FieldsPerRecord = -1
	r.LazyQuotes = true
	r.TrimLeadingSpace = true
	out := ParsedFile{Kind: kind, Encoding: enc}

	header, err := r.Read()
	if err != nil {
		return ParsedFile{}, fmt.Errorf("%w: the first line should be the column names", ErrInvalid)
	}
	index := map[string]int{}
	for i, h := range header {
		h = strings.ToLower(strings.TrimSpace(h))
		if h == "" {
			continue
		}
		name := h
		if canonical, ok := importAliases[h]; ok {
			name = canonical
		}
		known := false
		for _, c := range cols {
			known = known || c == name
		}
		if !known {
			out.Unknown = append(out.Unknown, h)
			continue
		}
		if _, dup := index[name]; dup {
			return ParsedFile{}, fmt.Errorf("%w: the column %q is there twice", ErrInvalid, name)
		}
		index[name] = i
	}
	for _, need := range importRequired[kind] {
		if _, ok := index[need]; !ok {
			return ParsedFile{}, fmt.Errorf("%w: the file has no %q column", ErrInvalid, need)
		}
	}

	line := 1
	for {
		rec, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		line++
		if err != nil {
			// A line that cannot be read is a problem on that line, and the
			// rest of the file is still read.
			out.Rows = append(out.Rows, ParsedRow{No: line, Errors: []string{"this line cannot be read as a row"}})
			continue
		}
		blank := true
		for _, c := range rec {
			if strings.TrimSpace(c) != "" {
				blank = false
			}
		}
		if blank {
			continue
		}
		if len(out.Rows) >= maxImportRows {
			return ParsedFile{}, fmt.Errorf("%w: more than %d rows; split the file", ErrInvalid, maxImportRows)
		}
		row := ParsedRow{No: line, Values: map[string]string{}}
		get := func(name string) string {
			i, ok := index[name]
			if !ok || i >= len(rec) {
				return ""
			}
			return strings.TrimSpace(rec[i])
		}
		for _, c := range cols {
			v := get(c)
			if v != "" {
				row.Values[c] = v
			}
		}
		validateImportRow(kind, &row)
		out.Rows = append(out.Rows, row)
	}
	if len(out.Rows) == 0 {
		return ParsedFile{}, fmt.Errorf("%w: the file has a header and no rows", ErrInvalid)
	}
	return out, nil
}

// looksLikeFormula is a cell a spreadsheet would run. Written into a shop's
// own data it is harmless; exported again and opened in a spreadsheet it is
// not, so it is refused where it comes in.
func looksLikeFormula(s string) bool {
	if s == "" {
		return false
	}
	switch s[0] {
	case '=', '@', '+', '\t', '\r':
		return true
	case '-':
		// A minus sign is a number's, unless it is followed by a letter.
		return len(s) > 1 && !(s[1] >= '0' && s[1] <= '9' || s[1] == '.' || s[1] == ',' || s[1] == ' ')
	}
	return false
}

// parseScaledDecimal reads a number written the Swedish way or the English
// way into an integer of thousandths, hundredths and so on, refusing digits
// beyond the scale instead of rounding them away.
func parseScaledDecimal(s string, scale int) (int64, error) {
	s = strings.NewReplacer(" ", "", " ", "", " ", "", "'", "").Replace(strings.TrimSpace(s))
	if s == "" {
		return 0, errors.New("empty")
	}
	neg := false
	if s[0] == '-' {
		neg, s = true, s[1:]
	}
	comma, dot := strings.LastIndex(s, ","), strings.LastIndex(s, ".")
	switch {
	case comma >= 0 && dot >= 0:
		// Both: the later is the decimal separator, the other groups digits.
		if comma > dot {
			s = strings.ReplaceAll(s, ".", "")
			s = strings.Replace(s, ",", ".", 1)
		} else {
			s = strings.ReplaceAll(s, ",", "")
		}
	case comma >= 0:
		if strings.Count(s, ",") > 1 {
			return 0, errors.New("more than one decimal comma")
		}
		s = strings.Replace(s, ",", ".", 1)
	case dot >= 0 && strings.Count(s, ".") > 1:
		return 0, errors.New("more than one decimal point")
	}
	whole, frac, _ := strings.Cut(s, ".")
	if whole == "" {
		whole = "0"
	}
	if len(frac) > scale {
		return 0, fmt.Errorf("more than %d decimals", scale)
	}
	for len(frac) < scale {
		frac += "0"
	}
	for _, part := range []string{whole, frac} {
		for _, c := range part {
			if c < '0' || c > '9' {
				return 0, errors.New("not a number")
			}
		}
	}
	n, err := strconv.ParseInt(whole+frac, 10, 64)
	if err != nil {
		return 0, errors.New("not a number")
	}
	if n > 1<<50 {
		return 0, errors.New("too large")
	}
	if neg {
		n = -n
	}
	return n, nil
}

func parseImportDate(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	for _, layout := range []string{"2006-01-02", "20060102"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, errors.New("write the date as 2026-01-31")
}

func validateImportRow(kind string, row *ParsedRow) {
	bad := func(format string, a ...any) { row.Errors = append(row.Errors, fmt.Sprintf(format, a...)) }
	warn := func(format string, a ...any) { row.Warnings = append(row.Warnings, fmt.Sprintf(format, a...)) }
	v := row.Values

	for col, cell := range v {
		switch {
		case utf8.RuneCountInString(cell) > maxCellLength:
			bad("%s is longer than %d characters", col, maxCellLength)
		case looksLikeFormula(cell) && !numericColumn(kind, col):
			bad("%s looks like a formula (it begins with %q); remove the first character", col, cell[:1])
		}
	}
	for _, need := range importRequired[kind] {
		if v[need] == "" {
			bad("%s is missing", need)
		}
	}

	number := func(col string, scale int, allowZero bool) {
		cell, ok := v[col]
		if !ok {
			return
		}
		n, err := parseScaledDecimal(cell, scale)
		switch {
		case err != nil:
			bad("%s: %q is not a number (%s)", col, cell, err)
		case n < 0:
			bad("%s cannot be negative", col)
		case n == 0 && !allowZero:
			bad("%s cannot be nothing", col)
		default:
			v[col] = strconv.FormatInt(n, 10)
		}
	}

	switch kind {
	case "customers":
		row.Ref = v["ref"]
		if len(row.Ref) > 100 {
			bad("ref is longer than 100 characters")
		}
		switch strings.ToLower(v["type"]) {
		case "", "private", "privat", "person", "p":
			v["type"] = "private"
		case "company", "företag", "foretag", "f", "c", "bolag":
			v["type"] = "company"
		default:
			bad("type: %q is neither private nor company", v["type"])
		}
		if e := v["email"]; e != "" {
			addr, err := mail.ParseAddress(e)
			if err != nil || addr.Address != e || strings.ContainsAny(e, " ,;") {
				bad("email: %q is not an email address", e)
			} else {
				v["email"] = strings.ToLower(e)
			}
		}
		if len(v["phone"]) > 40 {
			bad("phone is longer than 40 characters")
		}
		if v["type"] == "private" && v["org_number"] != "" {
			warn("a private customer with a company number")
		}
	case "vehicles":
		if reg := v["registration"]; reg != "" {
			n := NormaliseRegistration(reg)
			switch {
			case n == "":
				bad("registration: %q has no letters or digits", reg)
			case len(n) > 12:
				bad("registration: %q is too long", reg)
			}
			row.Ref = n
		}
		if vin := v["vin"]; vin != "" {
			vin = strings.ToUpper(strings.ReplaceAll(vin, " ", ""))
			if len(vin) != 17 || strings.ContainsAny(vin, "IOQ") {
				bad("vin: %q is not a 17-character chassis number", v["vin"])
			}
			v["vin"] = vin
		}
		if y := v["year"]; y != "" {
			n, err := strconv.Atoi(y)
			if err != nil || n < 1900 || n > time.Now().Year()+1 {
				bad("year: %q is not a model year", y)
			}
		}
		number("odometer_km", 0, true)
		if d := v["odometer_date"]; d != "" {
			t, err := parseImportDate(d)
			switch {
			case err != nil:
				bad("odometer_date: %s", err)
			case t.After(time.Now()):
				bad("odometer_date is in the future")
			default:
				v["odometer_date"] = t.Format("2006-01-02")
			}
		}
		if v["odometer_date"] != "" && v["odometer_km"] == "" {
			warn("an odometer date without a reading is ignored")
		}
		if v["owner_ref"] != "" && len(v["owner_ref"]) > 100 {
			bad("owner_ref is longer than 100 characters")
		}
	case "parts":
		row.Ref = v["number"]
		if len(v["number"]) > 60 {
			bad("number is longer than 60 characters")
		}
		if len(v["name"]) > 200 {
			bad("name is longer than 200 characters")
		}
		unit, ok := importUnits[strings.ToLower(v["unit"])]
		if !ok {
			bad("unit: %q is not a unit (each, litre, metre, kilogram, hour)", v["unit"])
		}
		v["unit"] = unit
		number("cost", 2, true)
		number("price", 2, true)
		number("minimum", 3, true)
	case "stock":
		row.Ref = v["part_number"]
		number("quantity", 3, false)
		number("unit_cost", 2, true)
		if d := v["as_of"]; d != "" {
			t, err := parseImportDate(d)
			switch {
			case err != nil:
				bad("as_of: %s", err)
			case t.After(time.Now().AddDate(0, 0, 1)):
				bad("as_of is in the future; opening stock is counted on a day that has been")
			default:
				v["as_of"] = t.Format("2006-01-02")
			}
		}
	}
}

func numericColumn(kind, col string) bool {
	switch kind {
	case "vehicles":
		return col == "odometer_km"
	case "parts":
		return col == "cost" || col == "price" || col == "minimum"
	case "stock":
		return col == "quantity" || col == "unit_cost"
	}
	return false
}

// ImportTemplate is a file to start from: the columns, and a row that shows
// how they are written.
func ImportTemplate(kind string) (string, bool) {
	switch kind {
	case "customers":
		return "ref;type;name;contact;org_number;vat_number;phone;email;address;address2;postal_code;city\n" +
			"K-1001;private;Anna Svensson;;;;070-123 45 67;anna@example.se;Storgatan 1;;411 38;Göteborg\n" +
			"K-1002;company;Åkeri Nilsson AB;Per Nilsson;556677-8899;SE556677889901;031-12 34 56;info@akeri.example;Hamnvägen 4;;418 34;Göteborg\n", true
	case "vehicles":
		return "registration;vin;make;model;year;engine;owner_ref;odometer_km;odometer_date\n" +
			"ABC 123;;Volvo;V70;2012;D5;K-1001;184500;2026-01-15\n" +
			"DEF456;YV1BW59P0B1234567;Volvo;XC60;2015;;K-1002;;\n", true
	case "parts":
		return "number;name;unit;cost;price;minimum;location\n" +
			"BP-100;Bromsklossar fram;each;450,00;650,00;2;A3\n" +
			"OIL-5W30;Motorolja 5W-30;litre;89,50;129,00;20;Oljerum\n", true
	case "stock":
		return "part_number;quantity;as_of;unit_cost\n" +
			"BP-100;6;2026-01-31;450,00\n" +
			"OIL-5W30;48,5;2026-01-31;89,50\n", true
	}
	return "", false
}
