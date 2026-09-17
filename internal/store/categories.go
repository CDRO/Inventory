package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/CDRO/Inventory/internal/gamification"
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
		if err := lockStorageTree(ctx, tx, storageID); err != nil {
			return err
		}
		cat, err := insertCategory(ctx, tx, storageID, id, in)
		out = cat
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// CreateCategoryAsUser is CreateCategory, additionally recording a
// category_created contribution in the same transaction
// (docs/specs/51-gamification-scoring.md) — the category-tree counterpart of
// CreateLocationAsUser's location_mapped.
func (s *Store) CreateCategoryAsUser(ctx context.Context, storageID uuid.UUID, in NewCategory, userID uuid.UUID) (*Category, error) {
	id, err := newID()
	if err != nil {
		return nil, err
	}

	var out *Category
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		if err := lockStorageTree(ctx, tx, storageID); err != nil {
			return err
		}
		cat, err := insertCategory(ctx, tx, storageID, id, in)
		if err != nil {
			return err
		}
		out = cat
		return recordContribution(ctx, tx, storageID, userID, gamification.KindCategoryCreated, &cat.ID, nil)
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// insertCategory writes one node inside a caller's transaction. The caller
// holds the storage's tree lock.
//
// A new node needs no expiry recompute, whatever shelf life it carries: no
// product can be filed under a category that did not exist until this
// statement.
func insertCategory(ctx context.Context, tx pgx.Tx, storageID, id uuid.UUID, in NewCategory) (*Category, error) {
	if err := resolveParent(ctx, tx, treeCategories, storageID, nil, in.ParentID); err != nil {
		return nil, err
	}
	return scanCategory(tx.QueryRow(ctx, `
		INSERT INTO categories (id, storage_id, parent_id, name, default_shelf_life_days)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id, storage_id, parent_id, name, default_shelf_life_days, created_at, updated_at`,
		id, storageID, in.ParentID, in.Name, in.DefaultShelfLifeDays))
}

// MoveCategory re-parents a node, with the same same-storage and cycle rules
// as MoveLocation.
//
// It is UpdateCategory with only the parent set, rather than a second write
// path, so a move can never skip the expiry recompute UpdateCategory does.
func (s *Store) MoveCategory(ctx context.Context, storageID, id uuid.UUID, parentID *uuid.UUID) error {
	_, err := s.UpdateCategory(ctx, storageID, id, CategoryPatch{ParentID: parentID, SetParentID: true})
	return err
}

// CategoryPatch is a partial update to one node, with the same
// absent-versus-null contract as LocationPatch: a nil ParentID with
// SetParentID makes the node a root, and without it leaves the parent alone.
//
// The shelf-life rule is deliberately not a field. Changing it is a
// correction that reports how many dates it moved
// (docs/specs/08-expiration-and-classification.md), which is what
// SetCategoryShelfLife and its own route are for.
type CategoryPatch struct {
	// Name nil leaves the name unchanged.
	Name *string
	// ParentID is applied only when SetParentID is true.
	ParentID    *uuid.UUID
	SetParentID bool
}

// UpdateCategory renames and/or re-parents one node in a single transaction,
// for the same half-applied-PATCH reason UpdateLocation gives.
//
// A re-parent also recomputes the derived expiry dates of every product in the
// moved subtree, in the same transaction. Those products' category chains now
// climb through different ancestors, so an inherited shelf life may have
// changed under them — the same consequence as re-filing a single product
// (SetProductCategory), and silent for the same reason: the move is performed
// to reorganise the tree, and the dates follow from it. Dates a person set are
// untouched, as everywhere.
//
// A node or parent in another storage is ErrNotFound; a parent inside the
// node's own subtree is ErrConflict.
func (s *Store) UpdateCategory(ctx context.Context, storageID, id uuid.UUID, patch CategoryPatch) (*Category, error) {
	var out *Category
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		if err := lockStorageTree(ctx, tx, storageID); err != nil {
			return err
		}
		if err := requireSameStorage(ctx, tx, treeCategories, storageID, id); err != nil {
			return err
		}
		if patch.SetParentID {
			if err := resolveParent(ctx, tx, treeCategories, storageID, &id, patch.ParentID); err != nil {
				return err
			}
		}

		// The casts pin each parameter's type for the CASE arms, as in
		// UpdateLocation.
		cat, err := scanCategory(tx.QueryRow(ctx, `
			UPDATE categories SET
			    name       = COALESCE($1::varchar, name),
			    parent_id  = CASE WHEN $2::bool THEN $3::uuid ELSE parent_id END,
			    updated_at = now()
			 WHERE id = $4 AND storage_id = $5
			RETURNING id, storage_id, parent_id, name, default_shelf_life_days, created_at, updated_at`,
			patch.Name, patch.SetParentID, patch.ParentID, id, storageID))
		if err != nil {
			return err
		}
		out = cat

		if !patch.SetParentID {
			// A rename changes no rule a product resolves through.
			return nil
		}
		products, err := productsUnderCategory(ctx, tx, storageID, id)
		if err != nil {
			return err
		}
		for _, productID := range products {
			if _, err := recomputeDerivedExpiry(ctx, tx, storageID, productID); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
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
		if err := lockStorageTree(ctx, tx, storageID); err != nil {
			return err
		}
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
