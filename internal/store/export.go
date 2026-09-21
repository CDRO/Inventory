package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// StorageExport is everything one storage holds, as
// docs/specs/15-backup-restore-and-export.md's member export carries it.
//
// The type exists so that the "nothing from any other storage" rule is decided
// in one place. Every query below is scoped by the same storage id, either
// directly or through a join back to products/locations of that storage, and
// there is no path by which a caller can widen it — the handler receives this
// struct and can only serialize what is in it.
//
// Two things are resolved rather than carried: a user id becomes a display
// name, because an id from this instance means nothing in an archive that
// outlives it; and stock is aggregated per product here rather than queried
// per product by the caller.
type StorageExport struct {
	Storage       Storage
	Locations     []Location
	Categories    []Category
	Products      []ExportProduct
	Batches       []Batch
	Logs          []ExportLog
	ShoppingLists []ExportShoppingList
}

// ExportProduct is a product as the export carries it, plus the stock it was
// taken at.
//
// It is its own struct rather than an embedded Product on purpose, and the
// missing field is the point: there is no CatalogID here. A catalog id names a
// row of the global, cross-household catalog (docs/specs/02-data-model.md) —
// the one identifier within reach that belongs to no storage at all, and the
// most plausible route for one household's data to appear in another's
// archive. Leaving it out here rather than only in the handler's JSON means
// the guarantee holds at the store boundary: nothing downstream can serialize
// what it was never given.
//
// ImageURL is kept because the handler needs it to find the picture's bytes on
// disk. It is never serialized — it is a path on this instance, which is
// exactly what an archive must not contain.
type ExportProduct struct {
	ID                   uuid.UUID
	Name                 string
	CategoryID           *uuid.UUID
	ItemType             ItemType
	DefaultShelfLifeDays *int
	MinStock             int
	ImageURL             *string
	IconName             *string
	CreatedAt            time.Time
	CurrentStock         int
}

// ExportLog is one inventory_logs row with created_by resolved.
//
// CreatedBy is nil for a row whose user has since been deleted: the column is
// ON DELETE SET NULL, so "someone who no longer has an account did this" is a
// normal state of the table and not a defect to paper over.
type ExportLog struct {
	ID        uuid.UUID
	ProductID uuid.UUID
	BatchID   *uuid.UUID
	ChangeQty int
	Reason    LogReason
	CreatedBy *string
	Timestamp time.Time
}

// ExportShoppingList is one list with its lines, created_by resolved the same
// way as a log row's.
type ExportShoppingList struct {
	ID        uuid.UUID
	Source    ShoppingListSource
	CreatedBy *string
	CreatedAt time.Time
	Items     []ShoppingListItem
}

// ExportStorage reads one storage's entire contents.
//
// Everything runs inside a single read-only, repeatable-read transaction. An
// export is eight queries over tables a household is actively writing to, and
// without one snapshot the archive could contain a batch whose product is
// missing, or a log row pointing at a batch that was split between two of the
// reads — an export that does not parse as a whole is worse than one taken a
// second later.
//
// An unknown storage is ErrNotFound, the same as a storage the caller may not
// see (see the comment on ErrNotFound) — though in practice the membership
// gate has already answered that question before this is called.
func (s *Store) ExportStorage(ctx context.Context, storageID uuid.UUID) (*StorageExport, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{
		IsoLevel:   pgx.RepeatableRead,
		AccessMode: pgx.ReadOnly,
	})
	if err != nil {
		return nil, fmt.Errorf("store: begin export: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	out := &StorageExport{}

	if err := tx.QueryRow(ctx,
		`SELECT id, name, created_at FROM storages WHERE id = $1`, storageID,
	).Scan(&out.Storage.ID, &out.Storage.Name, &out.Storage.CreatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("store: export storage: %w", err)
	}

	if out.Locations, err = exportLocations(ctx, tx, storageID); err != nil {
		return nil, err
	}
	if out.Categories, err = exportCategories(ctx, tx, storageID); err != nil {
		return nil, err
	}
	if out.Products, err = exportProducts(ctx, tx, storageID); err != nil {
		return nil, err
	}
	if out.Batches, err = exportBatches(ctx, tx, storageID); err != nil {
		return nil, err
	}
	if out.Logs, err = exportLogs(ctx, tx, storageID); err != nil {
		return nil, err
	}
	if out.ShoppingLists, err = exportShoppingLists(ctx, tx, storageID); err != nil {
		return nil, err
	}
	return out, nil
}

func exportLocations(ctx context.Context, q querier, storageID uuid.UUID) ([]Location, error) {
	rows, err := q.Query(ctx, `
		SELECT id, storage_id, parent_id, name, description, last_audited_at, created_at, updated_at
		  FROM locations
		 WHERE storage_id = $1
		 ORDER BY created_at, id`, storageID)
	if err != nil {
		return nil, fmt.Errorf("store: export locations: %w", err)
	}
	defer rows.Close()

	out := []Location{}
	for rows.Next() {
		var l Location
		if err := rows.Scan(&l.ID, &l.StorageID, &l.ParentID, &l.Name, &l.Description,
			&l.LastAuditedAt, &l.CreatedAt, &l.UpdatedAt); err != nil {
			return nil, fmt.Errorf("store: scan exported location: %w", err)
		}
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: export locations: %w", err)
	}
	return out, nil
}

func exportCategories(ctx context.Context, q querier, storageID uuid.UUID) ([]Category, error) {
	rows, err := q.Query(ctx, `
		SELECT id, storage_id, parent_id, name, default_shelf_life_days, created_at, updated_at
		  FROM categories
		 WHERE storage_id = $1
		 ORDER BY created_at, id`, storageID)
	if err != nil {
		return nil, fmt.Errorf("store: export categories: %w", err)
	}
	defer rows.Close()

	out := []Category{}
	for rows.Next() {
		var c Category
		if err := rows.Scan(&c.ID, &c.StorageID, &c.ParentID, &c.Name,
			&c.DefaultShelfLifeDays, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, fmt.Errorf("store: scan exported category: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: export categories: %w", err)
	}
	return out, nil
}

// exportProducts reads this storage's products with their stock.
//
// The stock is one aggregate over inventory_batches rather than a CurrentStock
// call per product: a household with a few hundred products would otherwise
// make a few hundred round trips for a single download.
//
// catalog_id is not selected at all, which is what keeps it out of the archive
// (see the comment on ExportProduct). What the catalog contributed is already
// denormalized onto the product's own name, item_type and icon_name, which do
// travel.
func exportProducts(ctx context.Context, q querier, storageID uuid.UUID) ([]ExportProduct, error) {
	rows, err := q.Query(ctx, `
		SELECT p.id, p.name, p.category_id, p.item_type,
		       p.default_shelf_life_days, p.min_stock, p.image_url, p.icon_name, p.created_at,
		       coalesce((SELECT sum(b.quantity) FROM inventory_batches b WHERE b.product_id = p.id), 0)
		  FROM products p
		 WHERE p.storage_id = $1
		 ORDER BY p.created_at, p.id`, storageID)
	if err != nil {
		return nil, fmt.Errorf("store: export products: %w", err)
	}
	defer rows.Close()

	out := []ExportProduct{}
	for rows.Next() {
		var p ExportProduct
		if err := rows.Scan(&p.ID, &p.Name, &p.CategoryID, &p.ItemType,
			&p.DefaultShelfLifeDays, &p.MinStock, &p.ImageURL, &p.IconName,
			&p.CreatedAt, &p.CurrentStock); err != nil {
			return nil, fmt.Errorf("store: scan exported product: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: export products: %w", err)
	}
	return out, nil
}

// exportBatches reads the batches of this storage's products.
//
// Scoped through products rather than through locations, because a batch's
// storage is the storage of its product; the two agree by the same-storage
// invariant, and going through products keeps this query's scoping identical
// to the products query above.
func exportBatches(ctx context.Context, q querier, storageID uuid.UUID) ([]Batch, error) {
	rows, err := q.Query(ctx, `
		SELECT b.id, b.product_id, b.location_id, b.quantity,
		       b.expiration_date, b.expiration_source, b.created_at
		  FROM inventory_batches b
		  JOIN products p ON p.id = b.product_id
		 WHERE p.storage_id = $1
		 ORDER BY b.created_at, b.id`, storageID)
	if err != nil {
		return nil, fmt.Errorf("store: export batches: %w", err)
	}
	defer rows.Close()

	out := []Batch{}
	for rows.Next() {
		var b Batch
		if err := rows.Scan(&b.ID, &b.ProductID, &b.LocationID, &b.Quantity,
			&b.ExpirationDate, &b.ExpirationSource, &b.CreatedAt); err != nil {
			return nil, fmt.Errorf("store: scan exported batch: %w", err)
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: export batches: %w", err)
	}
	return out, nil
}

// exportLogs reads the ledger of this storage's products, with created_by
// resolved to a display name.
//
// The join is what keeps a user id out of the archive. An id identifies a row
// in this instance's users table and nothing else; the person's name is the
// part that still means something when the export is opened years later on
// another machine.
func exportLogs(ctx context.Context, q querier, storageID uuid.UUID) ([]ExportLog, error) {
	rows, err := q.Query(ctx, `
		SELECT l.id, l.product_id, l.batch_id, l.change_qty, l.reason, u.display_name, l.timestamp
		  FROM inventory_logs l
		  JOIN products p ON p.id = l.product_id
		  LEFT JOIN users u ON u.id = l.created_by
		 WHERE p.storage_id = $1
		 ORDER BY l.timestamp, l.id`, storageID)
	if err != nil {
		return nil, fmt.Errorf("store: export logs: %w", err)
	}
	defer rows.Close()

	out := []ExportLog{}
	for rows.Next() {
		var l ExportLog
		if err := rows.Scan(&l.ID, &l.ProductID, &l.BatchID, &l.ChangeQty,
			&l.Reason, &l.CreatedBy, &l.Timestamp); err != nil {
			return nil, fmt.Errorf("store: scan exported log: %w", err)
		}
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: export logs: %w", err)
	}
	return out, nil
}

// exportShoppingLists reads this storage's lists with their lines.
//
// Two queries rather than one join, so that a list with no lines is still a
// list in the archive rather than vanishing.
func exportShoppingLists(ctx context.Context, q querier, storageID uuid.UUID) ([]ExportShoppingList, error) {
	rows, err := q.Query(ctx, `
		SELECT sl.id, sl.source, u.display_name, sl.created_at
		  FROM shopping_lists sl
		  LEFT JOIN users u ON u.id = sl.created_by
		 WHERE sl.storage_id = $1
		 ORDER BY sl.created_at, sl.id`, storageID)
	if err != nil {
		return nil, fmt.Errorf("store: export shopping lists: %w", err)
	}

	lists := []ExportShoppingList{}
	index := map[uuid.UUID]int{}
	for rows.Next() {
		var l ExportShoppingList
		if err := rows.Scan(&l.ID, &l.Source, &l.CreatedBy, &l.CreatedAt); err != nil {
			rows.Close()
			return nil, fmt.Errorf("store: scan exported shopping list: %w", err)
		}
		l.Items = []ShoppingListItem{}
		index[l.ID] = len(lists)
		lists = append(lists, l)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: export shopping lists: %w", err)
	}
	if len(lists) == 0 {
		return lists, nil
	}

	itemRows, err := q.Query(ctx, `
		SELECT i.id, i.shopping_list_id, i.raw_text, i.status, i.matched_product_id,
		       i.resolved_quantity, i.created_at
		  FROM shopping_list_items i
		  JOIN shopping_lists sl ON sl.id = i.shopping_list_id
		 WHERE sl.storage_id = $1
		 ORDER BY i.created_at, i.id`, storageID)
	if err != nil {
		return nil, fmt.Errorf("store: export shopping list items: %w", err)
	}
	defer itemRows.Close()

	for itemRows.Next() {
		var item ShoppingListItem
		if err := itemRows.Scan(&item.ID, &item.ShoppingListID, &item.RawText, &item.Status,
			&item.MatchedProductID, &item.ResolvedQuantity, &item.CreatedAt); err != nil {
			return nil, fmt.Errorf("store: scan exported shopping list item: %w", err)
		}
		// A line whose list is not in the slice cannot happen — both queries
		// are scoped to the same storage inside one snapshot — but dropping it
		// rather than indexing blindly keeps a future change to either query
		// from panicking here.
		if at, ok := index[item.ShoppingListID]; ok {
			lists[at].Items = append(lists[at].Items, item)
		}
	}
	if err := itemRows.Err(); err != nil {
		return nil, fmt.Errorf("store: export shopping list items: %w", err)
	}
	return lists, nil
}
