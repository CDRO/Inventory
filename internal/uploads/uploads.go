// Package uploads keeps user photos on the uploads volume
// (docs/specs/04-backend-api-conventions.md).
//
// A Dir is one storage area — ingest photos awaiting review, say — and it
// accepts only the filenames the server itself generates: a UUID and the
// extension implied by the image's own bytes. Every name arriving here is
// checked against that shape before it touches the filesystem, so no value
// that ever passed through a client — a job row edited by hand, a path
// parameter — can reach outside the directory.
package uploads

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
)

// IngestDir is where photos backing a review job live.
const IngestDir = "/data/uploads/ingest"

// ProductImagesDir is permanent storage for product pictures taken from a
// user's own photo (docs/specs/07-shopping-list-reconciliation.md, "Product
// images — permanent"). Unlike IngestDir it has no retention sweep, and unlike
// the image-suggestion cache it has no eviction: a picture someone chose for a
// product must never disappear because a cleanup ran.
const ProductImagesDir = "/data/uploads/products"

// ErrInvalidName is a filename that is not one the server generates.
var ErrInvalidName = errors.New("uploads: not a generated filename")

// generatedName is the only filename shape accepted: a lowercase UUID plus
// .jpg or .png. No separators, no dots beyond the extension, nothing a path
// could be built from.
var generatedName = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\.(jpg|png)$`)

// Dir is one upload area on disk.
type Dir struct {
	root string
}

// NewDir returns the area at root, creating it if needed.
func NewDir(root string) (*Dir, error) {
	if err := os.MkdirAll(root, 0o750); err != nil {
		return nil, fmt.Errorf("uploads: create %s: %w", root, err)
	}
	return &Dir{root: root}, nil
}

func (d *Dir) path(name string) (string, error) {
	if !generatedName.MatchString(name) {
		return "", ErrInvalidName
	}
	return filepath.Join(d.root, name), nil
}

// Save writes a photo.
//
// Through a temporary file and a rename, so a crash mid-write leaves either no
// file or the whole file — never a truncated photo that a review weeks later
// would render as a broken image.
func (d *Dir) Save(name string, data []byte) error {
	target, err := d.path(name)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(d.root, ".upload-*")
	if err != nil {
		return fmt.Errorf("uploads: create temp: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("uploads: write: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("uploads: close: %w", err)
	}
	if err := os.Rename(tmp.Name(), target); err != nil {
		return fmt.Errorf("uploads: rename: %w", err)
	}
	return nil
}

// Read returns a photo's bytes. A missing file is os.ErrNotExist.
func (d *Dir) Read(name string) ([]byte, error) {
	target, err := d.path(name)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(target)
}

// Remove deletes a photo. Removing one that is already gone is not an error:
// the end state the caller wanted already holds.
func (d *Dir) Remove(name string) error {
	target, err := d.path(name)
	if err != nil {
		return err
	}
	if err := os.Remove(target); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("uploads: remove: %w", err)
	}
	return nil
}
