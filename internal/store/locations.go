package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Location is one node of a storage's location tree.
type Location struct {
	ID          uuid.UUID
	StorageID   uuid.UUID
	ParentID    *uuid.UUID
	Name        string
	Description *string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// NewLocation is the input to CreateLocation. StorageID is not a field: it
// comes from the validated request context, never from a request body, so a
// caller cannot insert into a storage they do not belong to.
type NewLocation struct {
	ParentID    *uuid.UUID
	Name        string
	Description *string
}

// CreateLocation inserts a node, rejecting a parent that belongs to another
// storage with ErrNotFound.
//
// The new row's storage_id is taken from storageID, never from the input, so
// there is no request field that could place a node in a foreign storage.
func (s *Store) CreateLocation(ctx context.Context, storageID uuid.UUID, in NewLocation) (*Location, error) {
	id, err := newID()
	if err != nil {
		return nil, err
	}

	var out *Location
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		if err := lockStorageTree(ctx, tx, storageID); err != nil {
			return err
		}
		if err := resolveParent(ctx, tx, treeLocations, storageID, nil, in.ParentID); err != nil {
			return err
		}

		row := tx.QueryRow(ctx, `
			INSERT INTO locations (id, storage_id, parent_id, name, description)
			VALUES ($1, $2, $3, $4, $5)
			RETURNING id, storage_id, parent_id, name, description, created_at, updated_at`,
			id, storageID, in.ParentID, in.Name, in.Description)

		loc, err := scanLocation(row)
		if err != nil {
			return err
		}
		out = loc
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// MoveLocation re-parents a node.
//
// Both ends are validated against storageID, which is what makes a
// cross-storage move impossible by construction rather than by a check someone
// has to remember: a parent in another storage is ErrNotFound, and so is a
// node in another storage.
func (s *Store) MoveLocation(ctx context.Context, storageID, id uuid.UUID, parentID *uuid.UUID) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		if err := lockStorageTree(ctx, tx, storageID); err != nil {
			return err
		}
		if err := requireSameStorage(ctx, tx, treeLocations, storageID, id); err != nil {
			return err
		}
		if err := resolveParent(ctx, tx, treeLocations, storageID, &id, parentID); err != nil {
			return err
		}

		_, err := tx.Exec(ctx, `
			UPDATE locations SET parent_id = $1, updated_at = now()
			 WHERE id = $2 AND storage_id = $3`,
			parentID, id, storageID)
		if err != nil {
			return fmt.Errorf("store: move location: %w", err)
		}
		return nil
	})
}

// RenameLocation updates the display fields, touching updated_at so a client
// delta picks the change up.
func (s *Store) RenameLocation(ctx context.Context, storageID, id uuid.UUID, name string, description *string) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE locations SET name = $1, description = $2, updated_at = now()
		 WHERE id = $3 AND storage_id = $4`,
		name, description, id, storageID)
	if err != nil {
		return fmt.Errorf("store: rename location: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteLocation removes a node and its descendants.
//
// It refuses with ErrConflict while the node *or any descendant* still holds
// inventory: the rows would be taken out by the location cascade, orphaning
// stock that physically exists. The user has to move or clear it first.
//
// A tombstone is written for every removed node, in the same transaction, so a
// client that cached the subtree learns the whole thing is gone rather than
// only its root.
func (s *Store) DeleteLocation(ctx context.Context, storageID, id uuid.UUID) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		if err := lockStorageTree(ctx, tx, storageID); err != nil {
			return err
		}
		if err := requireSameStorage(ctx, tx, treeLocations, storageID, id); err != nil {
			return err
		}

		subtree, err := locationSubtree(ctx, tx, id)
		if err != nil {
			return err
		}

		held, err := scanCount(ctx, tx, `
			SELECT count(*) FROM inventory_batches WHERE location_id = ANY($1)`, subtree)
		if err != nil {
			return fmt.Errorf("store: check inventory under location: %w", err)
		}
		if held > 0 {
			return fmt.Errorf("%w: location still holds %d inventory batch(es)", ErrConflict, held)
		}

		if _, err := tx.Exec(ctx, `DELETE FROM locations WHERE id = $1 AND storage_id = $2`, id, storageID); err != nil {
			return fmt.Errorf("store: delete location: %w", err)
		}

		return recordTombstones(ctx, tx, storageID, TombstoneLocation, subtree)
	})
}

// LocationTree returns every node of one storage, ordered so parents precede
// children. The query is filtered by storage_id, so no other storage's node
// can appear at any depth.
func (s *Store) LocationTree(ctx context.Context, storageID uuid.UUID) ([]Location, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, storage_id, parent_id, name, description, created_at, updated_at
		  FROM locations
		 WHERE storage_id = $1
		 ORDER BY created_at, id`, storageID)
	if err != nil {
		return nil, fmt.Errorf("store: list locations: %w", err)
	}
	defer rows.Close()

	var out []Location
	for rows.Next() {
		loc, err := scanLocation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *loc)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list locations: %w", err)
	}
	return out, nil
}

// locationSubtree returns the id of root and of every node beneath it.
func locationSubtree(ctx context.Context, q querier, root uuid.UUID) ([]uuid.UUID, error) {
	rows, err := q.Query(ctx, fmt.Sprintf(`
		WITH RECURSIVE subtree AS (
		    SELECT id, 1 AS depth FROM locations WHERE id = $1
		    UNION ALL
		    SELECT l.id, s.depth + 1
		      FROM locations l
		      JOIN subtree s ON l.parent_id = s.id
		     WHERE s.depth < %d
		)
		SELECT id FROM subtree`, maxTreeDepth), root)
	if err != nil {
		return nil, fmt.Errorf("store: collect location subtree: %w", err)
	}
	defer rows.Close()

	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store: collect location subtree: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: collect location subtree: %w", err)
	}
	return ids, nil
}

// rowScanner is satisfied by both pgx.Row and pgx.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanLocation(row rowScanner) (*Location, error) {
	var loc Location
	err := row.Scan(&loc.ID, &loc.StorageID, &loc.ParentID, &loc.Name, &loc.Description, &loc.CreatedAt, &loc.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: scan location: %w", err)
	}
	return &loc, nil
}
