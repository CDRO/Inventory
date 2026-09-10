package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CDRO/Inventory/internal/store"
)

// The recency throttle is the one part of the cache that is pure SQL, and the
// only tests that touched it before this file used a hand-rolled fake that
// counted calls without ever running the statement. That is exactly the shape
// of gap where an inverted comparison survives: every fake-based test stays
// green while production either writes on every image view or never refreshes
// recency at all — and the second failure mode is silent, because it only
// shows up later as LRU eviction throwing away the wrong files.

func putCachedImage(t *testing.T, ctx context.Context, s *store.Store, hash string, size int64) {
	t.Helper()

	require.NoError(t, s.PutCachedImage(ctx, store.CachedImage{
		Hash: hash, SourceURL: "https://example.test/" + hash,
		ContentType: "image/jpeg", ByteSize: size, Status: store.CachedOK,
	}))
}

// lastAccessed reads the stored recency stamp directly, rather than through the
// store's own reader, so a broken reader cannot mask a broken writer.
func lastAccessed(t *testing.T, ctx context.Context, hash string) time.Time {
	t.Helper()

	var at time.Time
	require.NoError(t, testPool.QueryRow(ctx,
		`SELECT last_accessed_at FROM cached_images WHERE hash = $1`, hash).Scan(&at))
	return at
}

// TestTouchIsSuppressedWhileTheStampIsFresh is the throttle itself: at most one
// write per image per hour, so serving an image does not cost a database write
// every time somebody's browser loads a card.
func TestTouchIsSuppressedWhileTheStampIsFresh(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()

	const hash = "fresh0000000000000000000000000000000000000000000000000000000001"
	putCachedImage(t, ctx, s, hash, 1024)

	before := lastAccessed(t, ctx, hash)

	// The row was just written, so its stamp is far newer than an hour.
	require.NoError(t, s.TouchCachedImage(ctx, hash, time.Hour))

	assert.Equal(t, before, lastAccessed(t, ctx, hash),
		"a stamp newer than the staleness window must not be rewritten")
}

// TestTouchRefreshesOnceTheStampIsStale is the other half. Without it, recency
// never moves and the LRU sweep evicts by insertion order instead of by use.
func TestTouchRefreshesOnceTheStampIsStale(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()

	const hash = "stale0000000000000000000000000000000000000000000000000000000002"
	putCachedImage(t, ctx, s, hash, 1024)

	// Age the row past the window, in the database rather than by sleeping.
	_, err := testPool.Exec(ctx,
		`UPDATE cached_images SET last_accessed_at = now() - interval '3 hours' WHERE hash = $1`, hash)
	require.NoError(t, err)

	before := lastAccessed(t, ctx, hash)
	require.NoError(t, s.TouchCachedImage(ctx, hash, time.Hour))

	assert.True(t, lastAccessed(t, ctx, hash).After(before),
		"a stamp older than the window must be refreshed, or eviction stops tracking use")
}

// TestTouchWindowIsTheDurationItIsGiven pins the conversion from a Go duration
// to a Postgres interval. staleAfter.String() produces forms like "1h0m0s" and
// "2m0s"; if that ever stopped parsing the way it does, the throttle would
// silently take on a completely different window.
func TestTouchWindowIsTheDurationItIsGiven(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()

	const hash = "window000000000000000000000000000000000000000000000000000000003"
	putCachedImage(t, ctx, s, hash, 1024)

	_, err := testPool.Exec(ctx,
		`UPDATE cached_images SET last_accessed_at = now() - interval '5 minutes' WHERE hash = $1`, hash)
	require.NoError(t, err)
	before := lastAccessed(t, ctx, hash)

	// Five minutes old, asked to refresh anything older than an hour: no write.
	require.NoError(t, s.TouchCachedImage(ctx, hash, time.Hour))
	assert.Equal(t, before, lastAccessed(t, ctx, hash))

	// Same row, asked to refresh anything older than a minute: written.
	require.NoError(t, s.TouchCachedImage(ctx, hash, time.Minute))
	assert.True(t, lastAccessed(t, ctx, hash).After(before),
		"the window actually follows the duration passed in")
}

func TestTouchingAnUnknownHashIsNotAnError(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()

	// Recency bookkeeping runs after the response, on a hash that may have been
	// evicted in between. Failing there would log noise for something that is
	// already handled: the file is gone and will be refetched if needed.
	err := s.TouchCachedImage(ctx,
		"missing00000000000000000000000000000000000000000000000000000004", time.Hour)

	assert.NoError(t, err)
}

// TestUnusableRowsHoldNoBudget — an unusable candidate is metadata only, so
// counting it would evict real images to make room for the memory of a broken
// one.
func TestUnusableRowsHoldNoBudget(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()

	const okHash = "budget000000000000000000000000000000000000000000000000000000005"
	const badHash = "budget000000000000000000000000000000000000000000000000000000006"

	putCachedImage(t, ctx, s, okHash, 4096)
	require.NoError(t, s.PutCachedImage(ctx, store.CachedImage{
		Hash: badHash, SourceURL: "https://example.test/bad", ContentType: "",
		ByteSize: 0, Status: store.CachedUnusable,
	}))

	total, err := s.CachedImageBytes(ctx)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, total, int64(4096))

	lru, err := s.LeastRecentlyUsedImages(ctx, 100)
	require.NoError(t, err)
	for _, row := range lru {
		assert.NotEqual(t, badHash, row.Hash,
			"an unusable row has no file to evict and must never be offered as a candidate")
	}
}

// TestPutReplacesARowForTheSameHash — a refetch after eviction has to be able
// to record the new file. Unlike catalog_products this table is a cache, not a
// shared description, so there is no abuse channel in letting it be rewritten.
func TestPutReplacesARowForTheSameHash(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()

	const hash = "replace00000000000000000000000000000000000000000000000000000007"
	putCachedImage(t, ctx, s, hash, 1000)
	putCachedImage(t, ctx, s, hash, 2000)

	row, err := s.CachedImageByHash(ctx, hash)
	require.NoError(t, err)
	assert.Equal(t, int64(2000), row.ByteSize)

	assert.Equal(t, 1, countRows(t, ctx,
		`SELECT count(*) FROM cached_images WHERE hash = $1`, hash))
}
