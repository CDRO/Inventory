package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/CDRO/Inventory/internal/expiry"
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
// and so do its logs; that is the model's choice, not this function's. It
// writes no inventory_logs rows of its own: the ledger for this product is
// being erased, not appended to, which is the semantic difference from
// consuming down to zero (docs/specs/09-consumption-logging.md). It leaves
// catalog_products alone, which is insert-only (docs/specs/02-data-model.md).
//
// The returned string is the product's image_url when no other product still
// references that file, and "" otherwise — the "unless shared" half of
// docs/specs/07-shopping-list-reconciliation.md's deletion rule. Removing the
// file is the caller's job, deliberately: a file delete cannot be rolled back,
// so it must happen after this transaction commits rather than inside it.
func (s *Store) DeleteProduct(ctx context.Context, storageID, id uuid.UUID) (string, error) {
	var orphaned string
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		if err := requireProductInStorage(ctx, tx, storageID, id); err != nil {
			return err
		}
		var imageURL *string
		if err := tx.QueryRow(ctx, `
			SELECT image_url FROM products WHERE id = $1 AND storage_id = $2 FOR UPDATE`,
			id, storageID).Scan(&imageURL); err != nil {
			return fmt.Errorf("store: lock product to delete: %w", err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM products WHERE id = $1 AND storage_id = $2`, id, storageID); err != nil {
			return fmt.Errorf("store: delete product: %w", err)
		}
		if err := recordTombstones(ctx, tx, storageID, TombstoneProduct, []uuid.UUID{id}); err != nil {
			return err
		}
		name, err := orphanedImage(ctx, tx, imageURL)
		orphaned = name
		return err
	})
	if err != nil {
		return "", err
	}
	return orphaned, nil
}

// orphanedImage returns imageURL back when the row that carried it is gone and
// nothing else points at the same file, and "" in every other case.
//
// The check spans every storage rather than one, because the question is about
// a file on disk: two products holding the same image_url is the only thing
// that makes deleting the file wrong, and which storages they belong to does
// not change that.
func orphanedImage(ctx context.Context, q querier, imageURL *string) (string, error) {
	if imageURL == nil || *imageURL == "" {
		return "", nil
	}
	var stillUsed bool
	if err := q.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM products WHERE image_url = $1)`, *imageURL).Scan(&stillUsed); err != nil {
		return "", fmt.Errorf("store: check product image still used: %w", err)
	}
	if stillUsed {
		return "", nil
	}
	return *imageURL, nil
}

// ProductPatch is the edit surface of docs/specs/16-product-maintenance.md's
// PATCH route: every field a person can change on an existing product, each
// one optional.
//
// The three Set* flags exist because for those fields nil is a value a caller
// may legitimately be sending — "no category", "resolve the shelf life down
// the chain in docs/specs/08-expiration-and-classification.md", "no icon" —
// and a pointer alone cannot tell "absent" from "explicitly null". Collapsing
// the two would make clearing any of them unexpressible.
type ProductPatch struct {
	Name *string

	CategoryID    *uuid.UUID
	SetCategoryID bool

	ItemType *ItemType
	MinStock *int

	DefaultShelfLifeDays    *int
	SetDefaultShelfLifeDays bool

	IconName    *string
	SetIconName bool
}

// touchesExpiryRules reports whether this patch changes something the derived
// dates of the product's batches are computed from.
//
// Only two fields do: the category whose chain the shelf life climbs, and the
// product's own override that outranks it. A rename or a min_stock change
// moves no date, so running the cascade for those would lock every batch of
// the product only to write back what is already there.
func (p ProductPatch) touchesExpiryRules() bool {
	return p.SetCategoryID || p.SetDefaultShelfLifeDays
}

// UpdateProduct applies a patch in one transaction and returns the updated row
// together with how many batch dates the change moved.
//
// A category from another storage — like a product from another storage — is
// ErrNotFound, never a 403 and never a distinct error: the same-storage rule
// on category_id is the one the database cannot express, because a plain
// foreign key only checks that the category exists
// (docs/specs/03-auth-and-multi-tenancy.md).
//
// Changing the category or the shelf-life override recomputes the product's
// derived expiry dates in this same transaction, so its rules and its batches'
// dates can never be seen disagreeing. Dates a person set are untouched, as
// everywhere: the cascade is recomputeDerivedExpiry (expiry.go), the same code
// every other spec-08 caller runs, rather than a second implementation that
// could drift from it.
//
// The recomputed count is returned for every patch; only the caller knows
// whether the change was made *in order to* move dates and should report it.
// docs/specs/16-product-maintenance.md draws that line: a shelf-life change
// reports the count, a re-filing is silent.
func (s *Store) UpdateProduct(ctx context.Context, storageID, id uuid.UUID, patch ProductPatch) (*Product, int, error) {
	var out *Product
	var recomputed int

	err := s.inTx(ctx, func(tx pgx.Tx) error {
		if err := requireProductInStorage(ctx, tx, storageID, id); err != nil {
			return err
		}
		if patch.SetCategoryID && patch.CategoryID != nil {
			if err := requireSameStorage(ctx, tx, treeCategories, storageID, *patch.CategoryID); err != nil {
				return err
			}
		}

		// One statement of a fixed shape rather than a built-up one: a field
		// the patch does not mention writes itself back, and a field it clears
		// is driven by its own flag so that NULL means "clear" rather than
		// "absent". updated_at is bumped unconditionally
		// (docs/specs/02-data-model.md, "every write bumps updated_at").
		row := tx.QueryRow(ctx, `
			UPDATE products
			   SET name                    = COALESCE($3, name),
			       category_id             = CASE WHEN $4 THEN $5 ELSE category_id END,
			       item_type               = COALESCE($6, item_type),
			       min_stock               = COALESCE($7, min_stock),
			       default_shelf_life_days = CASE WHEN $8 THEN $9 ELSE default_shelf_life_days END,
			       icon_name               = CASE WHEN $10 THEN $11 ELSE icon_name END,
			       updated_at              = now()
			 WHERE id = $1 AND storage_id = $2
			RETURNING id, storage_id, name, category_id, catalog_id, item_type,
			          default_shelf_life_days, min_stock, image_url, icon_name, created_at, updated_at`,
			id, storageID,
			patch.Name,
			patch.SetCategoryID, patch.CategoryID,
			itemTypeArg(patch.ItemType),
			patch.MinStock,
			patch.SetDefaultShelfLifeDays, patch.DefaultShelfLifeDays,
			patch.SetIconName, patch.IconName)

		updated, err := scanProduct(row)
		if err != nil {
			return err
		}
		out = updated

		if !patch.touchesExpiryRules() {
			return nil
		}
		n, err := recomputeDerivedExpiry(ctx, tx, storageID, id)
		recomputed = n
		return err
	})
	if err != nil {
		return nil, 0, err
	}
	return out, recomputed, nil
}

// itemTypeArg turns an optional ItemType into the text COALESCE expects, so a
// patch that does not mention it leaves the column alone.
func itemTypeArg(t *ItemType) *string {
	if t == nil {
		return nil
	}
	text := string(*t)
	return &text
}

// GetProduct returns one product of a storage, or ErrNotFound — which a
// product of another storage gets too, indistinguishably.
func (s *Store) GetProduct(ctx context.Context, storageID, id uuid.UUID) (*Product, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT id, storage_id, name, category_id, catalog_id, item_type,
		       default_shelf_life_days, min_stock, image_url, icon_name, created_at, updated_at
		  FROM products
		 WHERE id = $1 AND storage_id = $2`, id, storageID)
	return scanProduct(row)
}

// ProductLog is one inventory_logs row as the product detail view shows it,
// with created_by resolved to a display name.
//
// CreatedBy is nil for a row whose user has since been deleted: the column is
// ON DELETE SET NULL, so "someone who no longer has an account did this" is a
// normal state of the table, the same thing ExportLog records.
type ProductLog struct {
	ID        uuid.UUID
	BatchID   *uuid.UUID
	ChangeQty int
	Reason    LogReason
	CreatedBy *string
	Timestamp time.Time
}

// ListProductLogs returns a product's most recent ledger rows, newest first —
// "this product's recent inventory_logs" on the detail view of
// docs/specs/16-product-maintenance.md.
//
// A product of another storage is ErrNotFound rather than an empty list: an
// empty list would say "that product exists here and has no history", which is
// a different statement and one a caller could use to probe.
//
// After a merge these are the union of both products' rows, each keeping its
// own timestamp and reason, because the two names always were one product.
func (s *Store) ListProductLogs(ctx context.Context, storageID, productID uuid.UUID, limit int) ([]ProductLog, error) {
	if err := requireProductInStorage(ctx, s.pool, storageID, productID); err != nil {
		return nil, err
	}

	rows, err := s.pool.Query(ctx, `
		SELECT l.id, l.batch_id, l.change_qty, l.reason, u.display_name, l.timestamp
		  FROM inventory_logs l
		  LEFT JOIN users u ON u.id = l.created_by
		 WHERE l.product_id = $1
		 ORDER BY l.timestamp DESC, l.id DESC
		 LIMIT $2`, productID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list product logs: %w", err)
	}
	defer rows.Close()

	out := []ProductLog{}
	for rows.Next() {
		var l ProductLog
		if err := rows.Scan(&l.ID, &l.BatchID, &l.ChangeQty, &l.Reason, &l.CreatedBy, &l.Timestamp); err != nil {
			return nil, fmt.Errorf("store: scan product log: %w", err)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// MergeResult is what a merge did: the survivor as it now stands, how many
// batches changed hands, how many of their dates the survivor's rules moved,
// and the image file the source left behind when nothing else references it.
type MergeResult struct {
	Survivor          *Product
	MovedBatches      int
	RecomputedBatches int
	// OrphanedImage is the source's image_url when no product still points at
	// that file, and "" otherwise. The caller removes it after the commit, for
	// the reason DeleteProduct gives.
	OrphanedImage string
}

// MergeProducts folds source into survivor — "these were the same thing all
// along, and *this* is its description"
// (docs/specs/16-product-maintenance.md).
//
// Both ids must belong to storageID; either one elsewhere, or nonexistent, is
// ErrNotFound. Merging a product into itself is ErrValidation — a 422, because
// it is a malformed request rather than a missing row. The HTTP layer refuses
// that case with a field message before ever reaching here; the guard below is
// the second line, so the invariant holds for any future caller too.
//
// **The survivor's own fields are not touched.** Nothing is copied from the
// source — name, image, category, item type, min_stock, shelf-life override
// all stay as they are, because field-mixing rules would turn a one-click
// cleanup into a form. Only updated_at moves.
//
// **No inventory_logs rows are written.** A merge changes nothing about how
// much stock exists, only which product row it belongs to, and a ledger entry
// recording "nothing moved" would be noise in the one table that exists to
// explain movement — the same reasoning RecomputeDerivedExpiry records for the
// cascade. This is the considered exception to the project rule that an
// inventory_batches write is paired with an inventory_logs row: that rule is
// about quantity, and neither write here (a change of owner, and a date the
// rules imply) changes a quantity. The survivor's history afterwards is simply
// the union of both histories, which is what reporting
// (docs/specs/11-reporting-and-analytics.md) should see.
//
// catalog_products is untouched: its rows are insert-only and describe what was
// once claimed, not the current state of any storage
// (docs/specs/02-data-model.md). The survivor keeps its own catalog_id; the
// source's is discarded with it.
func (s *Store) MergeProducts(ctx context.Context, storageID, survivorID, sourceID uuid.UUID) (*MergeResult, error) {
	if survivorID == sourceID {
		return nil, fmt.Errorf("%w: a product cannot be merged into itself", ErrValidation)
	}

	out := &MergeResult{}
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		// Both rows are locked before anything moves, in a fixed id order, so
		// that two merges naming the same pair from opposite ends take the
		// locks in the same sequence rather than deadlocking half-way — the
		// same precaution CorrectCatalogShelfLife takes for its walk. The
		// lookup is also the same-storage check for both ids: a row in another
		// storage simply is not found.
		first, second := survivorID, sourceID
		if second.String() < first.String() {
			first, second = second, first
		}
		for _, id := range []uuid.UUID{first, second} {
			var found uuid.UUID
			err := tx.QueryRow(ctx, `
				SELECT id FROM products WHERE id = $1 AND storage_id = $2 FOR UPDATE`,
				id, storageID).Scan(&found)
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			if err != nil {
				return fmt.Errorf("store: lock product to merge: %w", err)
			}
		}

		var sourceImage *string
		if err := tx.QueryRow(ctx, `SELECT image_url FROM products WHERE id = $1`, sourceID).
			Scan(&sourceImage); err != nil {
			return fmt.Errorf("store: read merged product image: %w", err)
		}

		// The moved batches are read *before* the re-point, while they still
		// carry the source's id, and with the creation dates their expiry is
		// measured from. That captured set is exactly what the recompute below
		// touches, so the survivor's own batches — which already follow the
		// survivor's rules — are left as they are.
		moved, err := loadDerivedBatches(ctx, tx, sourceID)
		if err != nil {
			return err
		}

		// Re-point history and references. All three must precede the DELETE
		// below: shopping_list_items.matched_product_id is ON DELETE SET NULL,
		// so a delete that ran first would silently drop the matches instead
		// of failing.
		//
		// When docs/specs/20-barcode-recall.md lands, product_barcodes.product_id
		// is re-pointed here too — a barcode already on the survivor wins on
		// conflict, and the source's duplicate row is dropped. That table does
		// not exist yet, so this is the marker for where it attaches.
		batchTag, err := tx.Exec(ctx,
			`UPDATE inventory_batches SET product_id = $1 WHERE product_id = $2`, survivorID, sourceID)
		if err != nil {
			return fmt.Errorf("store: re-point merged batches: %w", err)
		}
		out.MovedBatches = int(batchTag.RowsAffected())

		if _, err := tx.Exec(ctx,
			`UPDATE inventory_logs SET product_id = $1 WHERE product_id = $2`, survivorID, sourceID); err != nil {
			return fmt.Errorf("store: re-point merged logs: %w", err)
		}
		if _, err := tx.Exec(ctx,
			`UPDATE shopping_list_items SET matched_product_id = $1 WHERE matched_product_id = $2`,
			survivorID, sourceID); err != nil {
			return fmt.Errorf("store: re-point merged shopping list matches: %w", err)
		}

		// The moved batches now belong to the survivor, so the survivor's
		// rules are the ones that apply to them: 'derived' means "follows the
		// current rules", and the rules just changed for exactly these rows.
		rules, err := expiryRulesFor(ctx, tx, storageID, survivorID)
		if err != nil {
			return err
		}
		recomputed, err := applyDerivedExpiry(ctx, tx, moved, expiry.Resolve(rules))
		if err != nil {
			return err
		}
		out.RecomputedBatches = recomputed

		if _, err := tx.Exec(ctx, `DELETE FROM products WHERE id = $1 AND storage_id = $2`,
			sourceID, storageID); err != nil {
			return fmt.Errorf("store: delete merged product: %w", err)
		}
		if err := recordTombstones(ctx, tx, storageID, TombstoneProduct, []uuid.UUID{sourceID}); err != nil {
			return err
		}
		orphaned, err := orphanedImage(ctx, tx, sourceImage)
		if err != nil {
			return err
		}
		out.OrphanedImage = orphaned

		row := tx.QueryRow(ctx, `
			UPDATE products SET updated_at = now()
			 WHERE id = $1 AND storage_id = $2
			RETURNING id, storage_id, name, category_id, catalog_id, item_type,
			          default_shelf_life_days, min_stock, image_url, icon_name, created_at, updated_at`,
			survivorID, storageID)
		survivor, err := scanProduct(row)
		if err != nil {
			return err
		}
		out.Survivor = survivor
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
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
