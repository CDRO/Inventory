package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/CDRO/Inventory/internal/gamification"
)

// ShoppingListSource is how a list arrived.
type ShoppingListSource string

const (
	// SourceText is a typed or pasted list.
	SourceText ShoppingListSource = "text"
	// SourcePhoto is a photographed list, whose lines were read out by the
	// vision model before reaching these rows.
	SourcePhoto ShoppingListSource = "photo"
)

// ShoppingListItemStatus is the matching outcome for one line. The values are
// the ones shopping_list_items.status accepts.
type ShoppingListItemStatus string

const (
	ItemExactMatch ShoppingListItemStatus = "exact_match"
	ItemNewItem    ShoppingListItemStatus = "new_item"
	ItemAmbiguous  ShoppingListItemStatus = "ambiguous"
	ItemResolved   ShoppingListItemStatus = "resolved"
)

// ShoppingList is one submitted list.
type ShoppingList struct {
	ID        uuid.UUID
	StorageID uuid.UUID
	Source    ShoppingListSource
	CreatedBy *uuid.UUID
	CreatedAt time.Time
}

// ShoppingListItem is one line of a list.
type ShoppingListItem struct {
	ID               uuid.UUID
	ShoppingListID   uuid.UUID
	RawText          string
	Status           ShoppingListItemStatus
	MatchedProductID *uuid.UUID
	ResolvedQuantity *int
	CreatedAt        time.Time
}

// NewShoppingListItem is one line as the matching service classified it.
type NewShoppingListItem struct {
	RawText          string
	Status           ShoppingListItemStatus
	MatchedProductID *uuid.UUID
}

// CreateShoppingList writes the list and all of its lines in one transaction.
//
// All-or-nothing on purpose: a partially written list is worse than no list,
// because the user cannot tell which of their lines the system actually kept
// and would have to re-read their own handwriting to find out.
//
// A matched product is validated against storageID before it is stored, so a
// caller cannot attach a line to a product in someone else's storage even if
// its own matching stage were tricked into proposing one.
func (s *Store) CreateShoppingList(ctx context.Context, storageID uuid.UUID, source ShoppingListSource, createdBy *uuid.UUID, items []NewShoppingListItem) (*ShoppingList, []ShoppingListItem, error) {
	if len(items) == 0 {
		return nil, nil, fmt.Errorf("%w: a shopping list needs at least one line", ErrValidation)
	}

	listID, err := newID()
	if err != nil {
		return nil, nil, err
	}

	var list *ShoppingList
	var created []ShoppingListItem

	err = s.inTx(ctx, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			INSERT INTO shopping_lists (id, storage_id, source, created_by)
			VALUES ($1, $2, $3, $4)
			RETURNING id, storage_id, source, created_by, created_at`,
			listID, storageID, string(source), createdBy)

		l, err := scanShoppingList(row)
		if err != nil {
			return err
		}
		list = l

		for _, in := range items {
			if in.MatchedProductID != nil {
				if err := requireProductInStorage(ctx, tx, storageID, *in.MatchedProductID); err != nil {
					return err
				}
			}

			itemID, err := newID()
			if err != nil {
				return err
			}

			itemRow := tx.QueryRow(ctx, `
				INSERT INTO shopping_list_items (id, shopping_list_id, raw_text, status, matched_product_id)
				VALUES ($1, $2, $3, $4, $5)
				RETURNING id, shopping_list_id, raw_text, status, matched_product_id, resolved_quantity, created_at`,
				itemID, listID, in.RawText, string(in.Status), in.MatchedProductID)

			item, err := scanShoppingListItem(itemRow)
			if err != nil {
				return err
			}
			created = append(created, *item)
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return list, created, nil
}

// ShoppingListWithItems reads one list and its lines, scoped to storageID.
//
// A list belonging to another storage is ErrNotFound, the same answer a
// nonexistent id gets.
func (s *Store) ShoppingListWithItems(ctx context.Context, storageID, listID uuid.UUID) (*ShoppingList, []ShoppingListItem, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT id, storage_id, source, created_by, created_at
		  FROM shopping_lists WHERE id = $1 AND storage_id = $2`, listID, storageID)

	list, err := scanShoppingList(row)
	if err != nil {
		return nil, nil, err
	}

	rows, err := s.pool.Query(ctx, `
		SELECT id, shopping_list_id, raw_text, status, matched_product_id, resolved_quantity, created_at
		  FROM shopping_list_items
		 WHERE shopping_list_id = $1
		 ORDER BY created_at, id`, listID)
	if err != nil {
		return nil, nil, fmt.Errorf("store: list shopping list items: %w", err)
	}
	defer rows.Close()

	var items []ShoppingListItem
	for rows.Next() {
		item, err := scanShoppingListItem(rows)
		if err != nil {
			return nil, nil, err
		}
		items = append(items, *item)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("store: list shopping list items: %w", err)
	}
	return list, items, nil
}

// ShoppingListItemByID reads one line, scoped to storageID through its list.
//
// The join is what enforces the scope: shopping_list_items has no storage_id of
// its own, so an item id from another storage must be resolved through the
// list that owns it or it would be readable by anyone who guessed the id.
func (s *Store) ShoppingListItemByID(ctx context.Context, storageID, itemID uuid.UUID) (*ShoppingListItem, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT i.id, i.shopping_list_id, i.raw_text, i.status, i.matched_product_id,
		       i.resolved_quantity, i.created_at
		  FROM shopping_list_items i
		  JOIN shopping_lists l ON l.id = i.shopping_list_id
		 WHERE i.id = $1 AND l.storage_id = $2`, itemID, storageID)

	return scanShoppingListItem(row)
}

// RematchShoppingListItem replaces a line's text and its matching outcome.
//
// This exists because matching is explicitly *not* automatic after ingestion
// (docs/specs/07-shopping-list-reconciliation.md): a user who corrects a
// misread line asks for a re-match, rather than having the system silently
// re-run and change what they were looking at. Nothing here re-runs matching
// itself — the caller does that and passes the outcome in, so the decision to
// re-match stays an explicit user action all the way down.
//
// An already-resolved line is refused with ErrConflict. Re-matching it would
// discard a decision the user has already made, and possibly one that already
// created inventory.
func (s *Store) RematchShoppingListItem(ctx context.Context, storageID, itemID uuid.UUID, rawText string, status ShoppingListItemStatus, matchedProductID *uuid.UUID) (*ShoppingListItem, error) {
	var out *ShoppingListItem

	err := s.inTx(ctx, func(tx pgx.Tx) error {
		var current ShoppingListItemStatus
		err := tx.QueryRow(ctx, `
			SELECT i.status
			  FROM shopping_list_items i
			  JOIN shopping_lists l ON l.id = i.shopping_list_id
			 WHERE i.id = $1 AND l.storage_id = $2
			 FOR UPDATE OF i`, itemID, storageID).Scan(&current)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("store: load shopping list item: %w", err)
		}
		if current == ItemResolved {
			return fmt.Errorf("%w: this line is already resolved", ErrConflict)
		}

		if matchedProductID != nil {
			if err := requireProductInStorage(ctx, tx, storageID, *matchedProductID); err != nil {
				return err
			}
		}

		row := tx.QueryRow(ctx, `
			UPDATE shopping_list_items
			   SET raw_text = $1, status = $2, matched_product_id = $3
			 WHERE id = $4
			RETURNING id, shopping_list_id, raw_text, status, matched_product_id, resolved_quantity, created_at`,
			rawText, string(status), matchedProductID, itemID)

		item, err := scanShoppingListItem(row)
		if err != nil {
			return err
		}
		out = item
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ResolveLine is what a person confirmed for one shopping-list line
// (docs/specs/07-shopping-list-reconciliation.md, "Resolution UI per state").
//
// At most one of ProductID and NewProduct is set. Neither is a dismissal — the
// line was not bought, or not wanted — and records nothing but the resolution,
// so its Quantity must be 0: stock of no product cannot be added.
type ResolveLine struct {
	// ProductID is an existing product of this storage: an exact match, or the
	// candidate picked for an ambiguous line.
	ProductID *uuid.UUID
	// NewProduct is created in the same transaction as the batch.
	NewProduct *ResolvedProduct

	// Quantity is what was actually bought. Above 0 it becomes one batch at
	// LocationID, with its purchase log row; 0 resolves without touching
	// inventory.
	Quantity   int
	LocationID *uuid.UUID
}

// ResolvedProduct is a product a resolution creates.
type ResolvedProduct struct {
	Name     string
	ItemType ItemType
	MinStock int
	// CategoryID files it under an existing category. CategoryPath instead
	// resolves a catalog card's "Food > Dairy" against this storage's tree,
	// creating whatever nodes are missing. At most one is set.
	CategoryID           *uuid.UUID
	CategoryPath         *string
	DefaultShelfLifeDays *int
	IconName             *string
	// ImageURL is the product's own picture, already written to permanent
	// storage by the caller. Never a suggestion-cache URL (spec 07, "Product
	// images — permanent").
	ImageURL *string

	// Exactly one of AcceptedCatalogID and Catalog is set: link the product to
	// the catalog row the person accepted, or describe it to the catalog as a
	// new row (insert-only, ON CONFLICT DO NOTHING — see InsertCatalogProduct).
	AcceptedCatalogID *uuid.UUID
	Catalog           *NewCatalogProduct
}

// ResolveResult is what a resolution wrote.
type ResolveResult struct {
	Item           ShoppingListItem
	ProductCreated bool
	// BatchID is the batch the quantity became, nil for a quantity of 0.
	BatchID *uuid.UUID
}

// ResolveShoppingListItem applies what a person confirmed for one line, all or
// nothing: a new product (with its catalog row or catalog link, and any
// missing category nodes), its batch and purchase log row, and the line marked
// resolved — in one transaction.
//
// It resolves exactly one line. That is the acceptance criterion "resolving one
// item doesn't block or auto-resolve others" expressed in the only place it can
// be guaranteed: a statement that names a single id cannot cascade to a
// sibling, no matter what the calling handler does. And it is the last
// criterion's only writer: no products or inventory_batches row exists for a
// line before this call.
//
// Resolving is idempotent-hostile on purpose: a second resolve of the same line
// is ErrConflict rather than a silent overwrite, because the first one already
// wrote a batch and the second would double it.
//
// Refusals: ErrNotFound for a line, product, category or location that is not
// in this storage — the same answer as one that does not exist; ErrValidation
// for a shape that cannot be applied (both or neither product, stock with no
// product, stock with no location).
//
// userID attributes the resolve: the batch and the product are created by
// them, and clearing an ambiguous line records its contribution
// (docs/specs/51-gamification-scoring.md). nil earns nobody XP, which is the
// correct behaviour for a system-driven resolve rather than an error.
func (s *Store) ResolveShoppingListItem(ctx context.Context, storageID, itemID uuid.UUID, in ResolveLine, userID *uuid.UUID) (*ResolveResult, error) {
	if err := in.validate(); err != nil {
		return nil, err
	}

	var out *ResolveResult

	err := s.inTx(ctx, func(tx pgx.Tx) error {
		var current ShoppingListItemStatus
		err := tx.QueryRow(ctx, `
			SELECT i.status
			  FROM shopping_list_items i
			  JOIN shopping_lists l ON l.id = i.shopping_list_id
			 WHERE i.id = $1 AND l.storage_id = $2
			 FOR UPDATE OF i`, itemID, storageID).Scan(&current)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("store: load shopping list item: %w", err)
		}
		if current == ItemResolved {
			return fmt.Errorf("%w: this line is already resolved", ErrConflict)
		}

		result := &ResolveResult{}

		productID := in.ProductID
		switch {
		case productID != nil:
			if err := requireProductInStorage(ctx, tx, storageID, *productID); err != nil {
				return err
			}
		case in.NewProduct != nil:
			id, err := createResolvedProduct(ctx, tx, storageID, *in.NewProduct)
			if err != nil {
				return err
			}
			productID = &id
			result.ProductCreated = true
		}

		if in.Quantity > 0 {
			// createBatch refuses a location outside this storage itself.
			batch, err := createBatch(ctx, tx, storageID, NewBatch{
				ProductID:  *productID,
				LocationID: *in.LocationID,
				Quantity:   in.Quantity,
				Reason:     ReasonPurchase,
				CreatedBy:  userID,
			})
			if err != nil {
				return err
			}
			result.BatchID = &batch.ID
		}

		row := tx.QueryRow(ctx, `
			UPDATE shopping_list_items
			   SET status = 'resolved', matched_product_id = $1, resolved_quantity = $2
			 WHERE id = $3
			RETURNING id, shopping_list_id, raw_text, status, matched_product_id, resolved_quantity, created_at`,
			productID, in.Quantity, itemID)

		item, err := scanShoppingListItem(row)
		if err != nil {
			return err
		}
		result.Item = *item

		if userID != nil && current == ItemAmbiguous {
			ref := itemID
			if productID != nil {
				ref = *productID
			}
			if err := recordContribution(ctx, tx, storageID, *userID, gamification.KindAmbiguityResolved, &ref, nil); err != nil {
				return err
			}
		}
		out = result
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// validate refuses a resolution that cannot be applied, before any lock is
// taken. Whether the ids it names belong to this storage is decided inside the
// transaction.
func (in ResolveLine) validate() error {
	switch {
	case in.ProductID != nil && in.NewProduct != nil:
		return fmt.Errorf("%w: a resolution names an existing product or a new one, not both", ErrValidation)
	case in.Quantity < 0:
		return fmt.Errorf("%w: resolved quantity cannot be negative", ErrValidation)
	case in.Quantity > 0 && in.ProductID == nil && in.NewProduct == nil:
		return fmt.Errorf("%w: stock cannot be added without a product", ErrValidation)
	case in.Quantity > 0 && in.LocationID == nil:
		return fmt.Errorf("%w: stock needs a location", ErrValidation)
	}
	if p := in.NewProduct; p != nil {
		switch {
		case strings.TrimSpace(p.Name) == "":
			return fmt.Errorf("%w: a new product needs a name", ErrValidation)
		case p.CategoryID != nil && p.CategoryPath != nil:
			return fmt.Errorf("%w: a new product names a category or a category path, not both", ErrValidation)
		case (p.AcceptedCatalogID == nil) == (p.Catalog == nil):
			return fmt.Errorf("%w: a new product links an accepted catalog row or adds one, exactly one", ErrValidation)
		case p.MinStock < 0:
			return fmt.Errorf("%w: minimum stock cannot be negative", ErrValidation)
		}
	}
	return nil
}

// createResolvedProduct creates a product a resolution named, inside the
// caller's transaction.
//
// Its catalog side is one of two things. An accepted card links to the row
// that was shown, and writes nothing to the catalog: that row already
// describes the product. Anything else is described to the catalog now —
// insert-only, so a name another storage wrote first keeps its description —
// and linked to whichever row holds that name afterwards.
func createResolvedProduct(ctx context.Context, tx pgx.Tx, storageID uuid.UUID, in ResolvedProduct) (uuid.UUID, error) {
	categoryID := in.CategoryID
	if in.CategoryPath != nil {
		id, err := ensureCategoryPath(ctx, tx, storageID, *in.CategoryPath)
		if err != nil {
			return uuid.Nil, err
		}
		categoryID = id
	}

	var catalogID uuid.UUID
	if in.AcceptedCatalogID != nil {
		var exists bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM catalog_products WHERE id = $1)`, *in.AcceptedCatalogID).Scan(&exists); err != nil {
			return uuid.Nil, fmt.Errorf("store: check accepted catalog row: %w", err)
		}
		if !exists {
			return uuid.Nil, ErrNotFound
		}
		catalogID = *in.AcceptedCatalogID
	} else {
		entry := *in.Catalog
		if categoryID != nil && entry.CategoryPath == nil {
			path, err := categoryPathOf(ctx, tx, *categoryID)
			if err != nil {
				return uuid.Nil, err
			}
			entry.CategoryPath = &path
		}
		row, err := insertCatalogProduct(ctx, tx, entry)
		if err != nil {
			return uuid.Nil, err
		}
		catalogID = row.ID
	}

	product, err := createProduct(ctx, tx, storageID, NewProduct{
		Name:                 strings.TrimSpace(in.Name),
		CategoryID:           categoryID,
		CatalogID:            &catalogID,
		ItemType:             in.ItemType,
		DefaultShelfLifeDays: in.DefaultShelfLifeDays,
		MinStock:             in.MinStock,
		ImageURL:             in.ImageURL,
		IconName:             in.IconName,
	})
	if err != nil {
		return uuid.Nil, err
	}
	return product.ID, nil
}

// ensureCategoryPath resolves a catalog category path such as "Food > Dairy"
// against this storage's category tree, creating the nodes that are missing,
// and returns the leaf. A blank path files the product under no category.
//
// Names compare case-insensitively under the same parent, so "dairy" reuses an
// existing "Dairy" rather than growing a second one beside it.
func ensureCategoryPath(ctx context.Context, tx pgx.Tx, storageID uuid.UUID, path string) (*uuid.UUID, error) {
	var names []string
	for _, part := range strings.Split(path, ">") {
		if name := strings.TrimSpace(part); name != "" {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return nil, nil
	}
	if err := lockStorageTree(ctx, tx, storageID); err != nil {
		return nil, err
	}

	var parent *uuid.UUID
	for _, name := range names {
		var existing uuid.UUID
		err := tx.QueryRow(ctx, `
			SELECT id FROM categories
			 WHERE storage_id = $1 AND parent_id IS NOT DISTINCT FROM $2 AND lower(name) = lower($3)
			 ORDER BY created_at, id
			 LIMIT 1`, storageID, parent, name).Scan(&existing)
		if err == nil {
			parent = &existing
			continue
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("store: find category: %w", err)
		}

		id, err := newID()
		if err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO categories (id, storage_id, parent_id, name)
			VALUES ($1, $2, $3, $4)`, id, storageID, parent, name); err != nil {
			return nil, fmt.Errorf("store: create category: %w", err)
		}
		parent = &id
	}
	return parent, nil
}

func scanShoppingList(row rowScanner) (*ShoppingList, error) {
	var l ShoppingList
	var source string
	err := row.Scan(&l.ID, &l.StorageID, &source, &l.CreatedBy, &l.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: scan shopping list: %w", err)
	}
	l.Source = ShoppingListSource(source)
	return &l, nil
}

func scanShoppingListItem(row rowScanner) (*ShoppingListItem, error) {
	var i ShoppingListItem
	var status string
	err := row.Scan(&i.ID, &i.ShoppingListID, &i.RawText, &status,
		&i.MatchedProductID, &i.ResolvedQuantity, &i.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: scan shopping list item: %w", err)
	}
	i.Status = ShoppingListItemStatus(status)
	return &i, nil
}
