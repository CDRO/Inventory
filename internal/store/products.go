package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
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
	id, err := newID()
	if err != nil {
		return nil, err
	}
	if in.ItemType == "" {
		in.ItemType = ItemLongShelfLife
	}

	var out *Product
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		if in.CategoryID != nil {
			if err := requireSameStorage(ctx, tx, treeCategories, storageID, *in.CategoryID); err != nil {
				return err
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

		p, err := scanProduct(row)
		if err != nil {
			return err
		}
		out = p
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// SetProductCategory re-categorises a product, rejecting a category from
// another storage with ErrNotFound.
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
