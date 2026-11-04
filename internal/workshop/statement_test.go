package workshop_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// A bank's statement reads with semicolons or commas, a byte order mark,
// a header, öre after a comma, and money going out negative; a line that
// does not read is refused by its number (#152).
func TestABanksStatementReadsAsItIsExported(t *testing.T) {
	for name, file := range map[string]string{
		"semicolons": "\ufeffDatum;Belopp;Text;Id\n2026-10-30;1 250,00;OCR 4711;B-1\n2026-10-31;-300,50;Bankavgift;B-2\n",
		"commas":     "2026-10-30,1250.00,OCR 4711,B-1\n2026-10-31,-300.5,Bankavgift,B-2\n",
	} {
		lines, err := workshop.ParseStatementCSV(strings.NewReader(file))
		switch {
		case err != nil:
			t.Errorf("%s: %v", name, err)
		case len(lines) != 2 || lines[0].AmountMinor != 125_000 || lines[1].AmountMinor != -30_050 || lines[0].ExternalID != "B-1" || lines[1].Reference != "Bankavgift":
			t.Errorf("%s read as %+v", name, lines)
		}
	}
	if _, err := workshop.ParseStatementCSV(strings.NewReader("2026-10-30;1250;OCR\n30/10;12;x\n")); !errors.Is(err, workshop.ErrInvalid) || !strings.Contains(err.Error(), "line 2") {
		t.Errorf("a day that does not read: %v", err)
	}
}
