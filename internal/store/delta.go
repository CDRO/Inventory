package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// ErrResyncRequired means a client's cursor predates the tombstone retention
// window, so the changes since it cannot be described completely.
//
// It is its own error rather than an ErrValidation because the caller did
// nothing wrong: the request is well formed, the server simply no longer
// remembers far enough back. The honest answer is "throw your cache away and
// fetch everything", which is a different instruction from "fix your request"
// (docs/specs/12-client-api-contract.md).
var ErrResyncRequired = errors.New("store: delta cursor predates tombstone retention")

// Delta is one entity kind's changes since a client's cursor: the rows that
// changed, the ids that went away, and the instant to send back next time.
//
// Deleted carries ids rather than rows because there is nothing left to
// describe — the row is gone, and an id is all a client needs to drop it from
// its cache. Ids are never reused (UUIDv7), so an id in Deleted can only ever
// mean "this one, permanently".
type Delta[T any] struct {
	Changed  []T
	Deleted  []uuid.UUID
	SyncedAt time.Time
}

// DeltaIsResumable reports whether a client whose cursor is `since` can be
// brought up to date, given the server's current time.
//
// **The boundary is the retention window, not the oldest surviving
// tombstone.** That distinction is the whole point of this function. A rule
// built on `min(deleted_at)` looks equivalent and is not: once the sweep has
// removed the only tombstone a storage ever had, `min(deleted_at)` is NULL and
// such a rule answers "resumable" to a cursor from a year ago — handing back a
// delta that silently omits the deletion and leaving that row in the client's
// cache forever. That is exactly the failure spec 12 says to make the server
// detect. Retention is also the less conservative rule of the two: a storage
// whose oldest surviving tombstone is 25 days old can still answer a 28-day-old
// cursor correctly, because everything swept was older than the window.
//
// It is a pure function of two instants so the rule can be tested directly and
// so callers that already hold the database's clock do not need a second round
// trip to ask it.
func DeltaIsResumable(since, now time.Time) bool {
	return !since.Before(now.Add(-TombstoneRetention))
}

// ProductsChangedSince answers a delta request for this storage's products.
func (s *Store) ProductsChangedSince(ctx context.Context, storageID uuid.UUID, since time.Time) (*Delta[Product], error) {
	return loadDelta(ctx, s, storageID, TombstoneProduct, since, func(ctx context.Context, at time.Time) ([]Product, error) {
		rows, err := s.pool.Query(ctx, `
			SELECT id, storage_id, name, category_id, catalog_id, item_type,
			       default_shelf_life_days, min_stock, image_url, icon_name, created_at, updated_at
			  FROM products
			 WHERE storage_id = $1 AND updated_at > $2
			 ORDER BY name`, storageID, at)
		if err != nil {
			return nil, fmt.Errorf("store: products changed since: %w", err)
		}
		defer rows.Close()

		out := []Product{}
		for rows.Next() {
			p, err := scanProduct(rows)
			if err != nil {
				return nil, err
			}
			out = append(out, *p)
		}
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("store: products changed since: %w", err)
		}
		return out, nil
	})
}

// CategoriesChangedSince answers a delta request for this storage's categories.
func (s *Store) CategoriesChangedSince(ctx context.Context, storageID uuid.UUID, since time.Time) (*Delta[Category], error) {
	return loadDelta(ctx, s, storageID, TombstoneCategory, since, func(ctx context.Context, at time.Time) ([]Category, error) {
		rows, err := s.pool.Query(ctx, `
			SELECT id, storage_id, parent_id, name, default_shelf_life_days, created_at, updated_at
			  FROM categories
			 WHERE storage_id = $1 AND updated_at > $2
			 ORDER BY created_at, id`, storageID, at)
		if err != nil {
			return nil, fmt.Errorf("store: categories changed since: %w", err)
		}
		defer rows.Close()

		out := []Category{}
		for rows.Next() {
			cat, err := scanCategory(rows)
			if err != nil {
				return nil, err
			}
			out = append(out, *cat)
		}
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("store: categories changed since: %w", err)
		}
		return out, nil
	})
}

// LocationsChangedSince answers a delta request for this storage's locations.
func (s *Store) LocationsChangedSince(ctx context.Context, storageID uuid.UUID, since time.Time) (*Delta[Location], error) {
	return loadDelta(ctx, s, storageID, TombstoneLocation, since, func(ctx context.Context, at time.Time) ([]Location, error) {
		rows, err := s.pool.Query(ctx, `
			SELECT id, storage_id, parent_id, name, description, created_at, updated_at
			  FROM locations
			 WHERE storage_id = $1 AND updated_at > $2
			 ORDER BY created_at, id`, storageID, at)
		if err != nil {
			return nil, fmt.Errorf("store: locations changed since: %w", err)
		}
		defer rows.Close()

		out := []Location{}
		for rows.Next() {
			loc, err := scanLocation(rows)
			if err != nil {
				return nil, err
			}
			out = append(out, *loc)
		}
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("store: locations changed since: %w", err)
		}
		return out, nil
	})
}

// loadDelta assembles one entity kind's delta: the resync check, the changed
// rows, and the tombstones for the same window.
//
// **SyncedAt is read before either query, never after.** A client sends it
// back as its next cursor, so anything that happens after it is read must
// still be caught by the following request. Reading it afterwards would leave
// a window — a write landing between the row query and the clock read would be
// reported by neither request, and the client would keep a stale row with
// nothing anywhere to notice. Reading it first can only make a row appear in
// two consecutive deltas, which is a duplicate the client overwrites with the
// same value. Delta sync here is at-least-once by construction, and that is the
// direction the error has to fall.
//
// The three queries are deliberately not wrapped in a transaction. With
// SyncedAt read first, a deletion landing between the changed-rows query and
// the tombstone query is simply reported by the next request; a snapshot would
// buy nothing a client can observe and would hold a transaction open across
// the whole response.
func loadDelta[T any](
	ctx context.Context,
	s *Store,
	storageID uuid.UUID,
	kind EntityType,
	since time.Time,
	changed func(ctx context.Context, since time.Time) ([]T, error),
) (*Delta[T], error) {
	var syncedAt time.Time
	if err := s.pool.QueryRow(ctx, `SELECT now()`).Scan(&syncedAt); err != nil {
		return nil, fmt.Errorf("store: delta sync point: %w", err)
	}
	if !DeltaIsResumable(since, syncedAt) {
		return nil, ErrResyncRequired
	}

	rows, err := changed(ctx, since)
	if err != nil {
		return nil, err
	}

	deleted, err := s.deletedSince(ctx, storageID, kind, since)
	if err != nil {
		return nil, err
	}

	return &Delta[T]{Changed: rows, Deleted: deleted, SyncedAt: syncedAt}, nil
}

// deletedSince returns the ids of one kind of entity deleted in this storage
// after the given instant.
//
// Deleting a parent tombstones every node beneath it (recordTombstones is
// called with the whole subtree), so a client that only ever saw the parent
// still learns that each descendant is gone. That is what lets a client hold
// these trees as a flat keyed cache rather than having to work out the
// cascade itself.
func (s *Store) deletedSince(ctx context.Context, storageID uuid.UUID, kind EntityType, since time.Time) ([]uuid.UUID, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT entity_id
		  FROM tombstones
		 WHERE storage_id = $1 AND entity_type = $2 AND deleted_at > $3
		 ORDER BY deleted_at, id`, storageID, string(kind), since)
	if err != nil {
		return nil, fmt.Errorf("store: list %s tombstones: %w", kind, err)
	}
	defer rows.Close()

	out := []uuid.UUID{}
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store: scan %s tombstone: %w", kind, err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list %s tombstones: %w", kind, err)
	}
	return out, nil
}
