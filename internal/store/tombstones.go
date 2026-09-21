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

// TombstoneRetention is how long a deletion stays visible to a delta sync.
//
// It is the one number the resync boundary is computed from
// (DeltaIsResumable in delta.go): a client whose updated_since predates this
// window cannot be told about every deletion since, because the sweep has
// already removed some, so it must discard its cache and fetch everything
// (docs/specs/12-client-api-contract.md). Both the sweep and the boundary read
// this constant, so they cannot drift apart.
const TombstoneRetention = 30 * 24 * time.Hour

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

// Deletions are read back through the delta loaders in delta.go
// (Store.deletedSince), which scope the query to one entity kind because a
// delta request only ever concerns one list endpoint. There is deliberately no
// exported "all tombstones since" reader: nothing needs the mixed set, and one
// would invite a caller to filter in Go what the index can filter in SQL.

// SweepTombstones deletes rows past the retention window. Returns how many
// went.
func (s *Store) SweepTombstones(ctx context.Context, now time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM tombstones WHERE deleted_at < $1`, now.Add(-TombstoneRetention))
	if err != nil {
		return 0, fmt.Errorf("store: sweep tombstones: %w", err)
	}
	return tag.RowsAffected(), nil
}
