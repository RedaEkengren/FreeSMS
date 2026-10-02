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

// Arithmetic every Code 128 symbol satisfies, whoever typed the table: a data
// symbol is eleven modules of three bars and three spaces, its bars sum to an
// even number and its spaces to an odd one, and no two are the same. The first
// table broke all of these and nothing checked.
func TestThePatternTableIsCode128(t *testing.T) {
	seen := map[string]int{}
	for i, p := range patterns {
		if j, dup := seen[p]; dup {
			t.Errorf("values %d and %d share the pattern %s", j, i, p)
		}
		seen[p] = i

		w := make([]int, len(p))
		total := 0
		for k, c := range p {
			w[k] = int(c - '0')
			if w[k] < 1 || w[k] > 4 {
				t.Errorf("value %d (%s): a width of %d is not a Code 128 width", i, p, w[k])
			}
			total += w[k]
		}
		if i == stop {
			if len(p) != 7 || total != 13 {
				t.Errorf("the stop symbol %s is not seven elements over thirteen modules", p)
			}
			continue
		}
		if len(p) != 6 || total != 11 {
			t.Errorf("value %d (%s) is %d elements over %d modules, want 6 over 11", i, p, len(p), total)
			continue
		}
		if (w[0]+w[2]+w[4])%2 != 0 {
			t.Errorf("value %d (%s): bar widths sum to an odd number", i, p)
		}
		if (w[1]+w[3]+w[5])%2 != 1 {
			t.Errorf("value %d (%s): space widths sum to an even number", i, p)
		}
	}
}
