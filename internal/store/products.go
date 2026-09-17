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

// ItemType drives default-expiry behaviour
// (docs/specs/08-expiration-and-classification.md).
type ItemType string

const (
	ItemPerishable    ItemType = "perishable"
	ItemLongShelfLife ItemType = "long_shelf_life"
	ItemNonPerishable ItemType = "non_perishable"
)

// Product is a storage-local product.
//
// CatalogID is deliberately absent from this struct's JSON-facing use: it is
// server-side only and must never appear in an API response
// (docs/specs/02-data-model.md). It is carried here because the shelf-life
// cascade in spec 08 needs it.
type Product struct {
	ID                   uuid.UUID
	StorageID            uuid.UUID
	Name                 string
	CategoryID           *uuid.UUID
	CatalogID            *uuid.UUID
	ItemType             ItemType
	DefaultShelfLifeDays *int
	MinStock             int
	ImageURL             *string
	IconName             *string
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

// NewProduct is the input to CreateProduct.
type NewProduct struct {
	Name                 string
	CategoryID           *uuid.UUID
	CatalogID            *uuid.UUID
	ItemType             ItemType
	DefaultShelfLifeDays *int
	MinStock             int
	ImageURL             *string
	IconName             *string
}

// CreateProduct inserts a product, rejecting a category from another storage
// with ErrNotFound.
//
// The same-storage rule on category_id is the one the database cannot express:
// categories and products both carry storage_id, but a plain foreign key only
// checks that the category exists, not that it belongs here.
func (s *Store) CreateProduct(ctx context.Context, storageID uuid.UUID, in NewProduct) (*Product, error) {
	var out *Product
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		p, err := createProduct(ctx, tx, storageID, in)
		out = p
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// createProduct is CreateProduct inside a caller's transaction.
func createProduct(ctx context.Context, tx pgx.Tx, storageID uuid.UUID, in NewProduct) (*Product, error) {
	id, err := newID()
	if err != nil {
		return nil, err
	}
	if in.ItemType == "" {
		in.ItemType = ItemLongShelfLife
	}

	if in.CategoryID != nil {
		if err := requireSameStorage(ctx, tx, treeCategories, storageID, *in.CategoryID); err != nil {
			return nil, err
		}
	}

	row := tx.QueryRow(ctx, `
		INSERT INTO products (id, storage_id, name, category_id, catalog_id, item_type,
		                      default_shelf_life_days, min_stock, image_url, icon_name)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		RETURNING id, storage_id, name, category_id, catalog_id, item_type,
		          default_shelf_life_days, min_stock, image_url, icon_name, created_at, updated_at`,
		id, storageID, in.Name, in.CategoryID, in.CatalogID, string(in.ItemType),
		in.DefaultShelfLifeDays, in.MinStock, in.ImageURL, in.IconName)

	return scanProduct(row)
}

// SetProductCategory re-categorises a product, rejecting a category from
// another storage with ErrNotFound.
// Re-filing a product recomputes its derived expiry dates.
//
// docs/specs/08-expiration-and-classification.md is explicit that this is not
// optional: "'derived' means 'follows the current rules', and a stale derived
// date is simply a wrong one". Moving cheese out of Dairy and into Canned
// changes which rule applies to it, so the dates that came from the old rule
// have to follow — while dates a person typed stay exactly where they are,
// like everywhere else.
func (s *Store) SetProductCategory(ctx context.Context, storageID, id uuid.UUID, categoryID *uuid.UUID) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		if err := requireProductInStorage(ctx, tx, storageID, id); err != nil {
			return err
		}
		if categoryID != nil {
			if err := requireSameStorage(ctx, tx, treeCategories, storageID, *categoryID); err != nil {
				return err
			}
		}

		if _, err := tx.Exec(ctx, `
			UPDATE products SET category_id = $1, updated_at = now()
			 WHERE id = $2 AND storage_id = $3`, categoryID, id, storageID); err != nil {
			return fmt.Errorf("store: set product category: %w", err)
		}

		// In the same transaction as the move, so the product's category and
		// its batches' dates can never be seen disagreeing.
		_, err := recomputeDerivedExpiry(ctx, tx, storageID, id)
		return err
	})
}

// SetProductCategoryAsUser is SetProductCategory, additionally recording a
// metadata_filled contribution when the change fills in a category that was
// previously unset (docs/specs/51-gamification-scoring.md,
// docs/specs/52-gamification-quests-and-ui.md's "uncategorized" quest). Like
// UpdateProductMinStockAsUser, re-filing an already-categorized product, or
// clearing one, earns nothing: the reward is for closing the gap.
func (s *Store) SetProductCategoryAsUser(ctx context.Context, storageID, id uuid.UUID, categoryID *uuid.UUID, userID uuid.UUID) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		var previous *uuid.UUID
		if err := requireProductInStorage(ctx, tx, storageID, id); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `
			SELECT category_id FROM products WHERE id = $1 AND storage_id = $2 FOR UPDATE`,
			id, storageID).Scan(&previous); err != nil {
			return fmt.Errorf("store: lock product category: %w", err)
		}
		if categoryID != nil {
			if err := requireSameStorage(ctx, tx, treeCategories, storageID, *categoryID); err != nil {
				return err
			}
		}

		if _, err := tx.Exec(ctx, `
			UPDATE products SET category_id = $1, updated_at = now()
			 WHERE id = $2 AND storage_id = $3`, categoryID, id, storageID); err != nil {
			return fmt.Errorf("store: set product category: %w", err)
		}
		if _, err := recomputeDerivedExpiry(ctx, tx, storageID, id); err != nil {
			return err
		}

		if previous == nil && categoryID != nil {
			generator := gamification.GeneratorUncategorized
			return recordContribution(ctx, tx, storageID, userID, gamification.KindMetadataFilled, &id, &generator)
		}
		return nil
	})
}

// ProductImageInStorage reports whether any product in this storage uses
// imageURL as its picture.
//
// It is the authorization check for serving a stored product photo: the file
// on disk carries no owner, so a photo belongs to a storage exactly when one
// of that storage's products points at it. Any other storage — and any URL no
// product uses, such as a photo left behind by a failed confirm — answers
// false, which the caller turns into the same 404 as a name that never
// existed.
func (s *Store) ProductImageInStorage(ctx context.Context, storageID uuid.UUID, imageURL string) (bool, error) {
	var found bool
	err := s.pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM products WHERE storage_id = $1 AND image_url = $2)`,
		storageID, imageURL).Scan(&found)
	if err != nil {
		return false, fmt.Errorf("store: product image lookup: %w", err)
	}
	return found, nil
}

// SetProductImageAsUser sets a product's image or icon, recording a
// metadata_filled contribution when it fills in a picture that was
// previously unset (docs/specs/51-gamification-scoring.md,
// docs/specs/52-gamification-quests-and-ui.md's "imageless" quest). Exactly
// one of imageURL and iconName is expected to be set by the caller — both
// nil clears the picture and earns nothing.
func (s *Store) SetProductImageAsUser(ctx context.Context, storageID, id uuid.UUID, imageURL, iconName *string, userID uuid.UUID) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		var previousImage, previousIcon *string
		if err := requireProductInStorage(ctx, tx, storageID, id); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `
			SELECT image_url, icon_name FROM products WHERE id = $1 AND storage_id = $2 FOR UPDATE`,
			id, storageID).Scan(&previousImage, &previousIcon); err != nil {
			return fmt.Errorf("store: lock product image: %w", err)
		}

		if _, err := tx.Exec(ctx, `
			UPDATE products SET image_url = $1, icon_name = $2, updated_at = now()
			 WHERE id = $3 AND storage_id = $4`, imageURL, iconName, id, storageID); err != nil {
			return fmt.Errorf("store: set product image: %w", err)
		}

		hadNone := previousImage == nil && previousIcon == nil
		hasOne := imageURL != nil || iconName != nil
		if hadNone && hasOne {
			generator := gamification.GeneratorImageless
			return recordContribution(ctx, tx, storageID, userID, gamification.KindMetadataFilled, &id, &generator)
		}
		return nil
	})
}

// DeleteProduct removes a product and records a tombstone in the same
// transaction.
//
// Its batches go with it (inventory_batches.product_id is ON DELETE CASCADE)
// and so do its logs; that is the model's choice, not this function's.
func (s *Store) DeleteProduct(ctx context.Context, storageID, id uuid.UUID) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		if err := requireProductInStorage(ctx, tx, storageID, id); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM products WHERE id = $1 AND storage_id = $2`, id, storageID); err != nil {
			return fmt.Errorf("store: delete product: %w", err)
		}
		return recordTombstones(ctx, tx, storageID, TombstoneProduct, []uuid.UUID{id})
	})
}

// ListProducts returns a storage's products, alphabetical by name — the whole
// list, for the manual-correction picker in
// docs/specs/09-consumption-logging.md. Browsing or paginating products at
// scale belongs to specs 10 and 11; nothing here is a substitute for that.
func (s *Store) ListProducts(ctx context.Context, storageID uuid.UUID) ([]Product, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, storage_id, name, category_id, catalog_id, item_type,
		       default_shelf_life_days, min_stock, image_url, icon_name, created_at, updated_at
		  FROM products
		 WHERE storage_id = $1
		 ORDER BY name`, storageID)
	if err != nil {
		return nil, fmt.Errorf("store: list products: %w", err)
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
	return out, rows.Err()
}

// CurrentStock is the live sum of a product's batches
// (docs/specs/10-reorder-and-shopping-export.md). It is computed, never stored:
// a denormalised counter is a number that can drift away from the rows it
// claims to summarise.
func (s *Store) CurrentStock(ctx context.Context, storageID, productID uuid.UUID) (int, error) {
	var total int
	err := s.pool.QueryRow(ctx, `
		SELECT coalesce(sum(b.quantity), 0)
		  FROM inventory_batches b
		  JOIN products p ON p.id = b.product_id
		 WHERE b.product_id = $1 AND p.storage_id = $2`, productID, storageID).Scan(&total)
	if err != nil {
		return 0, fmt.Errorf("store: current stock: %w", err)
	}
	return total, nil
}

// requireProductInStorage is the products equivalent of requireSameStorage. A
// product in another storage returns ErrNotFound.
func requireProductInStorage(ctx context.Context, q querier, storageID, id uuid.UUID) error {
	var found bool
	if err := q.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM products WHERE id = $1 AND storage_id = $2)`,
		id, storageID).Scan(&found); err != nil {
		return fmt.Errorf("store: check product in storage: %w", err)
	}
	if !found {
		return ErrNotFound
	}
	return nil
}

func scanProduct(row rowScanner) (*Product, error) {
	var p Product
	var itemType string
	err := row.Scan(&p.ID, &p.StorageID, &p.Name, &p.CategoryID, &p.CatalogID, &itemType,
		&p.DefaultShelfLifeDays, &p.MinStock, &p.ImageURL, &p.IconName, &p.CreatedAt, &p.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: scan product: %w", err)
	}
	p.ItemType = ItemType(itemType)
	return &p, nil
}
