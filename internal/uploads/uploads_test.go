package uploads_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CDRO/Inventory/internal/uploads"
)

const validName = "0190a1b2-c3d4-7e5f-8a6b-7c8d9e0f1a2b.jpg"

func TestSaveReadRemove(t *testing.T) {
	t.Parallel()

	dir, err := uploads.NewDir(filepath.Join(t.TempDir(), "ingest"))
	require.NoError(t, err)

	require.NoError(t, dir.Save(validName, []byte("jpeg bytes")))
	got, err := dir.Read(validName)
	require.NoError(t, err)
	assert.Equal(t, []byte("jpeg bytes"), got)

	require.NoError(t, dir.Remove(validName))
	_, err = dir.Read(validName)
	assert.ErrorIs(t, err, os.ErrNotExist)

	assert.NoError(t, dir.Remove(validName), "removing a photo that is already gone is fine")
}

// TestOnlyGeneratedNamesReachTheFilesystem — a name from anywhere else, however
// it got into a job row, must not become a path.
func TestOnlyGeneratedNamesReachTheFilesystem(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	dir, err := uploads.NewDir(filepath.Join(root, "ingest"))
	require.NoError(t, err)

	secret := filepath.Join(root, "secret.jpg")
	require.NoError(t, os.WriteFile(secret, []byte("not yours"), 0o600))

	for _, name := range []string{
		"../secret.jpg",
		"..\\secret.jpg",
		"/etc/passwd",
		"0190a1b2-c3d4-7e5f-8a6b-7c8d9e0f1a2b.svg",
		"0190A1B2-C3D4-7E5F-8A6B-7C8D9E0F1A2B.jpg",
		"0190a1b2-c3d4-7e5f-8a6b-7c8d9e0f1a2b.jpg/../../secret.jpg",
		"",
	} {
		_, err := dir.Read(name)
		assert.ErrorIs(t, err, uploads.ErrInvalidName, name)
		assert.ErrorIs(t, dir.Save(name, []byte("x")), uploads.ErrInvalidName, name)
		assert.ErrorIs(t, dir.Remove(name), uploads.ErrInvalidName, name)
	}

	still, err := os.ReadFile(secret)
	require.NoError(t, err)
	assert.Equal(t, []byte("not yours"), still)
}

func TestSaveLeavesNoTemporaryFiles(t *testing.T) {
	t.Parallel()

	root := filepath.Join(t.TempDir(), "ingest")
	dir, err := uploads.NewDir(root)
	require.NoError(t, err)
	require.NoError(t, dir.Save(validName, []byte("data")))

	entries, err := os.ReadDir(root)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, validName, entries[0].Name())
}
