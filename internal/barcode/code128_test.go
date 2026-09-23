package barcode

import (
	"errors"
	"strings"
	"testing"
)

func TestSVGRendersSomethingAScannerCouldRead(t *testing.T) {
	svg, err := SVG("BP-100", 300, 80)
	if err != nil {
		t.Fatalf("SVG: %v", err)
	}
	if !strings.HasPrefix(svg, "<svg") || !strings.HasSuffix(svg, "</svg>") {
		t.Error("that is not an SVG document")
	}
	if strings.Count(svg, "<rect") < 10 {
		t.Error("too few bars to be a barcode")
	}
	// The label is what a screen reader and a person get.
	if !strings.Contains(svg, `aria-label="BP-100"`) {
		t.Error("the code is not in the accessible label")
	}
}

// A workshop's part numbers are printable ASCII; anything else is a mistake
// worth catching at the label rather than at the scanner.
func TestSVGRefusesWhatItCannotEncode(t *testing.T) {
	for _, in := range []string{"", "Öberg", "tab\there"} {
		if _, err := SVG(in, 300, 80); !errors.Is(err, ErrUnencodable) {
			t.Errorf("SVG(%q) = %v, want ErrUnencodable", in, err)
		}
	}
}

// The check character depends on position, so two codes with the same
// characters in a different order must not render the same.
func TestTheCheckCharacterDependsOnOrder(t *testing.T) {
	a, _ := SVG("AB", 100, 40)
	b, _ := SVG("BA", 100, 40)
	if a == b {
		t.Error("two different codes rendered identically")
	}
}

func TestEscapingInTheLabel(t *testing.T) {
	svg, err := SVG(`A&B<C`, 100, 40)
	if err != nil {
		t.Fatalf("SVG: %v", err)
	}
	if strings.Contains(svg, `aria-label="A&B<C"`) {
		t.Error("the label was not escaped")
	}
	if !strings.Contains(svg, "A&amp;B&lt;C") {
		t.Error("the escaped label is missing")
	}
}
