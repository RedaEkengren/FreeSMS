// Package storage keeps the files an installation cannot regenerate.
//
// Inspection photographs are the only thing in this system that a database
// dump does not contain. Everything else can be rebuilt; these cannot, which
// is why DEPLOY.md calls them out separately in the backup procedure.
package storage

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	_ "image/png" // so a PNG from a screenshot decodes
	"io"
	"os"
	"path/filepath"

	"golang.org/x/image/draw"
)

const (
	// MaxUpload is what is read from a request before giving up. Phones
	// produce files of a few megabytes; anything far larger is a mistake or an
	// attack, and either way reading it to find out is the wrong move.
	MaxUpload = 12 << 20 // 12 MiB

	// maxDimension is the longest side kept. A brake disc does not need four
	// thousand pixels, and a workshop's storage and a customer's mobile data
	// both do better without them.
	maxDimension = 1600

	// quality is a deliberate compromise: visibly fine for showing a worn
	// component, roughly a tenth the size of the original.
	quality = 82
)

// ErrNotAnImage is returned when the bytes do not decode.
var ErrNotAnImage = errors.New("storage: that is not an image this system can read")

// Store holds files under a root directory.
type Store struct{ root string }

// New returns a Store, creating the root if it is missing.
func New(root string) (*Store, error) {
	if root == "" {
		return nil, errors.New("storage: no directory configured")
	}
	if err := os.MkdirAll(root, 0o750); err != nil {
		return nil, fmt.Errorf("storage: create %s: %w", root, err)
	}

	// Prove it is writable now, rather than discovering it is not when
	// somebody uploads the first photograph of the day.
	//
	// The case this catches is ordinary: a named Docker volume arrives owned
	// by root while the container runs as an unprivileged user, so the
	// directory exists, is readable, and cannot be written to. Failing at
	// startup puts the error next to the mistake.
	probe, err := os.CreateTemp(root, ".writable-*")
	if err != nil {
		return nil, fmt.Errorf(
			"storage: %s is not writable by this process: %w "+
				"(a Docker named volume is created owned by root; the image must "+
				"seed the directory with the right owner)", root, err)
	}
	name := probe.Name()
	probe.Close()
	if err := os.Remove(name); err != nil {
		return nil, fmt.Errorf("storage: clean up probe file: %w", err)
	}

	return &Store{root: root}, nil
}

// Saved describes a stored photograph.
type Saved struct {
	Key         string
	ContentType string
	ByteSize    int64
}

// SavePhoto decodes, normalises and writes an uploaded image.
//
// It re-encodes rather than storing the bytes as they arrived, which does
// three things at once:
//
//   - It proves the file is an image. A .jpg that is really something else
//     fails here rather than in whatever opens it later.
//   - It removes EXIF. A phone writes the location into every photograph, and
//     for a mobile mechanic that is a customer's home address travelling with
//     a picture the customer is then sent.
//   - It bounds the size, so a workshop's disk and a customer on mobile data
//     are not spending anything on four-thousand-pixel brake discs.
func (s *Store) SavePhoto(r io.Reader) (Saved, error) {
	// LimitReader rather than trusting a declared length: the declaration is
	// the client's, and this is not.
	src, format, err := image.Decode(io.LimitReader(r, MaxUpload))
	if err != nil {
		return Saved{}, fmt.Errorf("%w (%v)", ErrNotAnImage, err)
	}
	_ = format

	out := downscale(src)

	key, err := newKey()
	if err != nil {
		return Saved{}, err
	}
	path := s.pathFor(key)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return Saved{}, fmt.Errorf("storage: create directory: %w", err)
	}

	// Write to a temporary name and rename into place, so a crash halfway
	// leaves no half-written photograph that looks like a real one.
	tmp, err := os.CreateTemp(filepath.Dir(path), ".partial-*")
	if err != nil {
		return Saved{}, fmt.Errorf("storage: create file: %w", err)
	}
	defer os.Remove(tmp.Name())

	if err := jpeg.Encode(tmp, out, &jpeg.Options{Quality: quality}); err != nil {
		tmp.Close()
		return Saved{}, fmt.Errorf("storage: encode: %w", err)
	}
	size, err := tmp.Seek(0, io.SeekEnd)
	if err != nil {
		tmp.Close()
		return Saved{}, fmt.Errorf("storage: size: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return Saved{}, fmt.Errorf("storage: close: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return Saved{}, fmt.Errorf("storage: store: %w", err)
	}

	return Saved{Key: key, ContentType: "image/jpeg", ByteSize: size}, nil
}

// Open returns a stored photograph for reading.
func (s *Store) Open(key string) (io.ReadCloser, error) {
	if !validKey(key) {
		return nil, os.ErrNotExist
	}
	return os.Open(s.pathFor(key))
}

// Remove deletes a photograph.
//
// Deleting the row is not enough. An inspection photograph is personal data --
// it catches other customers' plates, colleagues, paperwork on a bench -- so
// an erasure that leaves the file on disk has not erased anything.
func (s *Store) Remove(key string) error {
	if !validKey(key) {
		return os.ErrNotExist
	}
	err := os.Remove(s.pathFor(key))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// pathFor shards by the first two characters, because one directory with
// fifty thousand files in it is slow to list and unpleasant to look at.
//
// The key is generated here and validated on the way back in, so nothing a
// caller supplies ever reaches a path.
func (s *Store) pathFor(key string) string {
	return filepath.Join(s.root, key[:2], key+".jpg")
}

func newKey() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("storage: generate key: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// validKey accepts only what newKey produces. A key arriving from a URL must
// not be able to name a file outside the store, and the cheapest way to
// guarantee that is to accept nothing that is not thirty-two hex characters.
func validKey(key string) bool {
	if len(key) != 32 {
		return false
	}
	for _, r := range key {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

func downscale(src image.Image) image.Image {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= maxDimension && h <= maxDimension {
		return src
	}
	if w > h {
		h = h * maxDimension / w
		w = maxDimension
	} else {
		w = w * maxDimension / h
		h = maxDimension
	}
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	// CatmullRom over the faster kernels: this runs once per photograph, and
	// a resampling artefact on a picture of a worn component is the kind of
	// thing an argument gets had about.
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, b, draw.Over, nil)
	return dst
}
