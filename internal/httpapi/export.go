package httpapi

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/CDRO/Inventory/internal/store"
	"github.com/CDRO/Inventory/internal/uploads"
)

// ExportFormat is the value of export.json's `format` field
// (docs/specs/15-backup-restore-and-export.md).
//
// The suffix is the contract with whatever reads an archive years from now:
// an additive change bumps it, so a reader can decide what it understands
// before it starts parsing. It is a constant here rather than a literal in the
// marshalling code so that changing the format is a deliberate edit to a
// documented name, not a string nobody notices.
const ExportFormat = "inventory-export/1"

// ExportStore is the slice of the store the export needs: one read that
// returns one storage's entire contents. *store.Store satisfies it.
type ExportStore interface {
	ExportStorage(ctx context.Context, storageID uuid.UUID) (*store.StorageExport, error)
}

// ExportHandler serves the member export of
// docs/specs/15-backup-restore-and-export.md: one storage as portable files,
// readable without this software.
type ExportHandler struct {
	store    ExportStore
	pictures PhotoStore
	errors   *ErrorWriter
}

// NewExportHandler wires the handler. pictures may be nil — an upload volume
// the server could not open — in which case the archive carries the JSON and
// no images, which is the honest result rather than a failed download.
func NewExportHandler(s ExportStore, pictures PhotoStore, errs *ErrorWriter) *ExportHandler {
	return &ExportHandler{store: s, pictures: pictures, errors: errs}
}

// The JSON shapes below are the archive's format, deliberately separate from
// the API's own DTOs even where the two currently agree.
//
// An archive is opened by something that is not this application, possibly
// long after it. If these were the API types, renaming a JSON key to suit the
// frontend would silently change a file format that promises to be stable
// under a version string — and nothing in the test suite would notice,
// because both sides would have moved together.

// exportedLocation is one node of the exported location tree. Trees are
// exported nested, like their GET endpoints.
type exportedLocation struct {
	ID          uuid.UUID           `json:"id"`
	Name        string              `json:"name"`
	Description *string             `json:"description"`
	CreatedAt   time.Time           `json:"created_at"`
	Children    []*exportedLocation `json:"children"`
}

// exportedCategory is one node of the exported category tree.
type exportedCategory struct {
	ID                   uuid.UUID           `json:"id"`
	Name                 string              `json:"name"`
	DefaultShelfLifeDays *int                `json:"default_shelf_life_days"`
	CreatedAt            time.Time           `json:"created_at"`
	Children             []*exportedCategory `json:"children"`
}

// exportedProduct is one product, with the stock the export was taken at.
//
// There is no image_url and no catalog_id. image_url holds a path on this
// instance — a product-images route, or a suggestion-cache hash — which is an
// internal file path by any reading and means nothing elsewhere; image_file
// below replaces it with a path inside the archive. catalog_id names a row of
// the global, cross-household catalog (docs/specs/02-data-model.md), the one
// identifier in reach that belongs to no storage at all; what the catalog
// contributed is already denormalized onto name, item_type and icon_name,
// which do travel.
type exportedProduct struct {
	ID                   uuid.UUID `json:"id"`
	Name                 string    `json:"name"`
	CategoryID           *string   `json:"category_id"`
	ItemType             string    `json:"item_type"`
	DefaultShelfLifeDays *int      `json:"default_shelf_life_days"`
	MinStock             int       `json:"min_stock"`
	CurrentStock         int       `json:"current_stock"`
	IconName             *string   `json:"icon_name"`
	// ImageFile is a path relative to the archive root, or null. It is present
	// only when the bytes are actually in this archive, so a reader never has
	// to handle a reference to a file that is not there.
	ImageFile *string   `json:"image_file"`
	CreatedAt time.Time `json:"created_at"`
}

// exportedBatch is one quantity of one product at one location.
type exportedBatch struct {
	ID         uuid.UUID `json:"id"`
	ProductID  uuid.UUID `json:"product_id"`
	LocationID uuid.UUID `json:"location_id"`
	Quantity   int       `json:"quantity"`
	// ExpirationDate is a plain calendar date, not a timestamp: the column is
	// a DATE, and rendering it as an instant would invite a reader to apply a
	// timezone to a day that never had one.
	ExpirationDate   *string   `json:"expiration_date"`
	ExpirationSource string    `json:"expiration_source"`
	CreatedAt        time.Time `json:"created_at"`
}

// exportedLog is one ledger row. created_by is the person's display name, not
// their id: an id identifies a row in this instance's users table and nothing
// else.
type exportedLog struct {
	ID        uuid.UUID  `json:"id"`
	ProductID uuid.UUID  `json:"product_id"`
	BatchID   *uuid.UUID `json:"batch_id"`
	ChangeQty int        `json:"change_qty"`
	Reason    string     `json:"reason"`
	CreatedBy *string    `json:"created_by"`
	Timestamp time.Time  `json:"timestamp"`
}

// exportedShoppingListItem is one line of a list.
type exportedShoppingListItem struct {
	ID               uuid.UUID  `json:"id"`
	RawText          string     `json:"raw_text"`
	Status           string     `json:"status"`
	MatchedProductID *uuid.UUID `json:"matched_product_id"`
	ResolvedQuantity *int       `json:"resolved_quantity"`
	CreatedAt        time.Time  `json:"created_at"`
}

// exportedShoppingList is one submitted list with its lines.
type exportedShoppingList struct {
	ID        uuid.UUID                  `json:"id"`
	Source    string                     `json:"source"`
	CreatedBy *string                    `json:"created_by"`
	CreatedAt time.Time                  `json:"created_at"`
	Items     []exportedShoppingListItem `json:"items"`
}

// exportDocument is export.json.
type exportDocument struct {
	Format     string    `json:"format"`
	ExportedAt time.Time `json:"exported_at"`
	Storage    struct {
		Name string `json:"name"`
	} `json:"storage"`
	Locations     []*exportedLocation    `json:"locations"`
	Categories    []*exportedCategory    `json:"categories"`
	Products      []exportedProduct      `json:"products"`
	Batches       []exportedBatch        `json:"batches"`
	Logs          []exportedLog          `json:"logs"`
	ShoppingLists []exportedShoppingList `json:"shopping_lists"`
}

// exportImage is one picture on its way into the archive.
type exportImage struct {
	name string
	data []byte
}

// Export serves GET /api/storages/{storage_id}/export.
//
// Any member may export — rights inside a storage are flat
// (docs/specs/03-auth-and-multi-tenancy.md) — and a non-member never reaches
// here at all: the route sits behind RequireStorageMember with every other
// storage-scoped route, so an outsider gets the same 404 as for a storage that
// does not exist.
//
// Everything is read and assembled before the first byte of the response is
// written. Once archive/zip has emitted a header the status line is long gone,
// so a store error discovered half way through would become a 200 carrying a
// truncated archive — a corrupt file that looks like a successful download,
// which is the worst possible outcome for the one artifact whose whole purpose
// is to still be readable later.
func (h *ExportHandler) Export(w http.ResponseWriter, r *http.Request) {
	storageID, ok := StorageIDFrom(r.Context())
	if !ok {
		h.errors.WriteError(w, r, Internal(errNoStorageInContext))
		return
	}

	data, err := h.store.ExportStorage(r.Context(), storageID)
	if err != nil {
		h.errors.WriteError(w, r, FromStoreError(err, "export storage"))
		return
	}

	images, failure := h.collectImages(storageID, data.Products)
	if failure != nil {
		h.errors.WriteError(w, r, failure)
		return
	}
	document := buildExportDocument(data, images, time.Now().UTC())

	body, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		h.errors.WriteError(w, r, Internal(fmt.Errorf("render export.json: %w", err)))
		return
	}

	filename := exportFilename(data.Storage.Name, time.Now().UTC())
	header := w.Header()
	header.Set("Content-Type", "application/zip")
	header.Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
	// Never a cached copy of a household's entire inventory, anywhere.
	header.Set("Cache-Control", "no-store")
	header.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)

	// From here on the response is committed. A write failure means the client
	// hung up mid-download and there is nothing left to report.
	archive := zip.NewWriter(w)
	if err := writeZipEntry(archive, "export.json", body); err != nil {
		return
	}
	for _, image := range images {
		if err := writeZipEntry(archive, image.name, image.data); err != nil {
			return
		}
	}
	_ = archive.Close()
}

// writeZipEntry adds one file to the archive.
func writeZipEntry(archive *zip.Writer, name string, data []byte) error {
	entry, err := archive.Create(name)
	if err != nil {
		return err
	}
	_, err = entry.Write(data)
	return err
}

// collectImages loads the permanent product pictures this storage's products
// reference, before anything is written.
//
// A product's picture is included only when its image_url has the exact shape
// this server generates for a product image *of this storage*
// (productImageURL). A row can also carry a provider URL from the suggestion
// flow or a catalog entry; those name bytes that live somewhere else entirely
// and are not this storage's to hand out, so they are skipped rather than
// fetched.
//
// A file that is missing or whose name is not one the server would generate is
// skipped, not an error: a picture lost to a half-finished eviction must not
// cost the member their entire export. The product simply carries a null
// image_file, so the archive stays self-consistent.
func (h *ExportHandler) collectImages(storageID uuid.UUID, products []store.ExportProduct) (map[uuid.UUID]exportImage, *Failure) {
	images := map[uuid.UUID]exportImage{}
	if h.pictures == nil {
		return images, nil
	}

	prefix := productImageURL(storageID, "")
	for _, product := range products {
		if product.ImageURL == nil {
			continue
		}
		name, found := strings.CutPrefix(*product.ImageURL, prefix)
		if !found || name == "" || strings.ContainsRune(name, '/') {
			continue
		}

		data, err := h.pictures.Read(name)
		switch {
		case errors.Is(err, uploads.ErrInvalidName), errors.Is(err, os.ErrNotExist):
			continue
		case err != nil:
			// A broken upload volume is not a partial export. Better a 500
			// than an archive that quietly omits every picture.
			return nil, Internal(fmt.Errorf("read product image for export: %w", err))
		}

		// Named by product, so export.json can reference it by relative path
		// and a human unpacking the archive can tell what they are looking at
		// without consulting the JSON. The extension comes from the stored
		// name, which uploads.Dir has already constrained to .jpg/.png/.svg.
		images[product.ID] = exportImage{
			name: "images/" + product.ID.String() + path.Ext(name),
			data: data,
		}
	}
	return images, nil
}

// buildExportDocument turns the store's rows into the archive's own shape.
func buildExportDocument(data *store.StorageExport, images map[uuid.UUID]exportImage, now time.Time) exportDocument {
	doc := exportDocument{
		Format:        ExportFormat,
		ExportedAt:    now,
		Locations:     nestExportedLocations(data.Locations),
		Categories:    nestExportedCategories(data.Categories),
		Products:      make([]exportedProduct, 0, len(data.Products)),
		Batches:       make([]exportedBatch, 0, len(data.Batches)),
		Logs:          make([]exportedLog, 0, len(data.Logs)),
		ShoppingLists: make([]exportedShoppingList, 0, len(data.ShoppingLists)),
	}
	doc.Storage.Name = data.Storage.Name

	for _, product := range data.Products {
		var categoryID *string
		if product.CategoryID != nil {
			id := product.CategoryID.String()
			categoryID = &id
		}
		var imageFile *string
		if image, ok := images[product.ID]; ok {
			name := image.name
			imageFile = &name
		}
		doc.Products = append(doc.Products, exportedProduct{
			ID:                   product.ID,
			Name:                 product.Name,
			CategoryID:           categoryID,
			ItemType:             string(product.ItemType),
			DefaultShelfLifeDays: product.DefaultShelfLifeDays,
			MinStock:             product.MinStock,
			CurrentStock:         product.CurrentStock,
			IconName:             product.IconName,
			ImageFile:            imageFile,
			CreatedAt:            product.CreatedAt,
		})
	}

	for _, batch := range data.Batches {
		var expires *string
		if batch.ExpirationDate != nil {
			day := batch.ExpirationDate.Format(time.DateOnly)
			expires = &day
		}
		doc.Batches = append(doc.Batches, exportedBatch{
			ID:               batch.ID,
			ProductID:        batch.ProductID,
			LocationID:       batch.LocationID,
			Quantity:         batch.Quantity,
			ExpirationDate:   expires,
			ExpirationSource: string(batch.ExpirationSource),
			CreatedAt:        batch.CreatedAt,
		})
	}

	for _, row := range data.Logs {
		doc.Logs = append(doc.Logs, exportedLog{
			ID:        row.ID,
			ProductID: row.ProductID,
			BatchID:   row.BatchID,
			ChangeQty: row.ChangeQty,
			Reason:    string(row.Reason),
			CreatedBy: row.CreatedBy,
			Timestamp: row.Timestamp,
		})
	}

	for _, list := range data.ShoppingLists {
		items := make([]exportedShoppingListItem, 0, len(list.Items))
		for _, item := range list.Items {
			items = append(items, exportedShoppingListItem{
				ID:               item.ID,
				RawText:          item.RawText,
				Status:           string(item.Status),
				MatchedProductID: item.MatchedProductID,
				ResolvedQuantity: item.ResolvedQuantity,
				CreatedAt:        item.CreatedAt,
			})
		}
		doc.ShoppingLists = append(doc.ShoppingLists, exportedShoppingList{
			ID:        list.ID,
			Source:    string(list.Source),
			CreatedBy: list.CreatedBy,
			CreatedAt: list.CreatedAt,
			Items:     items,
		})
	}

	return doc
}

// nestExportedLocations builds the location tree.
//
// Parents are linked in a second pass rather than trusted to arrive first, for
// the same reason nestLocations does it: the rows are ordered by creation, and
// re-parenting an old node under a newer one puts a child ahead of its parent.
// A node whose parent is missing from the row set is kept as a root rather
// than dropped — an export that silently loses a subtree is worse than one
// with a flattened edge.
func nestExportedLocations(rows []store.Location) []*exportedLocation {
	byID := make(map[uuid.UUID]*exportedLocation, len(rows))
	for _, row := range rows {
		byID[row.ID] = &exportedLocation{
			ID: row.ID, Name: row.Name, Description: row.Description,
			CreatedAt: row.CreatedAt, Children: []*exportedLocation{},
		}
	}

	roots := make([]*exportedLocation, 0, len(rows))
	for _, row := range rows {
		node := byID[row.ID]
		if row.ParentID == nil {
			roots = append(roots, node)
			continue
		}
		parent, ok := byID[*row.ParentID]
		if !ok {
			roots = append(roots, node)
			continue
		}
		parent.Children = append(parent.Children, node)
	}
	return roots
}

// nestExportedCategories is nestExportedLocations for the category tree.
func nestExportedCategories(rows []store.Category) []*exportedCategory {
	byID := make(map[uuid.UUID]*exportedCategory, len(rows))
	for _, row := range rows {
		byID[row.ID] = &exportedCategory{
			ID: row.ID, Name: row.Name, DefaultShelfLifeDays: row.DefaultShelfLifeDays,
			CreatedAt: row.CreatedAt, Children: []*exportedCategory{},
		}
	}

	roots := make([]*exportedCategory, 0, len(rows))
	for _, row := range rows {
		node := byID[row.ID]
		if row.ParentID == nil {
			roots = append(roots, node)
			continue
		}
		parent, ok := byID[*row.ParentID]
		if !ok {
			roots = append(roots, node)
			continue
		}
		parent.Children = append(parent.Children, node)
	}
	return roots
}

// exportFilename is the download's name:
// inventory-export-<storagename-slug>-<YYYY-MM-DD>.zip.
func exportFilename(storageName string, now time.Time) string {
	return fmt.Sprintf("inventory-export-%s-%s.zip", slugify(storageName), now.Format(time.DateOnly))
}

// slugify reduces a storage name to something safe in a filename.
//
// Only ASCII letters and digits survive; everything else becomes a single
// dash. That is deliberately harsher than it needs to be for most names: the
// result goes into a Content-Disposition header, where a quote or a newline
// would let the name decide the shape of the header rather than its value, and
// a storage name is free text somebody typed.
func slugify(name string) string {
	var b strings.Builder
	dashed := false
	for _, r := range strings.ToLower(name) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			dashed = false
		case !dashed && b.Len() > 0:
			b.WriteByte('-')
			dashed = true
		}
	}
	slug := strings.Trim(b.String(), "-")
	if slug == "" {
		// A name made entirely of characters that did not survive — "厨房",
		// say. The date still distinguishes one download from the next.
		return "storage"
	}
	const maxSlug = 60
	if len(slug) > maxSlug {
		slug = strings.Trim(slug[:maxSlug], "-")
	}
	return slug
}
