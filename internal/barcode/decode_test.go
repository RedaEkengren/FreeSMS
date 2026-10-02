package barcode

import (
	"image"
	"image/color"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// decoderEnv names a command that reads a PNG and prints the Code 128 text in
// it -- "zbarimg --raw -q" in CI. Unset locally means skip; unset in CI is a
// failure, set by the workflow, because a decode test that quietly skips is
// how a table with twenty-one wrong entries passed for months.
const decoderEnv = "FREESMS_BARCODE_DECODER"

// raster draws the modules as a PNG a decoder can read: each module a few
// pixels wide, white quiet zones either side.
func raster(t *testing.T, text string) string {
	t.Helper()
	widths, err := Modules(text)
	if err != nil {
		t.Fatalf("Modules(%q): %v", text, err)
	}
	const px, height = 3, 60
	total := 2 * quietZone
	for _, w := range widths {
		total += w
	}
	img := image.NewGray(image.Rect(0, 0, total*px, height))
	for i := range img.Pix {
		img.Pix[i] = 0xff
	}
	x := quietZone
	for i, w := range widths {
		if i%2 == 0 {
			for col := x * px; col < (x+w)*px; col++ {
				for y := 0; y < height; y++ {
					img.SetGray(col, y, color.Gray{Y: 0})
				}
			}
		}
		x += w
	}
	path := filepath.Join(t.TempDir(), "label.png")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		t.Fatalf("encode: %v", err)
	}
	return path
}

// A label decodes back to the text that was printed, by a reader that shares
// no code with the encoder.
func TestLabelsDecodeWithAnIndependentReader(t *testing.T) {
	cmd := os.Getenv(decoderEnv)
	if cmd == "" {
		// The CI job that installs a decoder sets this too, so the one place
		// meant to verify cannot skip by accident. Everywhere else -- a
		// laptop, the shared Go job -- has no decoder, and says so.
		if os.Getenv(decoderEnv+"_REQUIRED") != "" {
			t.Fatalf("%s is not set where a decoder is required; the barcode would go unverified", decoderEnv)
		}
		t.Skipf("%s not set; no independent decoder to check against", decoderEnv)
	}
	parts := strings.Fields(cmd)

	// Ordinary part numbers, and texts chosen so the check symbol lands in
	// the range 83 to 102 that the first table had wrong -- the check symbol
	// can be any value, which is why a fault there broke labels at random.
	for _, text := range []string{
		"BP-100", "OIL-5W30", "SP-IR", "A", "Z", "0123456789",
		"WB-24", "GP-4C", "TU-1", "x~", "tuvwxyz", "{|}~",
	} {
		path := raster(t, text)
		out, err := exec.Command(parts[0], append(parts[1:], path)...).Output()
		if err != nil {
			t.Errorf("%q: the decoder could not read it: %v", text, err)
			continue
		}
		if got := strings.TrimSpace(string(out)); got != text {
			t.Errorf("printed %q, the decoder read %q", text, got)
		}
	}
}
