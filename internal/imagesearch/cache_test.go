package imagesearch_test

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CDRO/Inventory/internal/imagesearch"
	"github.com/CDRO/Inventory/internal/store"
)

// TestOpenForgetsARowWhoseFileIsGone is the cache half of
// docs/specs/15-backup-restore-and-export.md.
//
// `cached_images` rows travel in a backup; the files they name deliberately do
// not, because they are re-fetchable by definition. Every row is therefore
// fileless immediately after a restore, and without this the serving handler
// would answer a permanent 404 for each one while the database went on
// insisting it had the picture — a broken image with a dangling row that no
// sweep ever clears, because the orphan sweep only looks the other way.
//
// This is also the case an eviction that died between deleting the file and
// deleting the row leaves behind, which is why it is worth having outside a
// restore too.
func TestOpenForgetsARowWhoseFileIsGone(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	rows := newMemStore()
	cache := imagesearch.NewCache(dir, rows, nil, discardLogger())

	const hash = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	require.NoError(t, rows.PutCachedImage(context.Background(), store.CachedImage{
		Hash:        hash,
		SourceURL:   "https://example.test/butter.png",
		ContentType: "image/png",
		ByteSize:    12,
		Status:      store.CachedOK,
	}))
	// No file is written: exactly the state a restored dump produces.

	_, _, err := cache.Open(context.Background(), hash)
	require.ErrorIs(t, err, store.ErrNotFound, "a row with no file is a cache miss, not a served 404 forever")

	_, err = rows.CachedImageByHash(context.Background(), hash)
	assert.ErrorIs(t, err, store.ErrNotFound,
		"the stale row must be gone, so the next suggestion re-fetches instead of pointing at nothing")
}

// TestOpenKeepsAnUnusableRowWhoseFileIsAbsent guards the trap in the fix
// above.
//
// An 'unusable' row is the negative cache: a candidate that was too large, too
// many pixels, or would not decode. It has no file *by design*
// (docs/specs/07-shopping-list-reconciliation.md), so a self-heal that deleted
// every fileless row would delete exactly the memory that stops a broken
// candidate being re-downloaded on every run of the same query, forever.
func TestOpenKeepsAnUnusableRowWhoseFileIsAbsent(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	rows := newMemStore()
	cache := imagesearch.NewCache(dir, rows, nil, discardLogger())

	const hash = "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
	require.NoError(t, rows.PutCachedImage(context.Background(), store.CachedImage{
		Hash:      hash,
		SourceURL: "https://example.test/broken.png",
		Status:    store.CachedUnusable,
	}))

	_, _, err := cache.Open(context.Background(), hash)
	require.ErrorIs(t, err, store.ErrNotFound)

	row, err := rows.CachedImageByHash(context.Background(), hash)
	require.NoError(t, err, "the negative cache entry must survive")
	assert.Equal(t, store.CachedUnusable, row.Status)
}

// TestOpenServesAndKeepsARowWhoseFileIsPresent is the ordinary path, here so
// the self-heal above cannot be "delete the row every time" and still pass.
func TestOpenServesAndKeepsARowWhoseFileIsPresent(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	rows := newMemStore()
	cache := imagesearch.NewCache(dir, rows, nil, discardLogger())

	const hash = "aaaabbbbccccddddaaaabbbbccccddddaaaabbbbccccddddaaaabbbbccccdddd"
	require.NoError(t, rows.PutCachedImage(context.Background(), store.CachedImage{
		Hash: hash, SourceURL: "https://example.test/ok.png",
		ContentType: "image/png", ByteSize: 5, Status: store.CachedOK,
	}))
	require.NoError(t, os.WriteFile(cache.Path(hash), []byte("bytes"), 0o600))

	data, contentType, err := cache.Open(context.Background(), hash)
	require.NoError(t, err)
	assert.Equal(t, []byte("bytes"), data)
	assert.Equal(t, "image/png", contentType)

	_, err = rows.CachedImageByHash(context.Background(), hash)
	assert.NoError(t, err, "a row whose file is present must not be dropped")
}
