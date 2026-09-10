package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// CachedImageStatus is whether a cached candidate has usable bytes on disk.
type CachedImageStatus string

const (
	// CachedOK has a file.
	CachedOK CachedImageStatus = "ok"
	// CachedUnusable is a metadata-only row: the candidate was too large, too
	// many pixels, or would not decode. Keeping the row is the point — it is
	// what stops the same broken URL being downloaded again on every run of
	// the same query (docs/specs/07-shopping-list-reconciliation.md).
	CachedUnusable CachedImageStatus = "unusable"
)

// CachedImage is one row of the suggestion cache.
//
// There is no storage id here, and that is deliberate: the row describes a
// picture on the internet, not who went looking for it. Two households
// searching for the same product share the fetched bytes, and the second one
// costs no provider call.
type CachedImage struct {
	Hash           string
	SourceURL      string
	ContentType    string
	ByteSize       int64
	Width          *int
	Height         *int
	Status         CachedImageStatus
	FetchedAt      time.Time
	LastAccessedAt time.Time
}

// CachedImageByHash reads one row. A miss is ErrNotFound.
func (s *Store) CachedImageByHash(ctx context.Context, hash string) (*CachedImage, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT hash, source_url, content_type, byte_size, width, height, status, fetched_at, last_accessed_at
		  FROM cached_images WHERE hash = $1`, hash)
	return scanCachedImage(row)
}

// PutCachedImage records a fetched candidate, replacing any previous row for
// the same source URL.
//
// ON CONFLICT DO UPDATE rather than DO NOTHING: unlike catalog_products, this
// table is a cache and not a shared description, so re-fetching a URL whose
// file was evicted has to be able to record the new file. Nothing here is
// visible to another household, so there is no channel to abuse.
func (s *Store) PutCachedImage(ctx context.Context, in CachedImage) error {
	if in.Status == "" {
		in.Status = CachedOK
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO cached_images (hash, source_url, content_type, byte_size, width, height, status,
		                           fetched_at, last_accessed_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, now(), now())
		ON CONFLICT (hash) DO UPDATE
		   SET source_url = EXCLUDED.source_url,
		       content_type = EXCLUDED.content_type,
		       byte_size = EXCLUDED.byte_size,
		       width = EXCLUDED.width,
		       height = EXCLUDED.height,
		       status = EXCLUDED.status,
		       fetched_at = now(),
		       last_accessed_at = now()`,
		in.Hash, in.SourceURL, in.ContentType, in.ByteSize, in.Width, in.Height, string(in.Status))
	if err != nil {
		return fmt.Errorf("store: put cached image: %w", err)
	}
	return nil
}

// TouchCachedImage refreshes recency, but only when the stored value is older
// than staleAfter.
//
// The throttle is what turns a write-per-request into at most one write per
// image per hour. A 1GB LRU does not need minute-resolution recency, and the
// serving path should not pay for a database write on every <img> load
// (docs/specs/07-shopping-list-reconciliation.md).
//
// Recency lives here rather than in the filesystem because atime is not
// usable: most systems mount with relatime or noatime, Docker volumes on a NAS
// routinely do, and a touch-based scheme would silently degrade to whatever
// the mount options allow.
func (s *Store) TouchCachedImage(ctx context.Context, hash string, staleAfter time.Duration) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE cached_images
		   SET last_accessed_at = now()
		 WHERE hash = $1
		   AND last_accessed_at < now() - $2::interval`,
		hash, staleAfter.String())
	if err != nil {
		return fmt.Errorf("store: touch cached image: %w", err)
	}
	return nil
}

// CachedImageBytes returns the total size of usable cached files.
//
// Unusable rows hold no file, so they are excluded: counting them would evict
// real images to make room for metadata.
func (s *Store) CachedImageBytes(ctx context.Context) (int64, error) {
	var total int64
	err := s.pool.QueryRow(ctx,
		`SELECT coalesce(sum(byte_size), 0) FROM cached_images WHERE status = 'ok'`).Scan(&total)
	if err != nil {
		return 0, fmt.Errorf("store: cached image bytes: %w", err)
	}
	return total, nil
}

// LeastRecentlyUsedImages returns usable rows oldest-access-first.
func (s *Store) LeastRecentlyUsedImages(ctx context.Context, limit int) ([]CachedImage, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT hash, source_url, content_type, byte_size, width, height, status, fetched_at, last_accessed_at
		  FROM cached_images
		 WHERE status = 'ok'
		 ORDER BY last_accessed_at, hash
		 LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("store: lru cached images: %w", err)
	}
	defer rows.Close()

	var out []CachedImage
	for rows.Next() {
		image, err := scanCachedImage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *image)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: lru cached images: %w", err)
	}
	return out, nil
}

// DeleteCachedImage removes one row.
//
// Eviction deletes the row first and unlinks the file afterwards. A crash
// between the two leaves an orphan file rather than a row pointing at nothing:
// an orphan wastes disk until the next sweep, while a dangling reference would
// serve a 404 for an image the cache believes it has.
func (s *Store) DeleteCachedImage(ctx context.Context, hash string) error {
	if _, err := s.pool.Exec(ctx, `DELETE FROM cached_images WHERE hash = $1`, hash); err != nil {
		return fmt.Errorf("store: delete cached image: %w", err)
	}
	return nil
}

// CachedImageHashes returns every hash the table knows, for the orphan sweep.
func (s *Store) CachedImageHashes(ctx context.Context) (map[string]struct{}, error) {
	rows, err := s.pool.Query(ctx, `SELECT hash FROM cached_images`)
	if err != nil {
		return nil, fmt.Errorf("store: cached image hashes: %w", err)
	}
	defer rows.Close()

	out := map[string]struct{}{}
	for rows.Next() {
		var hash string
		if err := rows.Scan(&hash); err != nil {
			return nil, fmt.Errorf("store: scan cached image hash: %w", err)
		}
		out[hash] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: cached image hashes: %w", err)
	}
	return out, nil
}

func scanCachedImage(row rowScanner) (*CachedImage, error) {
	var c CachedImage
	var status string
	err := row.Scan(&c.Hash, &c.SourceURL, &c.ContentType, &c.ByteSize,
		&c.Width, &c.Height, &status, &c.FetchedAt, &c.LastAccessedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: scan cached image: %w", err)
	}
	c.Status = CachedImageStatus(status)
	return &c, nil
}
