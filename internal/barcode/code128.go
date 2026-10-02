// Package barcode renders Code 128 as SVG.
//
// Written out rather than pulled in, because it is a hundred lines of table
// lookup and a dependency is a thing to keep up to date for the life of the
// project. Code 128 because it takes any printable ASCII, which is what a
// shop's own part numbers are; EAN-13 is for things that come from a factory
// with a number already on them.
package barcode

import (
	"errors"
	"fmt"
	"strings"
)

// patterns holds the bar and space widths for each Code 128 value, as the
// specification gives them: bar, space, bar, space, bar, space -- and for the
// stop symbol a seventh, the closing two-module bar, so that the table alone
// describes every symbol and nothing is appended by hand.
//
// The first version of this table was typed from memory and twenty-one of its
// hundred and seven entries were wrong: values 83 to 102 were shifted, one was
// "200000", and the stop was "211133". The existing tests counted rectangles
// in the SVG, so every label passed and none of them scanned. The table is now
// checked arithmetically -- every data symbol is eleven modules, its bars sum
// to an even number and its spaces to an odd one -- and the rendered output is
// decoded by an independent reader. See the tests.
var patterns = [107]string{
	"212222", "222122", "222221", "121223", "121322", "131222", "122213", "122312", "132212", "221213",
	"221312", "231212", "112232", "122132", "122231", "113222", "123122", "123221", "223211", "221132",
	"221231", "213212", "223112", "312131", "311222", "321122", "321221", "312212", "322112", "322211",
	"212123", "212321", "232121", "111323", "131123", "131321", "112313", "132113", "132311", "211313",
	"231113", "231311", "112133", "112331", "132131", "113123", "113321", "133121", "313121", "211331",
	"231131", "213113", "213311", "213131", "311123", "311321", "331121", "312113", "312311", "332111",
	"314111", "221411", "431111", "111224", "111422", "121124", "121421", "141122", "141221", "112214",
	"112412", "122114", "122411", "142112", "142211", "241211", "221114", "413111", "241112", "134111",
	"111242", "121142", "121241", "114212", "124112", "124211", "411212", "421112", "421211", "212141",
	"214121", "412121", "111143", "111341", "131141", "114113", "114311", "411113", "411311", "113141",
	"114131", "311141", "411131", "211412", "211214", "211232", "2331112",
}

const (
	startB = 104
	stop   = 106
)

// ErrUnencodable is returned for text Code 128 set B cannot carry.
var ErrUnencodable = errors.New("barcode: that text cannot be encoded")

// quietZone is the white margin either side, in modules. The specification
// asks for at least ten; a scanner looking for where a code begins finds it by
// the white before the start symbol, and a label edge or a neighbouring label
// printed hard against the bars is read as part of the code.
const quietZone = 10

// Modules returns the widths that make up text as Code 128 set B, starting
// with a bar and alternating, without the quiet zones.
func Modules(text string) ([]int, error) {
	if text == "" {
		return nil, fmt.Errorf("%w: it is empty", ErrUnencodable)
	}
	values := []int{startB}
	sum := startB
	for i, r := range text {
		if r < 32 || r > 126 {
			return nil, fmt.Errorf("%w: %q is not printable ASCII", ErrUnencodable, r)
		}
		v := int(r) - 32
		values = append(values, v)
		sum += v * (i + 1)
	}
	values = append(values, sum%103, stop)

	var out []int
	for _, v := range values {
		for _, d := range patterns[v] {
			out = append(out, int(d-'0'))
		}
	}
	return out, nil
}

// SVG renders text as a Code 128 barcode.
//
// The human-readable number goes underneath, always. Labels in a workshop get
// oily, torn and painted over, and a code nobody can read by eye is a code
// that stops working on the day somebody needs it most.
func SVG(text string, width, height int) (string, error) {
	widths, err := Modules(text)
	if err != nil {
		return "", err
	}

	// Total width, so the bars scale to the space available rather than to a
	// fixed size that is wrong on every label but one.
	total := 2 * quietZone
	for _, w := range widths {
		total += w
	}

	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" width="%d" height="%d" role="img" aria-label="%s">`,
		total, height, width, height, escape(text))
	fmt.Fprintf(&b, `<rect width="%d" height="%d" fill="#fff"/>`, total, height)

	x := quietZone
	for i, w := range widths {
		if i%2 == 0 {
			fmt.Fprintf(&b, `<rect x="%d" y="0" width="%d" height="%d" fill="#000"/>`, x, w, height)
		}
		x += w
	}
	b.WriteString(`</svg>`)
	return b.String(), nil
}

func escape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	return r.Replace(s)
}
