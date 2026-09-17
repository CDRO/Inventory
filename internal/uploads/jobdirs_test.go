package uploads_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CDRO/Inventory/internal/uploads"
)

const validPNG = "0190a1b2-c3d4-7e5f-8a6b-7c8d9e0f1a2c.png"

// TestJobDirsKeepEachJobsFilesApart — a job's files are read back only through
// that job, and removing them leaves every other job's alone.
func TestJobDirsKeepEachJobsFilesApart(t *testing.T) {
	t.Parallel()

	dirs, err := uploads.NewJobDirs(filepath.Join(t.TempDir(), "cutouts"))
	require.NoError(t, err)
	mine, theirs := uuid.New(), uuid.New()

	_, err = dirs.Read(mine, validPNG)
	assert.ErrorIs(t, err, os.ErrNotExist, "a job with no files yet")

	require.NoError(t, dirs.Save(mine, validPNG, []byte("mine")))
	require.NoError(t, dirs.Save(theirs, validPNG, []byte("theirs")))

	got, err := dirs.Read(mine, validPNG)
	require.NoError(t, err)
	assert.Equal(t, []byte("mine"), got, "the same file name in another job is another file")

	require.NoError(t, dirs.RemoveAll(mine))
	_, err = dirs.Read(mine, validPNG)
	assert.ErrorIs(t, err, os.ErrNotExist)

	got, err = dirs.Read(theirs, validPNG)
	require.NoError(t, err)
	assert.Equal(t, []byte("theirs"), got)

	assert.NoError(t, dirs.RemoveAll(mine), "removing files that are already gone is fine")
}

// TestJobDirsAcceptOnlyGeneratedNames — the name rule of Dir holds inside a
// job's directory too.
func TestJobDirsAcceptOnlyGeneratedNames(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	dirs, err := uploads.NewJobDirs(filepath.Join(root, "cutouts"))
	require.NoError(t, err)
	job := uuid.New()

	for _, name := range []string{"../../secret.png", "cutout.png", ""} {
		assert.ErrorIs(t, dirs.Save(job, name, []byte("x")), uploads.ErrInvalidName, name)
		_, err := dirs.Read(job, name)
		assert.ErrorIs(t, err, uploads.ErrInvalidName, name)
	}
	_, err = os.Stat(filepath.Join(root, "secret.png"))
	assert.ErrorIs(t, err, os.ErrNotExist)
}
