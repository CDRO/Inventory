package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Category is one node of a storage's category tree. Same shape and same
// application-level invariants as Location.
type Category struct {
	ID                   uuid.UUID
	StorageID            uuid.UUID
	ParentID             *uuid.UUID
	Name                 string
	DefaultShelfLifeDays *int
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

// NewCategory is the input to CreateCategory.
type NewCategory struct {
	ParentID             *uuid.UUID
	Name                 string
	DefaultShelfLifeDays *int
}

// CreateCategory inserts a node, rejecting a parent from another storage with
// ErrNotFound.
func (s *Store) CreateCategory(ctx context.Context, storageID uuid.UUID, in NewCategory) (*Category, error) {
	id, err := newID()
	if err != nil {
		return nil, err
	}

	var out *Category
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		if err := resolveParent(ctx, tx, treeCategories, storageID, nil, in.ParentID); err != nil {
			return err
		}

		row := tx.QueryRow(ctx, `
			INSERT INTO categories (id, storage_id, parent_id, name, default_shelf_life_days)
			VALUES ($1, $2, $3, $4, $5)
			RETURNING id, storage_id, parent_id, name, default_shelf_life_days, created_at, updated_at`,
			id, storageID, in.ParentID, in.Name, in.DefaultShelfLifeDays)

		cat, err := scanCategory(row)
		if err != nil {
			return err
		}
		out = cat
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// MoveCategory re-parents a node, with the same same-storage and cycle rules
// as MoveLocation.
func (s *Store) MoveCategory(ctx context.Context, storageID, id uuid.UUID, parentID *uuid.UUID) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		if err := requireSameStorage(ctx, tx, treeCategories, storageID, id); err != nil {
			return err
		}
		if err := resolveParent(ctx, tx, treeCategories, storageID, &id, parentID); err != nil {
			return err
		}

		_, err := tx.Exec(ctx, `
			UPDATE categories SET parent_id = $1, updated_at = now()
			 WHERE id = $2 AND storage_id = $3`,
			parentID, id, storageID)
		if err != nil {
			return fmt.Errorf("store: move category: %w", err)
		}
		return nil
	})
}

// DeleteCategory removes a node and its descendants.
//
// It refuses with ErrConflict while any node in the subtree is still
// referenced by a product — the same policy as a location that still holds
// inventory. products.category_id is ON DELETE RESTRICT, so the database would
// refuse the cascade anyway; checking first turns a foreign-key violation into
// an answer that names the number of products in the way.
func (s *Store) DeleteCategory(ctx context.Context, storageID, id uuid.UUID) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		if err := requireSameStorage(ctx, tx, treeCategories, storageID, id); err != nil {
			return err
		}

		subtree, err := categorySubtree(ctx, tx, id)
		if err != nil {
			return err
		}

		used, err := scanCount(ctx, tx, `
			SELECT count(*) FROM products WHERE category_id = ANY($1)`, subtree)
		if err != nil {
			return fmt.Errorf("store: check products in category: %w", err)
		}
		if used > 0 {
			return fmt.Errorf("%w: category is still used by %d product(s)", ErrConflict, used)
		}

		if _, err := tx.Exec(ctx, `DELETE FROM categories WHERE id = $1 AND storage_id = $2`, id, storageID); err != nil {
			return fmt.Errorf("store: delete category: %w", err)
		}

		return recordTombstones(ctx, tx, storageID, TombstoneCategory, subtree)
	})
}

// SetCategoryShelfLife updates the shelf-life rule.
//
// The recompute of derived expiration dates that this change triggers belongs
// to docs/specs/08-expiration-and-classification.md and is not done here; this
// is the data-model write only.
func (s *Store) SetCategoryShelfLife(ctx context.Context, storageID, id uuid.UUID, days *int) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE categories SET default_shelf_life_days = $1, updated_at = now()
		 WHERE id = $2 AND storage_id = $3`, days, id, storageID)
	if err != nil {
		return fmt.Errorf("store: set category shelf life: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// CategoryTree returns every node of one storage.
func (s *Store) CategoryTree(ctx context.Context, storageID uuid.UUID) ([]Category, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, storage_id, parent_id, name, default_shelf_life_days, created_at, updated_at
		  FROM categories
		 WHERE storage_id = $1
		 ORDER BY created_at, id`, storageID)
	if err != nil {
		return nil, fmt.Errorf("store: list categories: %w", err)
	}
	defer rows.Close()

	var out []Category
	for rows.Next() {
		cat, err := scanCategory(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *cat)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list categories: %w", err)
	}
	return out, nil
}

func categorySubtree(ctx context.Context, q querier, root uuid.UUID) ([]uuid.UUID, error) {
	rows, err := q.Query(ctx, fmt.Sprintf(`
		WITH RECURSIVE subtree AS (
		    SELECT id, 1 AS depth FROM categories WHERE id = $1
		    UNION ALL
		    SELECT c.id, s.depth + 1
		      FROM categories c
		      JOIN subtree s ON c.parent_id = s.id
		     WHERE s.depth < %d
		)
		SELECT id FROM subtree`, maxTreeDepth), root)
	if err != nil {
		return nil, fmt.Errorf("store: collect category subtree: %w", err)
	}
	defer rows.Close()

	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store: collect category subtree: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: collect category subtree: %w", err)
	}
	return ids, nil
}

func scanCategory(row rowScanner) (*Category, error) {
	var cat Category
	err := row.Scan(&cat.ID, &cat.StorageID, &cat.ParentID, &cat.Name, &cat.DefaultShelfLifeDays, &cat.CreatedAt, &cat.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: scan category: %w", err)
	}
	return &cat, nil
}
