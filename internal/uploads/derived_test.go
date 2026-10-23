package uploads_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CDRO/Inventory/internal/uploads"
)

const (
	stemA = "0192a5f0-1111-7000-8000-000000000001"
	stemB = "0192a5f0-2222-7000-8000-000000000002"
)

func newDerived(t *testing.T) *uploads.Derived {
	t.Helper()
	d, err := uploads.NewDerived(filepath.Join(t.TempDir(), "derived"))
	require.NoError(t, err)
	return d
}

func TestDerivedWritesAndReadsAVariant(t *testing.T) {
	t.Parallel()
	d := newDerived(t)

	require.NoError(t, d.Write(uploads.AreaProducts, stemA, "thumb-96.jpg", []byte("jpeg bytes")))
	require.NoError(t, d.Write(uploads.AreaProducts, stemA, "rows-12-thumb-384.png", []byte("png bytes")))

	data, err := d.Read(uploads.AreaProducts, stemA, "thumb-96.jpg")
	require.NoError(t, err)
	assert.Equal(t, []byte("jpeg bytes"), data)
	assert.True(t, d.Exists(uploads.AreaProducts, stemA, "rows-12-thumb-384.png"))
	assert.False(t, d.Exists(uploads.AreaProducts, stemA, "thumb-192.jpg"))

	_, err = d.Read(uploads.AreaProducts, stemA, "preview.jpg")
	assert.ErrorIs(t, err, os.ErrNotExist, "a variant not yet made")
	_, err = d.Read(uploads.AreaIngest, stemA, "thumb-96.jpg")
	assert.ErrorIs(t, err, os.ErrNotExist, "areas are separate")
}

// TestDerivedRefusesAnythingButGeneratedNames — area, stem and filename all
// end up in a path, so each is matched against its one allowed shape rather
// than sanitised.
func TestDerivedRefusesAnythingButGeneratedNames(t *testing.T) {
	t.Parallel()
	d := newDerived(t)

	cases := []struct {
		area       uploads.Area
		stem, name string
	}{
		{"other", stemA, "thumb-96.jpg"},
		{uploads.AreaIngest, "..", "thumb-96.jpg"},
		{uploads.AreaIngest, "not-a-uuid", "thumb-96.jpg"},
		{uploads.AreaIngest, stemA + ".jpg", "thumb-96.jpg"},
		{uploads.AreaIngest, stemA, "../thumb-96.jpg"},
		{uploads.AreaIngest, stemA, "thumb-100.jpg"},
		{uploads.AreaIngest, stemA, "preview.gif"},
		{uploads.AreaIngest, stemA, "rows-a-thumb-192.jpg"},
		{uploads.AreaIngest, stemA, "rows-1-thumb-96.jpg"},
		{uploads.AreaIngest, stemA, "preview"},
	}
	for _, c := range cases {
		assert.ErrorIs(t, d.Write(c.area, c.stem, c.name, []byte("x")), uploads.ErrInvalidName, "%s/%s/%s", c.area, c.stem, c.name)
		_, err := d.Read(c.area, c.stem, c.name)
		assert.ErrorIs(t, err, uploads.ErrInvalidName, "%s/%s/%s", c.area, c.stem, c.name)
		assert.False(t, d.Exists(c.area, c.stem, c.name))
	}
	assert.ErrorIs(t, d.RemoveSource("other", stemA), uploads.ErrInvalidName)
	assert.ErrorIs(t, d.RemoveRows(uploads.AreaIngest, "x"), uploads.ErrInvalidName)
	_, err := d.Stems("other")
	assert.ErrorIs(t, err, uploads.ErrInvalidName)
}

// TestDirRemoveTakesDerivativesWithIt — the invariant that a derivative never
// outlives its source, enforced where sources are removed.
func TestDirRemoveTakesDerivativesWithIt(t *testing.T) {
	t.Parallel()
	d := newDerived(t)
	dir, err := uploads.NewDir(filepath.Join(t.TempDir(), "ingest"))
	require.NoError(t, err)
	dir.RemoveDerivedWith(d, uploads.AreaIngest)

	source := stemA + ".jpg"
	require.NoError(t, dir.Save(source, []byte("photo")))
	require.NoError(t, d.Write(uploads.AreaIngest, stemA, "thumb-96.jpg", []byte("t")))
	require.NoError(t, d.Write(uploads.AreaIngest, stemA, "rows-0-thumb-192.jpg", []byte("r")))
	// Another source's derivatives are not touched.
	require.NoError(t, d.Write(uploads.AreaIngest, stemB, "thumb-96.jpg", []byte("t")))

	require.NoError(t, dir.Remove(source))

	assert.False(t, dir.Exists(source))
	_, err = d.Read(uploads.AreaIngest, stemA, "thumb-96.jpg")
	assert.ErrorIs(t, err, os.ErrNotExist)
	_, err = d.Read(uploads.AreaIngest, stemA, "rows-0-thumb-192.jpg")
	assert.ErrorIs(t, err, os.ErrNotExist)
	assert.True(t, d.Exists(uploads.AreaIngest, stemB, "thumb-96.jpg"))

	// Removing again is still not an error, with or without derivatives.
	require.NoError(t, dir.Remove(source))
}

func TestRemoveRowsKeepsTheWholePictureVariants(t *testing.T) {
	t.Parallel()
	d := newDerived(t)

	for _, name := range []string{"preview.jpg", "thumb-96.jpg", "rows-0-thumb-192.jpg", "rows-0-thumb-384.jpg", "rows-15-thumb-192.png"} {
		require.NoError(t, d.Write(uploads.AreaIngest, stemA, name, []byte("x")))
	}

	require.NoError(t, d.RemoveRows(uploads.AreaIngest, stemA))

	assert.True(t, d.Exists(uploads.AreaIngest, stemA, "preview.jpg"))
	assert.True(t, d.Exists(uploads.AreaIngest, stemA, "thumb-96.jpg"))
	for _, name := range []string{"rows-0-thumb-192.jpg", "rows-0-thumb-384.jpg", "rows-15-thumb-192.png"} {
		assert.False(t, d.Exists(uploads.AreaIngest, stemA, name), name)
	}

	require.NoError(t, d.RemoveRows(uploads.AreaIngest, stemB), "a source with no derivatives at all")
}

func TestStemsListsOnlySourceDirectories(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "derived")
	d, err := uploads.NewDerived(root)
	require.NoError(t, err)

	stems, err := d.Stems(uploads.AreaProducts)
	require.NoError(t, err)
	assert.Empty(t, stems, "an area nothing was written to")

	require.NoError(t, d.Write(uploads.AreaProducts, stemA, "thumb-96.jpg", []byte("x")))
	require.NoError(t, d.Write(uploads.AreaProducts, stemB, "thumb-96.jpg", []byte("x")))
	// Things that are not a source's directory.
	require.NoError(t, os.MkdirAll(filepath.Join(root, "products", "not-a-stem"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(root, "products", "stray.jpg"), []byte("x"), 0o640))

	stems, err = d.Stems(uploads.AreaProducts)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{stemA, stemB}, stems)

	require.NoError(t, d.RemoveSource(uploads.AreaProducts, stemA))
	stems, err = d.Stems(uploads.AreaProducts)
	require.NoError(t, err)
	assert.Equal(t, []string{stemB}, stems)
}

func TestDirListsAndChecksGeneratedNamesOnly(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "products")
	dir, err := uploads.NewPictureDir(root)
	require.NoError(t, err)

	require.NoError(t, dir.Save(stemA+".jpg", []byte("a")))
	require.NoError(t, dir.Save(stemB+".svg", []byte("<svg/>")))
	require.NoError(t, os.WriteFile(filepath.Join(root, ".upload-leftover"), []byte("x"), 0o640))
	require.NoError(t, os.WriteFile(filepath.Join(root, "readme.txt"), []byte("x"), 0o640))

	names, err := dir.List()
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{stemA + ".jpg", stemB + ".svg"}, names)

	assert.True(t, dir.Exists(stemA+".jpg"))
	assert.False(t, dir.Exists(stemA+".png"))
	assert.False(t, dir.Exists("readme.txt"), "not a generated name, so it does not exist here")
	assert.False(t, dir.Exists("../"+stemA+".jpg"))

	assert.Equal(t, stemA, uploads.Stem(stemA+".jpg"))
	assert.Equal(t, stemB, uploads.Stem(stemB+".svg"))

	var pathErr *os.PathError
	_, err = (&uploads.Dir{}).List()
	assert.True(t, errors.As(err, &pathErr) || err != nil, "an unusable directory is an error, not an empty list")
}
