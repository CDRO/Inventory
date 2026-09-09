package store

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// EntityType names a client-cacheable entity kind. The set matches the CHECK
// on tombstones.entity_type.
type EntityType string

const (
	TombstoneProduct          EntityType = "product"
	TombstoneCategory         EntityType = "category"
	TombstoneLocation         EntityType = "location"
	TombstoneShoppingList     EntityType = "shopping_list"
	TombstoneShoppingListItem EntityType = "shopping_list_item"
)

// TombstoneRetention is how long a deletion stays visible to a delta sync. A
// client whose updated_since predates the oldest surviving tombstone cannot be
// brought up to date safely and must resync
// (docs/specs/12-client-api-contract.md).
const TombstoneRetention = 30 * 24 * time.Hour

// Tombstone records that a row was deleted.
type Tombstone struct {
	ID         uuid.UUID
	StorageID  uuid.UUID
	EntityType EntityType
	EntityID   uuid.UUID
	DeletedAt  time.Time
}

// recordTombstones writes one row per deleted entity.
//
// It takes a querier rather than the pool because it must run inside the same
// transaction as the delete it describes: a tombstone committed separately
// could be lost while the delete survives, leaving the row gone from the
// server and present in every client's cache forever.
func recordTombstones(ctx context.Context, q querier, storageID uuid.UUID, kind EntityType, entityIDs []uuid.UUID) error {
	for _, entityID := range entityIDs {
		id, err := newID()
		if err != nil {
			return err
		}
		if _, err := q.Exec(ctx, `
			INSERT INTO tombstones (id, storage_id, entity_type, entity_id)
			VALUES ($1, $2, $3, $4)`,
			id, storageID, string(kind), entityID); err != nil {
			return fmt.Errorf("store: record %s tombstone: %w", kind, err)
		}
	}
	return nil
}

// TombstonesSince returns deletions in this storage after the given instant,
// for a client catching up.
func (s *Store) TombstonesSince(ctx context.Context, storageID uuid.UUID, since time.Time) ([]Tombstone, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, storage_id, entity_type, entity_id, deleted_at
		  FROM tombstones
		 WHERE storage_id = $1 AND deleted_at > $2
		 ORDER BY deleted_at, id`, storageID, since)
	if err != nil {
		return nil, fmt.Errorf("store: list tombstones: %w", err)
	}
	defer rows.Close()

	var out []Tombstone
	for rows.Next() {
		var t Tombstone
		var kind string
		if err := rows.Scan(&t.ID, &t.StorageID, &kind, &t.EntityID, &t.DeletedAt); err != nil {
			return nil, fmt.Errorf("store: scan tombstone: %w", err)
		}
		t.EntityType = EntityType(kind)
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list tombstones: %w", err)
	}
	return out, nil
}

// DeltaIsResumable reports whether a client asking for changes since the given
// instant can be brought up to date from the surviving tombstones.
//
// If the client's cursor predates the oldest tombstone still retained, some
// deletion has already been swept and answering the delta would silently leave
// a deleted row in that client's cache. The honest answer is "resync".
func (s *Store) DeltaIsResumable(ctx context.Context, storageID uuid.UUID, since time.Time) (bool, error) {
	var oldest *time.Time
	err := s.pool.QueryRow(ctx, `
		SELECT min(deleted_at) FROM tombstones WHERE storage_id = $1`, storageID).Scan(&oldest)
	if err != nil {
		return false, fmt.Errorf("store: oldest tombstone: %w", err)
	}
	if oldest == nil {
		// No deletions recorded, so nothing can have been swept.
		return true, nil
	}
	return !since.Before(*oldest), nil
}

// SweepTombstones deletes rows past the retention window. Returns how many
// went.
func (s *Store) SweepTombstones(ctx context.Context, now time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM tombstones WHERE deleted_at < $1`, now.Add(-TombstoneRetention))
	if err != nil {
		return 0, fmt.Errorf("store: sweep tombstones: %w", err)
	}
	return tag.RowsAffected(), nil
}
