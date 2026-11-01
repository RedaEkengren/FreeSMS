package barcode

import (
	"image"
	"image/color"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The block table is the geometry's: every version's codewords, data and
// error correction together, are exactly the modules it has to put them in.
func TestTheQRBlockTableFillsEveryVersion(t *testing.T) {
	for v := 1; v <= 20; v++ {
		b := qrBlocks[v]
		total := (b[1]+b[3])*b[0] + qrDataCodewords(v)
		if total != qrRawModules(v)/8 {
			t.Errorf("version %d: %d codewords in the table, the symbol holds %d", v, total, qrRawModules(v)/8)
		}
		if b[3] > 0 && b[4] != b[2]+1 {
			t.Errorf("version %d: the second group's blocks are not one longer", v)
		}
	}
}

// The error correction is Reed-Solomon over GF(256): every block of data
// and its remainder divide by the generator with nothing left, which is
// what a reader checks.
func TestTheQRErrorCorrectionLeavesNoRemainder(t *testing.T) {
	data := qrStream(5, 8, []byte("Bankgiro 5555-5551, OCR 12345682"), 26)
	b := qrBlocks[5]
	div := qrDivisor(b[0])
	block := data[:b[2]]
	whole := append(append([]byte(nil), block...), qrRemainder(block, div)...)
	if r := qrRemainder(whole, div); strings.Trim(string(r), "\x00") != "" {
		t.Errorf("a block and its error correction leave %v", r)
	}
	if _, err := EncodeQR(make([]byte, 700), 0); err != ErrQRTooLong {
		t.Errorf("700 bytes: %v, want too long", err)
	}
}

func rasterQR(t *testing.T, q *QR) string {
	t.Helper()
	const px, quiet = 4, 4
	side := (q.Size + 2*quiet) * px
	img := image.NewGray(image.Rect(0, 0, side, side))
	for i := range img.Pix {
		img.Pix[i] = 0xff
	}
	for y := 0; y < q.Size; y++ {
		for x := 0; x < q.Size; x++ {
			if !q.Dark(x, y) {
				continue
			}
			for dy := 0; dy < px; dy++ {
				for dx := 0; dx < px; dx++ {
					img.SetGray((x+quiet)*px+dx, (y+quiet)*px+dy, color.Gray{Y: 0})
				}
			}
		}
	}
	path := filepath.Join(t.TempDir(), "qr.png")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		t.Fatal(err)
	}
	return path
}

// A QR code decodes back to what was put in it, by a reader that shares no
// code with the encoder: short and long, across the versions where the
// count grows to sixteen bits and version information appears, and UTF-8
// marked by its ECI so "å" reads as "å".
func TestQRCodesDecodeWithAnIndependentReader(t *testing.T) {
	cmd := os.Getenv(decoderEnv)
	if cmd == "" {
		if os.Getenv(decoderEnv+"_REQUIRED") != "" {
			t.Fatalf("%s is not set where a decoder is required; the QR code would go unverified", decoderEnv)
		}
		t.Skipf("%s not set; no independent decoder to check against", decoderEnv)
	}
	parts := strings.Fields(cmd)
	for _, text := range []string{
		"A",
		"C1234567890;1234,50;Faktura 12345682;0",
		`{"uqr":1,"tp":1,"nme":"Verkstaden i Åby AB","cid":"556677-8899","iref":"12345682","idt":"20261101","ddt":"20261201","due":1234.5,"pt":"BG","acc":"5555-5551"}`,
		strings.Repeat("0123456789", 20),
		strings.Repeat("Verkstad ", 60),
	} {
		q, err := EncodeQR([]byte(text), 26)
		if err != nil {
			t.Fatalf("%q: %v", text, err)
		}
		out, err := exec.Command(parts[0], append(parts[1:], rasterQR(t, q))...).Output()
		if err != nil {
			t.Errorf("version %d, %q: the decoder could not read it: %v", (q.Size-17)/4, text, err)
			continue
		}
		if got := strings.TrimSuffix(string(out), "\n"); got != text {
			t.Errorf("version %d: put in %q, the decoder read %q", (q.Size-17)/4, text, got)
		}
	}
}

// The SVG draws exactly the dark modules: its rectangles, read back, are
// the code's dark modules and nothing else.
func TestTheQRSVGDrawsExactlyTheDarkModules(t *testing.T) {
	q, err := EncodeQR([]byte("C1234567890;1234,50;12345682;0"), 0)
	if err != nil {
		t.Fatal(err)
	}
	svg := q.SVG("x")
	drawn := map[[2]int]bool{}
	for _, m := range regexp.MustCompile(`M(\d+) (\d+)h(\d+)v1h-(\d+)z`).FindAllStringSubmatch(svg, -1) {
		x, _ := strconv.Atoi(m[1])
		y, _ := strconv.Atoi(m[2])
		n, _ := strconv.Atoi(m[3])
		for i := 0; i < n; i++ {
			drawn[[2]int{x - 4 + i, y - 4}] = true
		}
	}
	for y := 0; y < q.Size; y++ {
		for x := 0; x < q.Size; x++ {
			if drawn[[2]int{x, y}] != q.Dark(x, y) {
				t.Fatalf("module %d,%d: drawn %v, dark %v", x, y, drawn[[2]int{x, y}], q.Dark(x, y))
			}
		}
	}
}
