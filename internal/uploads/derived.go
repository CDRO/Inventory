package uploads

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// DerivedDir is where derived pictures live: the thumbnails, previews and row
// crops made from the photos in IngestDir and the pictures in
// ProductImagesDir (docs/specs/43-image-derivatives.md).
//
// On the cache volume, beside the suggestion cache, and not on uploads: every
// file here can be made again from its source, so it belongs with the tier
// docs/specs/15-backup-restore-and-export.md leaves out of the backup, and a
// missing file is a miss, never a loss.
const DerivedDir = "/data/cache/derived"

// Area names the source directory a derivative was made from. It is the
// first path segment under DerivedDir, and an allow-list: nothing else is a
// place derivatives are written.
type Area string

// The areas.
const (
	AreaIngest   Area = "ingest"
	AreaProducts Area = "products"
)

func (a Area) valid() bool {
	return a == AreaIngest || a == AreaProducts
}

// derivedName is the only filename shape a derived directory accepts: a
// whole-picture variant, or a row crop named by the proposal's positional
// row id, with the extension of its own encoding.
var derivedName = regexp.MustCompile(`^(preview|thumb-(96|192|384|768)|rows-[0-9]{1,6}-thumb-(192|384))\.(jpg|png)$`)

// stemPattern is a source file's name without its extension: the UUID the
// server generated for it. It is the derived directory's name.
var stemPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// rowFilePrefix is what every row crop's filename starts with.
const rowFilePrefix = "rows-"

// Stem is a source filename without its extension — the key its derivatives
// are kept under.
func Stem(name string) string {
	return strings.TrimSuffix(name, filepath.Ext(name))
}

// Derived is the derived-picture cache: one directory per area, one per
// source beneath it, and the variants as files inside that.
type Derived struct {
	root string
}

// NewDerived returns the cache at root, creating it if needed.
func NewDerived(root string) (*Derived, error) {
	if err := os.MkdirAll(root, 0o750); err != nil {
		return nil, fmt.Errorf("uploads: create %s: %w", root, err)
	}
	return &Derived{root: root}, nil
}

// dir is the Dir for one source's derivatives. It validates the area and the
// stem and nothing else touches the filesystem with them: both appear in a
// path, and a stem comes from a filename a job row or a product recorded.
func (d *Derived) dir(area Area, stem string) (*Dir, error) {
	if !area.valid() || !stemPattern.MatchString(stem) {
		return nil, ErrInvalidName
	}
	return &Dir{root: filepath.Join(d.root, string(area), stem), names: derivedName}, nil
}

// Write stores one derivative, through the same temporary-file-and-rename a
// photo gets: a crash mid-write leaves no file, never a truncated picture.
func (d *Derived) Write(area Area, stem, name string, data []byte) error {
	dir, err := d.dir(area, stem)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir.root, 0o750); err != nil {
		return fmt.Errorf("uploads: create %s: %w", dir.root, err)
	}
	return dir.Save(name, data)
}

// Read returns one derivative. A missing one is os.ErrNotExist.
func (d *Derived) Read(area Area, stem, name string) ([]byte, error) {
	dir, err := d.dir(area, stem)
	if err != nil {
		return nil, err
	}
	return dir.Read(name)
}

// Exists reports whether one derivative is on disk.
func (d *Derived) Exists(area Area, stem, name string) bool {
	dir, err := d.dir(area, stem)
	if err != nil {
		return false
	}
	return dir.Exists(name)
}

// RemoveSource deletes every derivative of a source. A source with none is
// not an error.
func (d *Derived) RemoveSource(area Area, stem string) error {
	dir, err := d.dir(area, stem)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(dir.root); err != nil {
		return fmt.Errorf("uploads: remove derived %s: %w", stem, err)
	}
	return nil
}

// RemoveRows deletes a source's row crops and keeps its whole-picture
// variants: a proposal analysed again has new boxes, and the old crops must
// not survive beside them.
func (d *Derived) RemoveRows(area Area, stem string) error {
	dir, err := d.dir(area, stem)
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(dir.root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("uploads: list derived %s: %w", stem, err)
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), rowFilePrefix) || !derivedName.MatchString(entry.Name()) {
			continue
		}
		if err := dir.Remove(entry.Name()); err != nil {
			return err
		}
	}
	return nil
}

// Stems lists the sources an area holds derivatives for. Entries that are
// not a source's directory — a stray file, a misnamed directory — are not
// listed and not touched.
func (d *Derived) Stems(area Area) ([]string, error) {
	if !area.valid() {
		return nil, ErrInvalidName
	}
	entries, err := os.ReadDir(filepath.Join(d.root, string(area)))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("uploads: list derived area %s: %w", area, err)
	}
	var stems []string
	for _, entry := range entries {
		if entry.IsDir() && stemPattern.MatchString(entry.Name()) {
			stems = append(stems, entry.Name())
		}
	}
	return stems, nil
}
