package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// CatalogProduct is a row of the global, anonymous product catalogue.
//
// It carries no storage reference, no user, no quantity and no count by
// design: a catalog row describes *what a product is*, never who has it. Any
// response derived from it must expose display fields only — never the id,
// never created_at, never ordering that could hint at how many storages exist
// (docs/specs/02-data-model.md).
type CatalogProduct struct {
	ID                   uuid.UUID
	NormalizedName       string
	DisplayName          string
	BaseID               *uuid.UUID
	CategoryPath         *string
	ItemType             ItemType
	ImageURL             *string
	IconName             *string
	DefaultShelfLifeDays *int
	CreatedAt            time.Time
}

// NewCatalogProduct is the input to InsertCatalogProduct.
//
// ShownID is the catalog row the user was offered and declined, if any. It is
// how the variant graph gets built: from real usage, never authored or
// AI-generated.
type NewCatalogProduct struct {
	DisplayName          string
	CategoryPath         *string
	ItemType             ItemType
	ImageURL             *string
	IconName             *string
	DefaultShelfLifeDays *int
	ShownID              *uuid.UUID
}

// NormalizeCatalogName lowercases, trims and collapses whitespace. It is the
// uniqueness key, so it has to be deterministic and is exported for the
// matching service to reuse rather than re-derive.
func NormalizeCatalogName(name string) string {
	return strings.Join(strings.Fields(strings.ToLower(strings.TrimSpace(name))), " ")
}

// InsertCatalogProduct adds a row if its normalized name is not already
// present, and returns the row that now holds that name.
//
// **Insert-only. There is no upsert and no edit path.** The reason is abuse,
// not tidiness: a globally visible row that any storage can rewrite is a
// covert messaging channel between households. Once someone notices that
// editing a product name changes what strangers see, it will be used for that.
// Insert-only reduces the channel to a single one-shot write by whoever first
// names a product, which cannot carry a conversation.
//
// A conflict is therefore not an error — it means another storage described
// this product first, and their description stands.
func (s *Store) InsertCatalogProduct(ctx context.Context, in NewCatalogProduct) (*CatalogProduct, error) {
	normalized := NormalizeCatalogName(in.DisplayName)
	if normalized == "" {
		return nil, fmt.Errorf("%w: catalog name must not be blank", ErrValidation)
	}
	if in.ItemType == "" {
		in.ItemType = ItemLongShelfLife
	}

	id, err := newID()
	if err != nil {
		return nil, err
	}

	var out *CatalogProduct
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		baseID, err := resolveVariantBase(ctx, tx, in.ShownID)
		if err != nil {
			return err
		}

		_, err = tx.Exec(ctx, `
			INSERT INTO catalog_products (id, normalized_name, display_name, base_id, category_path,
			                              item_type, image_url, icon_name, default_shelf_life_days)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
			ON CONFLICT (normalized_name) DO NOTHING`,
			id, normalized, in.DisplayName, baseID, in.CategoryPath,
			string(in.ItemType), in.ImageURL, in.IconName, in.DefaultShelfLifeDays)
		if err != nil {
			return fmt.Errorf("store: insert catalog product: %w", err)
		}

		// Read back by name, not by id: on conflict the surviving row is the
		// one written first, and that is the row callers must be given.
		row := tx.QueryRow(ctx, `
			SELECT id, normalized_name, display_name, base_id, category_path, item_type,
			       image_url, icon_name, default_shelf_life_days, created_at
			  FROM catalog_products WHERE normalized_name = $1`, normalized)

		cp, err := scanCatalogProduct(row)
		if err != nil {
			return err
		}
		out = cp
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// resolveVariantBase turns "the row the user was shown" into the base a new
// variant should point at.
//
// Only one level is used: if the shown row is itself a variant, the new row
// links to that variant's base instead. Chains drift semantically — cherry
// tomato → tomato is useful, and a third hop is how "tomato" ends up a variant
// of "vegetable" — and a flat graph keeps the sibling lookup a single cheap
// query.
func resolveVariantBase(ctx context.Context, q querier, shownID *uuid.UUID) (*uuid.UUID, error) {
	if shownID == nil {
		return nil, nil
	}

	var id uuid.UUID
	var baseID *uuid.UUID
	err := q.QueryRow(ctx,
		`SELECT id, base_id FROM catalog_products WHERE id = $1`, *shownID).Scan(&id, &baseID)
	if errors.Is(err, pgx.ErrNoRows) {
		// The row the client claims to have been shown does not exist. Drop
		// the link rather than failing the insert: the product is still worth
		// recording, and the variant edge is a convenience.
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: resolve variant base: %w", err)
	}

	if baseID != nil {
		return baseID, nil
	}
	return &id, nil
}

// CatalogVariants returns the display names of a row's variant siblings: its
// children when it is a base, its co-variants when it is itself a variant.
//
// Only display names are returned. Handing back ids or timestamps would let a
// caller correlate catalog rows across requests and start inferring how many
// storages exist.
func (s *Store) CatalogVariants(ctx context.Context, id uuid.UUID, limit int) ([]string, error) {
	if limit <= 0 {
		limit = 5
	}

	rows, err := s.pool.Query(ctx, `
		WITH target AS (
		    SELECT id, coalesce(base_id, id) AS base FROM catalog_products WHERE id = $1
		)
		SELECT c.display_name
		  FROM catalog_products c, target t
		 WHERE c.id <> t.id
		   AND (c.base_id = t.base OR c.id = t.base)
		 ORDER BY c.display_name
		 LIMIT $2`, id, limit)
	if err != nil {
		return nil, fmt.Errorf("store: catalog variants: %w", err)
	}
	defer rows.Close()

	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("store: scan catalog variant: %w", err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: catalog variants: %w", err)
	}
	return names, nil
}

// SetCatalogShelfLife is the ONLY permitted update to a catalog_products row,
// and only an admin may call it.
//
// It does not reopen the abuse hole the insert-only rule closes: the field is
// a bounded integer written by an admin, so it cannot carry a message to
// another household — unlike the free-text and image fields, which stay
// permanently immutable.
func (s *Store) SetCatalogShelfLife(ctx context.Context, id uuid.UUID, days *int) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE catalog_products SET default_shelf_life_days = $1 WHERE id = $2`, days, id)
	if err != nil {
		return fmt.Errorf("store: set catalog shelf life: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteCatalogProduct is admin moderation, the only way a bad entry is
// removed. It never touches any storage's own products: products.catalog_id is
// ON DELETE SET NULL, so a household keeps the product it created.
func (s *Store) DeleteCatalogProduct(ctx context.Context, id uuid.UUID) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM catalog_products WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("store: delete catalog product: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// FindCatalogProduct looks a row up by its normalized name.
func (s *Store) FindCatalogProduct(ctx context.Context, name string) (*CatalogProduct, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT id, normalized_name, display_name, base_id, category_path, item_type,
		       image_url, icon_name, default_shelf_life_days, created_at
		  FROM catalog_products WHERE normalized_name = $1`, NormalizeCatalogName(name))
	return scanCatalogProduct(row)
}

func scanCatalogProduct(row rowScanner) (*CatalogProduct, error) {
	var c CatalogProduct
	var itemType string
	err := row.Scan(&c.ID, &c.NormalizedName, &c.DisplayName, &c.BaseID, &c.CategoryPath,
		&itemType, &c.ImageURL, &c.IconName, &c.DefaultShelfLifeDays, &c.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: scan catalog product: %w", err)
	}
	c.ItemType = ItemType(itemType)
	return &c, nil
}
