package imagesearch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/CDRO/Inventory/internal/store"
)

// Cache budget and bookkeeping intervals
// (docs/specs/07-shopping-list-reconciliation.md).
const (
	// MaxCacheBytes is the suggestion cache's ceiling.
	MaxCacheBytes int64 = 1 << 30 // 1GB

	// EvictTargetRatio is the low-water mark eviction runs down to. Evicting
	// to exactly the cap would re-trigger a sweep on the very next write; a
	// gap means eviction is occasional rather than constant.
	EvictTargetRatio = 0.9

	// TouchStaleAfter is how old a recency stamp must be before the serving
	// path bothers to refresh it. This is what makes recency tracking cost at
	// most one write per image per hour instead of one per request.
	TouchStaleAfter = time.Hour

	// SweepInterval is how often the cap and the orphan sweep run on their
	// own, independently of any write.
	SweepInterval = time.Hour
)

// CacheStore is the slice of the database the cache needs.
type CacheStore interface {
	CachedImageByHash(ctx context.Context, hash string) (*store.CachedImage, error)
	PutCachedImage(ctx context.Context, in store.CachedImage) error
	TouchCachedImage(ctx context.Context, hash string, staleAfter time.Duration) error
	CachedImageBytes(ctx context.Context) (int64, error)
	LeastRecentlyUsedImages(ctx context.Context, limit int) ([]store.CachedImage, error)
	DeleteCachedImage(ctx context.Context, hash string) error
	CachedImageHashes(ctx context.Context) (map[string]struct{}, error)
}

// Cache stores normalized suggestion images on disk, with their metadata in
// the database.
//
// The split is deliberate: bytes belong on a filesystem, and recency belongs
// somewhere that survives a mount option. See store.TouchCachedImage for why
// atime cannot be used.
type Cache struct {
	dir      string
	store    CacheStore
	client   *http.Client
	log      *slog.Logger
	maxBytes int64
}

// NewCache returns a cache rooted at dir.
func NewCache(dir string, s CacheStore, client *http.Client, log *slog.Logger) *Cache {
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	if log == nil {
		log = slog.Default()
	}
	return &Cache{dir: dir, store: s, client: client, log: log, maxBytes: MaxCacheBytes}
}

// HashURL is the cache key: the SHA-256 of the source URL.
//
// Keying on the URL rather than on the bytes means a second query that turns up
// the same candidate re-serves the stored file instead of spending another
// provider call — which is the entire reason the cache exists.
func HashURL(sourceURL string) string {
	sum := sha256.Sum256([]byte(sourceURL))
	return hex.EncodeToString(sum[:])
}

// Path is where a hash's bytes live on disk.
func (c *Cache) Path(hash string) string {
	return filepath.Join(c.dir, hash)
}

// ErrUnusableCached is returned by Fetch for a candidate already known to be
// unusable, so a caller can skip it without a network round trip.
var ErrUnusableCached = errors.New("imagesearch: candidate previously found unusable")

// Fetch returns the hash of a usable cached copy of sourceURL, downloading and
// normalizing it if this is the first time it has been seen.
//
// The order matters. A row already marked unusable short-circuits before any
// network access, which is what negative caching is for: without it, a broken
// candidate is re-downloaded on every run of the same query, forever.
func (c *Cache) Fetch(ctx context.Context, sourceURL string) (string, error) {
	hash := HashURL(sourceURL)

	existing, err := c.store.CachedImageByHash(ctx, hash)
	switch {
	case err == nil && existing.Status == store.CachedUnusable:
		return "", ErrUnusableCached
	case err == nil:
		if _, statErr := os.Stat(c.Path(hash)); statErr == nil {
			return hash, nil
		}
		// The row survived but the file did not — an eviction that crashed
		// between the two steps, or a wiped volume. Fall through and refetch
		// rather than serving a 404 for something the cache believes it has.
	case !errors.Is(err, store.ErrNotFound):
		return "", fmt.Errorf("imagesearch: look up cached image: %w", err)
	}

	data, contentType, err := c.download(ctx, sourceURL)
	if err != nil {
		// A transient failure is NOT recorded as unusable: the candidate may
		// be perfectly good and the network merely down, and a permanent
		// "unusable" row would make that outage stick forever.
		return "", err
	}

	normalized, normErr := Normalize(data, contentType)
	if normErr != nil {
		if errors.Is(normErr, ErrUnusable) {
			c.markUnusable(ctx, hash, sourceURL)
			return "", ErrUnusableCached
		}
		return "", normErr
	}

	if err := c.write(ctx, hash, sourceURL, normalized); err != nil {
		return "", err
	}
	return hash, nil
}

// download fetches a candidate under the size cap.
//
// The cap is applied to the stream rather than to a Content-Length header,
// because a header is a claim by the remote server and the body is the thing
// that actually costs memory.
func (c *Cache) download(ctx context.Context, sourceURL string) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, sourceURL, nil)
	if err != nil {
		return nil, "", fmt.Errorf("imagesearch: build request: %w", err)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("imagesearch: fetch candidate: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("imagesearch: candidate returned %d", resp.StatusCode)
	}

	// +1 so that a body exactly at the cap is distinguishable from one over it.
	data, err := io.ReadAll(io.LimitReader(resp.Body, MaxDownloadBytes+1))
	if err != nil {
		return nil, "", fmt.Errorf("imagesearch: read candidate: %w", err)
	}
	if int64(len(data)) > MaxDownloadBytes {
		return nil, "", fmt.Errorf("%w: over %d bytes", ErrUnusable, MaxDownloadBytes)
	}
	return data, resp.Header.Get("Content-Type"), nil
}

// write stores the normalized bytes and their row, then enforces the cap.
func (c *Cache) write(ctx context.Context, hash, sourceURL string, n *Normalized) error {
	if err := os.MkdirAll(c.dir, 0o755); err != nil {
		return fmt.Errorf("imagesearch: create cache dir: %w", err)
	}

	// Write to a temporary name and rename into place, so a reader never sees
	// a half-written file under a hash the database already advertises.
	tmp, err := os.CreateTemp(c.dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("imagesearch: create temp file: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(n.Data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("imagesearch: write cache file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("imagesearch: close cache file: %w", err)
	}
	if err := os.Rename(tmpName, c.Path(hash)); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("imagesearch: place cache file: %w", err)
	}

	row := store.CachedImage{
		Hash: hash, SourceURL: sourceURL, ContentType: n.ContentType,
		ByteSize: int64(len(n.Data)), Status: store.CachedOK,
	}
	if n.Width > 0 {
		row.Width, row.Height = &n.Width, &n.Height
	}
	if err := c.store.PutCachedImage(ctx, row); err != nil {
		// The file is on disk with no row pointing at it. That is the safe
		// direction — the orphan sweep will collect it — so the write is not
		// unwound here.
		return err
	}

	c.enforceCapAfterWrite(ctx)
	return nil
}

// markUnusable records a metadata-only row so the candidate is never fetched
// again.
func (c *Cache) markUnusable(ctx context.Context, hash, sourceURL string) {
	row := store.CachedImage{
		Hash: hash, SourceURL: sourceURL, ContentType: "", ByteSize: 0,
		Status: store.CachedUnusable,
	}
	if err := c.store.PutCachedImage(ctx, row); err != nil {
		// Failing to remember this costs a repeated download later, which is
		// not worth failing the user's suggestion request over.
		c.log.WarnContext(ctx, "imagesearch: could not record unusable candidate",
			slog.String("source_url", sourceURL), slog.Any("err", err))
	}
}

// Open returns the stored bytes and content type for a hash, and refreshes
// recency in the background.
func (c *Cache) Open(ctx context.Context, hash string) ([]byte, string, error) {
	row, err := c.store.CachedImageByHash(ctx, hash)
	if err != nil {
		return nil, "", err
	}
	if row.Status != store.CachedOK {
		return nil, "", store.ErrNotFound
	}

	data, err := os.ReadFile(c.Path(hash))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, "", store.ErrNotFound
		}
		return nil, "", fmt.Errorf("imagesearch: read cached image: %w", err)
	}
	return data, row.ContentType, nil
}

// Touch refreshes a hash's recency without blocking the caller.
//
// It runs on its own context rather than the request's: the request context is
// cancelled the moment the response is written, which would abort the update
// exactly when it is supposed to happen (after the response).
func (c *Cache) Touch(hash string) {
	go func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), 5*time.Second)
		defer cancel()

		if err := c.store.TouchCachedImage(ctx, hash, TouchStaleAfter); err != nil {
			// Losing a recency update means an image looks slightly staler
			// than it is and may be evicted early, which costs one refetch.
			c.log.WarnContext(ctx, "imagesearch: recency update failed",
				slog.String("hash", hash), slog.Any("err", err))
		}
	}()
}

// enforceCapAfterWrite evicts immediately when a write pushed the cache over
// its ceiling, so the budget is a real bound rather than an hourly average.
func (c *Cache) enforceCapAfterWrite(ctx context.Context) {
	total, err := c.store.CachedImageBytes(ctx)
	if err != nil {
		c.log.WarnContext(ctx, "imagesearch: could not measure cache", slog.Any("err", err))
		return
	}
	if total <= c.maxBytes {
		return
	}
	if err := c.Evict(ctx); err != nil {
		c.log.WarnContext(ctx, "imagesearch: eviction failed", slog.Any("err", err))
	}
}

// Evict deletes least-recently-accessed entries until the cache is at or below
// the low-water mark.
//
// Promoted product images are never in this tier — they were copied into
// permanent storage when the user chose them — so choosing an image can never
// be undone by a sweep.
func (c *Cache) Evict(ctx context.Context) error {
	target := int64(float64(c.maxBytes) * EvictTargetRatio)

	total, err := c.store.CachedImageBytes(ctx)
	if err != nil {
		return err
	}

	for total > target {
		batch, err := c.store.LeastRecentlyUsedImages(ctx, 64)
		if err != nil {
			return err
		}
		if len(batch) == 0 {
			return nil
		}

		for _, row := range batch {
			if total <= target {
				break
			}
			// Row first, file second: a crash in between leaves an orphan file
			// rather than a row promising bytes that are gone.
			if err := c.store.DeleteCachedImage(ctx, row.Hash); err != nil {
				return err
			}
			if err := os.Remove(c.Path(row.Hash)); err != nil && !os.IsNotExist(err) {
				c.log.WarnContext(ctx, "imagesearch: could not unlink evicted file",
					slog.String("hash", row.Hash), slog.Any("err", err))
			}
			total -= row.ByteSize
		}
	}
	return nil
}

// SweepOrphans deletes files under the cache directory that no row points at.
//
// This is the other half of "row first, file second": eviction and a crashed
// write both leave files behind, and this is what makes the cache self-healing
// in the safe direction.
func (c *Cache) SweepOrphans(ctx context.Context) error {
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("imagesearch: read cache dir: %w", err)
	}

	known, err := c.store.CachedImageHashes(ctx)
	if err != nil {
		return err
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if _, ok := known[name]; ok {
			continue
		}
		// A concurrent write's temporary file is not an orphan yet; it is
		// about to be renamed into place under its real hash.
		if len(name) > 0 && name[0] == '.' {
			continue
		}
		if err := os.Remove(filepath.Join(c.dir, name)); err != nil && !os.IsNotExist(err) {
			c.log.WarnContext(ctx, "imagesearch: could not remove orphan",
				slog.String("file", name), slog.Any("err", err))
		}
	}
	return nil
}

// RunSweeps runs the cap and orphan sweeps until ctx is cancelled.
//
// It sweeps once at startup as well as hourly, because the state most likely to
// need cleaning is what a crash left behind, and that is visible immediately
// rather than an hour later.
func (c *Cache) RunSweeps(ctx context.Context) {
	sweep := func() {
		if err := c.Evict(ctx); err != nil {
			c.log.WarnContext(ctx, "imagesearch: scheduled eviction failed", slog.Any("err", err))
		}
		if err := c.SweepOrphans(ctx); err != nil {
			c.log.WarnContext(ctx, "imagesearch: orphan sweep failed", slog.Any("err", err))
		}
	}

	sweep()

	ticker := time.NewTicker(SweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sweep()
		}
	}
}
