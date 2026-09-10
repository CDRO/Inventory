package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
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

// ResolveShoppingListItem marks one line resolved.
//
// It touches exactly one row. That is the acceptance criterion "resolving one
// item doesn't block or auto-resolve others" expressed in the only place it can
// be guaranteed: a statement that names a single id cannot cascade to a
// sibling, no matter what the calling handler does.
//
// Resolving is idempotent-hostile on purpose: a second resolve of the same line
// is ErrConflict rather than a silent overwrite, because the first one may have
// already written an inventory batch and the second would double it.
func (s *Store) ResolveShoppingListItem(ctx context.Context, storageID, itemID uuid.UUID, productID *uuid.UUID, quantity int) (*ShoppingListItem, error) {
	if quantity < 0 {
		return nil, fmt.Errorf("%w: resolved quantity cannot be negative", ErrValidation)
	}

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

		if productID != nil {
			if err := requireProductInStorage(ctx, tx, storageID, *productID); err != nil {
				return err
			}
		}

		row := tx.QueryRow(ctx, `
			UPDATE shopping_list_items
			   SET status = 'resolved', matched_product_id = $1, resolved_quantity = $2
			 WHERE id = $3
			RETURNING id, shopping_list_id, raw_text, status, matched_product_id, resolved_quantity, created_at`,
			productID, quantity, itemID)

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
