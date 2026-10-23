package derive_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CDRO/Inventory/internal/derive"
	"github.com/CDRO/Inventory/internal/images"
	"github.com/CDRO/Inventory/internal/uploads"
)

// TestSavingMakesThumbnailsForEverySavedPicture — the product picture
// directory, wrapped: a save schedules the set, an SVG gets none, a remove
// takes the set with the picture.
func TestSavingMakesThumbnailsForEverySavedPicture(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	dir, err := uploads.NewPictureDir(filepath.Join(t.TempDir(), "products"))
	require.NoError(t, err)
	dir.RemoveDerivedWith(f.store, uploads.AreaProducts)
	saving := derive.Saving{Dir: dir, Service: f.svc}

	require.NoError(t, saving.Save(sourceA, f.photo))
	svg := "0192a5f0-3333-7000-8000-000000000003.svg"
	require.NoError(t, saving.Save(svg, []byte("<svg xmlns='http://www.w3.org/2000/svg'/>")))
	f.svc.Wait()

	for _, v := range images.PhotoVariants {
		assert.True(t, f.store.Exists(uploads.AreaProducts, uploads.Stem(sourceA), string(v)+".jpg"), v)
	}
	stems, err := f.store.Stems(uploads.AreaProducts)
	require.NoError(t, err)
	assert.Equal(t, []string{uploads.Stem(sourceA)}, stems, "the icon has no derivatives")

	data, err := saving.Read(sourceA)
	require.NoError(t, err)
	assert.Equal(t, f.photo, data)

	require.NoError(t, saving.Remove(sourceA))
	assert.False(t, dir.Exists(sourceA))
	_, err = f.store.Read(uploads.AreaProducts, uploads.Stem(sourceA), "thumb-96.jpg")
	assert.ErrorIs(t, err, os.ErrNotExist)
}

// TestForJobsWorksInTheIngestArea — the job-facing view writes where the job
// photo routes read.
func TestForJobsWorksInTheIngestArea(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	jobs := derive.ForJobs(f.svc)
	ctx := context.Background()

	require.NoError(t, jobs.EnsurePhotoSet(ctx, sourceA, f.photo))
	require.NoError(t, jobs.ReplaceRowSet(ctx, sourceA, f.photo, map[string]images.Box{"3": {Width: 1, Height: 1}}))

	stem := uploads.Stem(sourceA)
	assert.True(t, f.store.Exists(uploads.AreaIngest, stem, "preview.jpg"))
	assert.True(t, f.store.Exists(uploads.AreaIngest, stem, "rows-3-thumb-192.jpg"))
	assert.True(t, f.store.Exists(uploads.AreaIngest, stem, "rows-3-thumb-384.jpg"))
	stems, err := f.store.Stems(uploads.AreaProducts)
	require.NoError(t, err)
	assert.Empty(t, stems)
}
