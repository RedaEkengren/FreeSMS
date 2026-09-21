package storage

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func testJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for x := 0; x < w; x++ {
		for y := 0; y < h; y++ {
			img.Set(x, y, color.RGBA{uint8(x % 256), uint8(y % 256), 120, 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatalf("encode fixture: %v", err)
	}
	return buf.Bytes()
}

func TestSaveAndOpen(t *testing.T) {
	s := newStore(t)
	saved, err := s.SavePhoto(bytes.NewReader(testJPEG(t, 400, 300)))
	if err != nil {
		t.Fatalf("SavePhoto: %v", err)
	}
	if saved.ContentType != "image/jpeg" || saved.ByteSize <= 0 {
		t.Errorf("saved = %+v, want a jpeg with a size", saved)
	}

	f, err := s.Open(saved.Key)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer f.Close()
	if _, err := jpeg.Decode(f); err != nil {
		t.Errorf("what was stored does not decode: %v", err)
	}
}

// A phone writes the location into every photograph. For a mobile mechanic
// that is a customer's home address, travelling with a picture the customer
// is then sent a link to.
func TestExifIsNotKept(t *testing.T) {
	// A JPEG carrying an APP1/Exif segment with a recognisable marker.
	original := testJPEG(t, 100, 100)
	exif := []byte{0xFF, 0xE1, 0x00, 0x20, 'E', 'x', 'i', 'f', 0x00, 0x00}
	exif = append(exif, []byte("GPSLATITUDE-SECRET-MARKER")...)
	withExif := append(append(append([]byte{}, original[:2]...), exif...), original[2:]...)

	s := newStore(t)
	saved, err := s.SavePhoto(bytes.NewReader(withExif))
	if err != nil {
		t.Fatalf("SavePhoto: %v", err)
	}

	f, _ := s.Open(saved.Key)
	defer f.Close()
	stored, _ := io.ReadAll(f)
	if bytes.Contains(stored, []byte("SECRET-MARKER")) {
		t.Error("the stored photograph still carries its EXIF segment")
	}
}

// A workshop's disk and a customer on mobile data should not be paying for
// four-thousand-pixel brake discs.
func TestLargePhotosAreScaledDown(t *testing.T) {
	s := newStore(t)
	saved, err := s.SavePhoto(bytes.NewReader(testJPEG(t, 4000, 3000)))
	if err != nil {
		t.Fatalf("SavePhoto: %v", err)
	}
	f, _ := s.Open(saved.Key)
	defer f.Close()
	cfg, err := jpeg.DecodeConfig(f)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if cfg.Width != maxDimension {
		t.Errorf("width = %d, want %d", cfg.Width, maxDimension)
	}
	if cfg.Height != 1200 {
		t.Errorf("height = %d, want 1200 -- the aspect ratio was not kept", cfg.Height)
	}
}

func TestSmallPhotosAreNotEnlarged(t *testing.T) {
	s := newStore(t)
	saved, _ := s.SavePhoto(bytes.NewReader(testJPEG(t, 320, 240)))
	f, _ := s.Open(saved.Key)
	defer f.Close()
	cfg, _ := jpeg.DecodeConfig(f)
	if cfg.Width != 320 || cfg.Height != 240 {
		t.Errorf("a small photo became %dx%d", cfg.Width, cfg.Height)
	}
}

// A file that is not an image must fail here, not in whatever opens it later.
func TestSomethingThatIsNotAnImageIsRefused(t *testing.T) {
	s := newStore(t)
	for _, in := range [][]byte{
		[]byte("#!/bin/sh\necho hello\n"),
		[]byte("%PDF-1.4"),
		{},
	} {
		if _, err := s.SavePhoto(bytes.NewReader(in)); !errors.Is(err, ErrNotAnImage) {
			t.Errorf("SavePhoto(%q) = %v, want ErrNotAnImage", in[:min(8, len(in))], err)
		}
	}
}

func TestPNGIsAccepted(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 50, 50))
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode: %v", err)
	}
	s := newStore(t)
	if _, err := s.SavePhoto(&buf); err != nil {
		t.Errorf("a PNG was refused: %v", err)
	}
}

// A key arriving from a URL must not be able to name a file outside the store.
func TestKeysFromOutsideCannotEscape(t *testing.T) {
	s := newStore(t)
	for _, key := range []string{
		"../../etc/passwd",
		"..",
		"/etc/passwd",
		strings.Repeat("g", 32),
		"",
	} {
		if _, err := s.Open(key); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("Open(%q) = %v, want ErrNotExist", key, err)
		}
		if err := s.Remove(key); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("Remove(%q) = %v, want ErrNotExist", key, err)
		}
	}
}

// Deleting the row is not enough; the file is the personal data.
func TestRemoveDeletesTheFile(t *testing.T) {
	s := newStore(t)
	saved, _ := s.SavePhoto(bytes.NewReader(testJPEG(t, 100, 100)))
	if err := s.Remove(saved.Key); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := s.Open(saved.Key); !errors.Is(err, os.ErrNotExist) {
		t.Error("the file survived its deletion")
	}
	// Removing what is already gone is not an error: an erasure that has
	// partly run must be able to run again.
	if err := s.Remove(saved.Key); err != nil {
		t.Errorf("removing twice returned %v", err)
	}
}

// A crash halfway must not leave something that looks like a photograph.
func TestNoPartialFilesAreLeftBehind(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir)
	if _, err := s.SavePhoto(bytes.NewReader(testJPEG(t, 200, 200))); err != nil {
		t.Fatalf("SavePhoto: %v", err)
	}
	var partial int
	filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && strings.HasPrefix(filepath.Base(p), ".partial-") {
			partial++
		}
		return nil
	})
	if partial != 0 {
		t.Errorf("%d partial files were left behind", partial)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
