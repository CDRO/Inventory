package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/CDRO/Inventory/internal/store"
)

// ProductStore is the slice of the store the product routes read and write.
type ProductStore interface {
	ListProducts(ctx context.Context, storageID uuid.UUID) ([]store.Product, error)
	ProductsChangedSince(ctx context.Context, storageID uuid.UUID, since time.Time) (*store.Delta[store.Product], error)
	ListProductBatches(ctx context.Context, storageID, productID uuid.UUID) ([]store.Batch, error)
	SetProductCategoryAsUser(ctx context.Context, storageID, id uuid.UUID, categoryID *uuid.UUID, userID uuid.UUID) error
	SetProductImageAsUser(ctx context.Context, storageID, id uuid.UUID, imageURL, iconName *string, userID uuid.UUID) error

	// The maintenance surface of docs/specs/16-product-maintenance.md.
	GetProduct(ctx context.Context, storageID, id uuid.UUID) (*store.Product, error)
	CurrentStock(ctx context.Context, storageID, productID uuid.UUID) (int, error)
	ListProductLogs(ctx context.Context, storageID, productID uuid.UUID, limit int) ([]store.ProductLog, error)
	UpdateProduct(ctx context.Context, storageID, id uuid.UUID, patch store.ProductPatch) (*store.Product, int, error)
	MergeProducts(ctx context.Context, storageID, survivorID, sourceID uuid.UUID) (*store.MergeResult, error)
	DeleteProduct(ctx context.Context, storageID, id uuid.UUID) (string, error)
}

// ProductHandler serves the two read-only product routes
// docs/specs/09-consumption-logging.md needs — naming an existing product to
// correct a row to, and listing the batches a decrement can be split across —
// the two narrow write routes docs/specs/52-gamification-quests-and-ui.md
// needs so its "uncategorized" and "imageless" quests have something a user
// can actually do to close them, and the maintenance surface of
// docs/specs/16-product-maintenance.md: the detail read behind products.html,
// the full PATCH, the merge, and the delete.
type ProductHandler struct {
	store    ProductStore
	pictures productPictures
	errors   *ErrorWriter
}

// NewProductHandler wires the product routes. cache and productImages may be
// nil, in which case setting a picture is refused and setting an icon works.
func NewProductHandler(s ProductStore, cache ImageCache, productImages PhotoStore, errs *ErrorWriter) *ProductHandler {
	return &ProductHandler{store: s, errors: errs, pictures: productPictures{images: productImages, cache: cache}}
}

// productResponse is id and name only — everything the manual-correction
// picker needs and nothing else. category_id and catalog_id are
// deliberately absent, the same restraint batchResponse and every other
// handler in this package applies to its store type.
type productResponse struct {
	ID   uuid.UUID `json:"id"`
	Name string    `json:"name"`
}

// List serves GET /api/storages/{storage_id}/products.
//
// With ?updated_since=<RFC3339> it answers a delta instead: the products
// changed since that instant, the ids of those deleted, and the cursor for
// next time (docs/specs/12-client-api-contract.md). Without the parameter the
// response is exactly what it has always been.
func (h *ProductHandler) List(w http.ResponseWriter, r *http.Request) {
	storageID, ok := StorageIDFrom(r.Context())
	if !ok {
		h.errors.WriteError(w, r, Internal(errNoStorageInContext))
		return
	}

	since, failure := readDeltaSince(r)
	if failure != nil {
		h.errors.WriteError(w, r, failure)
		return
	}
	if since != nil {
		h.listDelta(w, r, storageID, *since)
		return
	}

	products, err := h.store.ListProducts(r.Context(), storageID)
	if err != nil {
		h.errors.WriteError(w, r, FromStoreError(err, "list products"))
		return
	}

	out := make([]productResponse, 0, len(products))
	for _, p := range products {
		out = append(out, productResponse{ID: p.ID, Name: p.Name})
	}
	writeJSON(w, http.StatusOK, collection[productResponse]{Items: out})
}

// listDelta answers the delta form of List.
//
// The item shape is the full list's item shape, unchanged: a delta is the same
// collection narrowed to what moved, so a client parses one response type for
// both and a field added to the list is a field added to the delta for free.
func (h *ProductHandler) listDelta(w http.ResponseWriter, r *http.Request, storageID uuid.UUID, since time.Time) {
	delta, err := h.store.ProductsChangedSince(r.Context(), storageID, since)
	if err != nil {
		h.errors.WriteError(w, r, FromStoreError(err, "list product delta"))
		return
	}

	out := make([]productResponse, 0, len(delta.Changed))
	for _, p := range delta.Changed {
		out = append(out, productResponse{ID: p.ID, Name: p.Name})
	}
	writeJSON(w, http.StatusOK, deltaCollection[productResponse]{
		Items: out, Deleted: delta.Deleted, SyncedAt: delta.SyncedAt,
	})
}

// Batches serves GET /api/storages/{storage_id}/products/{product_id}/batches
// — the batch picker docs/specs/09-consumption-logging.md needs once a row's
// product is known, ordered nearest-expiry first, the default first-out
// batch.
func (h *ProductHandler) Batches(w http.ResponseWriter, r *http.Request) {
	storageID, ok := StorageIDFrom(r.Context())
	if !ok {
		h.errors.WriteError(w, r, Internal(errNoStorageInContext))
		return
	}
	productID, failure := idFromPath(r, "product_id", "malformed product id")
	if failure != nil {
		h.errors.WriteError(w, r, failure)
		return
	}

	batches, err := h.store.ListProductBatches(r.Context(), storageID, productID)
	if err != nil {
		h.errors.WriteError(w, r, FromStoreError(err, "product not found in storage"))
		return
	}

	out := make([]batchResponse, 0, len(batches))
	for _, b := range batches {
		out = append(out, newBatchResponse(b))
	}
	writeJSON(w, http.StatusOK, collection[batchResponse]{Items: out})
}

// SetCategory serves PATCH /api/storages/{storage_id}/products/{product_id}/category
// — the write that lets a person close the "uncategorized" quest
// (docs/specs/52-gamification-quests-and-ui.md) on an existing product.
func (h *ProductHandler) SetCategory(w http.ResponseWriter, r *http.Request) {
	storageID, ok := StorageIDFrom(r.Context())
	if !ok {
		h.errors.WriteError(w, r, Internal(errNoStorageInContext))
		return
	}
	user, ok := UserFrom(r.Context())
	if !ok {
		h.errors.WriteError(w, r, Unauthorized(ReasonSessionMissing))
		return
	}
	productID, failure := idFromPath(r, "product_id", "malformed product id")
	if failure != nil {
		h.errors.WriteError(w, r, failure)
		return
	}

	var body struct {
		CategoryID *string `json:"category_id"`
	}
	if failure := decodeJSON(w, r, &body); failure != nil {
		h.errors.WriteError(w, r, failure)
		return
	}

	var categoryID *uuid.UUID
	if body.CategoryID != nil {
		parsed, err := uuid.Parse(*body.CategoryID)
		if err != nil {
			h.errors.WriteError(w, r, ValidationFailed(map[string][]string{
				"category_id": {"Must be a UUID or null."},
			}, nil))
			return
		}
		categoryID = &parsed
	}

	if err := h.store.SetProductCategoryAsUser(r.Context(), storageID, productID, categoryID, user.ID); err != nil {
		h.errors.WriteError(w, r, FromStoreError(err, "product or category not found in storage"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"category_id": body.CategoryID})
}

// SetImage serves PATCH /api/storages/{storage_id}/products/{product_id}/image
// — the write that lets a person close the "imageless" quest
// (docs/specs/52-gamification-quests-and-ui.md) on an existing product.
//
// The body is `{"image": "<suggestion hash>"}` or `{"icon_name": "…"}`; both
// null clears the picture. A picture is taken by the hash of a picked image
// suggestion and promoted into permanent storage, never by a URL: a
// suggestion-cache URL recorded on a product could be evicted from under it,
// and any other URL would let a caller point a household's product at a
// server of their choosing (docs/specs/07-shopping-list-reconciliation.md).
func (h *ProductHandler) SetImage(w http.ResponseWriter, r *http.Request) {
	storageID, ok := StorageIDFrom(r.Context())
	if !ok {
		h.errors.WriteError(w, r, Internal(errNoStorageInContext))
		return
	}
	user, ok := UserFrom(r.Context())
	if !ok {
		h.errors.WriteError(w, r, Unauthorized(ReasonSessionMissing))
		return
	}
	productID, failure := idFromPath(r, "product_id", "malformed product id")
	if failure != nil {
		h.errors.WriteError(w, r, failure)
		return
	}

	var body struct {
		Image    *string `json:"image"`
		IconName *string `json:"icon_name"`
	}
	if failure := decodeJSON(w, r, &body); failure != nil {
		h.errors.WriteError(w, r, failure)
		return
	}

	var picture string
	var imageURL *string
	if body.Image != nil {
		name, url, _, err := h.pictures.promoteSuggestion(r.Context(), storageID, *body.Image)
		switch {
		case errors.Is(err, errPictureUnavailable):
			h.errors.WriteError(w, r, ValidationFailed(map[string][]string{
				"image": {"That picture is no longer available. Pick another."},
			}, err))
			return
		case err != nil:
			h.errors.WriteError(w, r, Internal(err))
			return
		}
		picture, imageURL = name, &url
	}

	if err := h.store.SetProductImageAsUser(r.Context(), storageID, productID, imageURL, body.IconName, user.ID); err != nil {
		h.pictures.remove(r.Context(), h.errors, picture)
		h.errors.WriteError(w, r, FromStoreError(err, "product not found in storage"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"image_url": imageURL, "icon_name": body.IconName})
}

// productDetail is the full product the edit screen of
// docs/specs/16-product-maintenance.md reads: every field its PATCH can
// change, plus the current stock, the batches behind it, and the recent
// ledger.
//
// catalog_id stays absent, as it is from every other response in this package:
// it is server-side only and must never appear in an API response
// (docs/specs/02-data-model.md). storage_id is absent for the reason
// locationNode omits it — the scoping happens once, in the gate and the query,
// so there is no per-row copy a bug could get wrong.
type productDetail struct {
	ID                   uuid.UUID         `json:"id"`
	Name                 string            `json:"name"`
	CategoryID           *uuid.UUID        `json:"category_id"`
	ItemType             string            `json:"item_type"`
	DefaultShelfLifeDays *int              `json:"default_shelf_life_days"`
	MinStock             int               `json:"min_stock"`
	ImageURL             *string           `json:"image_url"`
	IconName             *string           `json:"icon_name"`
	CurrentStock         int               `json:"current_stock"`
	Batches              []batchResponse   `json:"batches"`
	Logs                 []productLogEntry `json:"logs"`
	UpdatedAt            time.Time         `json:"updated_at"`

	// MovedBatches and RecomputedBatches are what a write did, reported on the
	// same object rather than wrapped around it — one response shape for the
	// read, the patch and the merge.
	//
	// Both are pointers with omitempty so they appear only where
	// docs/specs/16-product-maintenance.md says they should: a
	// default_shelf_life_days change reports its count, because it is a change
	// made *in order to* move dates, while re-filing into another category
	// runs the same cascade silently, as spec 08 already specifies. A plain
	// int would swallow a legitimate zero and make "no field" and "nothing
	// moved" the same answer.
	MovedBatches      *int `json:"moved_batches,omitempty"`
	RecomputedBatches *int `json:"recomputed_batches,omitempty"`
}

// productCounts are the numbers a write reports about itself. Its zero value
// reports nothing, which is what a plain read and a silent cascade both want.
type productCounts struct {
	Moved      *int
	Recomputed *int
}

// productLogEntry is one ledger row on the detail view.
//
// created_by is a display name rather than a user id, and null for a row whose
// user has since been deleted — the account is gone, the history is not.
type productLogEntry struct {
	ID        uuid.UUID  `json:"id"`
	BatchID   *uuid.UUID `json:"batch_id"`
	ChangeQty int        `json:"change_qty"`
	Reason    string     `json:"reason"`
	CreatedBy *string    `json:"created_by"`
	Timestamp time.Time  `json:"timestamp"`
}

// productLogLimit caps the ledger the detail view shows.
//
// "Recent" in docs/specs/16-product-maintenance.md, not "all": a product that
// has been consumed from weekly for two years has hundreds of rows, and a
// detail view is not the place to page through them. The full ledger leaves
// the system through the export of docs/specs/15-backup-restore-and-export.md.
const productLogLimit = 50

// Get serves GET /api/storages/{storage_id}/products/{product_id} — the detail
// read behind the edit screen of docs/specs/16-product-maintenance.md.
//
// Membership is the whole of its access control, as for every route on this
// sub-router: a product of another storage, and one that does not exist, are
// the same 404. 200 with the product on success.
//
// The list route stays as it is, id and name only. Widening it would widen the
// delta item shape with it (docs/specs/12-client-api-contract.md), and paying
// for a product's batches and ledger once per row of a list nobody asked to be
// detailed is the wrong trade.
func (h *ProductHandler) Get(w http.ResponseWriter, r *http.Request) {
	storageID, ok := StorageIDFrom(r.Context())
	if !ok {
		h.errors.WriteError(w, r, Internal(errNoStorageInContext))
		return
	}
	productID, failure := idFromPath(r, "product_id", "malformed product id")
	if failure != nil {
		h.errors.WriteError(w, r, failure)
		return
	}

	detail, failure := h.detail(r, storageID, productID, productCounts{})
	if failure != nil {
		h.errors.WriteError(w, r, failure)
		return
	}
	writeJSON(w, http.StatusOK, detail)
}

// detail assembles one product with its stock, batches and recent ledger.
//
// counts carry through to the response only when the caller has numbers worth
// reporting, which is what keeps a silent cascade silent.
func (h *ProductHandler) detail(r *http.Request, storageID, productID uuid.UUID, counts productCounts) (*productDetail, *Failure) {
	product, err := h.store.GetProduct(r.Context(), storageID, productID)
	if err != nil {
		return nil, FromStoreError(err, "product not found in storage")
	}
	stock, err := h.store.CurrentStock(r.Context(), storageID, productID)
	if err != nil {
		return nil, Internal(err)
	}
	batches, err := h.store.ListProductBatches(r.Context(), storageID, productID)
	if err != nil {
		return nil, FromStoreError(err, "product not found in storage")
	}
	logs, err := h.store.ListProductLogs(r.Context(), storageID, productID, productLogLimit)
	if err != nil {
		return nil, FromStoreError(err, "product not found in storage")
	}

	out := &productDetail{
		ID:                   product.ID,
		Name:                 product.Name,
		CategoryID:           product.CategoryID,
		ItemType:             string(product.ItemType),
		DefaultShelfLifeDays: product.DefaultShelfLifeDays,
		MinStock:             product.MinStock,
		ImageURL:             product.ImageURL,
		IconName:             product.IconName,
		CurrentStock:         stock,
		Batches:              make([]batchResponse, 0, len(batches)),
		Logs:                 make([]productLogEntry, 0, len(logs)),
		UpdatedAt:            product.UpdatedAt,
		MovedBatches:         counts.Moved,
		RecomputedBatches:    counts.Recomputed,
	}
	for _, b := range batches {
		out.Batches = append(out.Batches, newBatchResponse(b))
	}
	for _, l := range logs {
		out.Logs = append(out.Logs, productLogEntry{
			ID:        l.ID,
			BatchID:   l.BatchID,
			ChangeQty: l.ChangeQty,
			Reason:    string(l.Reason),
			CreatedBy: l.CreatedBy,
			Timestamp: l.Timestamp,
		})
	}
	return out, nil
}

// Update serves PATCH /api/storages/{storage_id}/products/{product_id} — the
// edit surface of docs/specs/16-product-maintenance.md.
//
// Body: any of name, category_id, item_type, min_stock,
// default_shelf_life_days and icon_name, individually or together. An **unknown
// field is a 422**, not an ignored key, because a client that sent one
// believes a change was made. category_id, default_shelf_life_days and
// icon_name each accept an explicit null, which clears them; omitting a field
// leaves it alone.
//
// 200 with the product on success. 404 for a product — or a category — not in
// this storage, indistinguishable from one that does not exist. 422 for an
// unknown field, a malformed body, or any value the model would refuse: the
// three item types, min_stock below zero, an empty or over-long name and a
// shelf life outside 0–36500 are all checked here rather than left to a
// database CHECK, which would surface as a 500 the caller cannot act on.
//
// A default_shelf_life_days change reports recomputed_batches; a category
// change runs the identical cascade and says nothing, which is the line
// docs/specs/16-product-maintenance.md draws.
func (h *ProductHandler) Update(w http.ResponseWriter, r *http.Request) {
	storageID, ok := StorageIDFrom(r.Context())
	if !ok {
		h.errors.WriteError(w, r, Internal(errNoStorageInContext))
		return
	}
	productID, failure := idFromPath(r, "product_id", "malformed product id")
	if failure != nil {
		h.errors.WriteError(w, r, failure)
		return
	}

	// RawMessage throughout, so an absent field is distinguishable from an
	// explicit null — the difference between "leave the category alone" and
	// "this product has no category", which a plain pointer cannot express.
	var body struct {
		Name                 *string         `json:"name"`
		CategoryID           json.RawMessage `json:"category_id"`
		ItemType             *string         `json:"item_type"`
		MinStock             *int            `json:"min_stock"`
		DefaultShelfLifeDays json.RawMessage `json:"default_shelf_life_days"`
		IconName             json.RawMessage `json:"icon_name"`
	}
	if failure := decodeJSONStrict(w, r, &body); failure != nil {
		h.errors.WriteError(w, r, failure)
		return
	}

	fields := map[string][]string{}
	patch := store.ProductPatch{}

	if body.Name != nil {
		name := strings.TrimSpace(*body.Name)
		switch {
		case name == "":
			fields["name"] = append(fields["name"], "Give the product a name.")
		case len(name) > maxProductNameLength:
			fields["name"] = append(fields["name"], "Keep the name under 255 characters.")
		}
		patch.Name = &name
	}

	if body.CategoryID != nil {
		patch.SetCategoryID = true
		var raw *string
		if err := json.Unmarshal(body.CategoryID, &raw); err != nil {
			fields["category_id"] = append(fields["category_id"], "Must be a UUID or null.")
		} else if raw != nil {
			parsed, err := uuid.Parse(*raw)
			if err != nil {
				// 422, not 404: it says only that the input was malformed. A
				// well-formed id belonging to another storage is the 404,
				// decided by the store.
				fields["category_id"] = append(fields["category_id"], "Must be a UUID or null.")
			} else {
				patch.CategoryID = &parsed
			}
		}
	}

	if body.ItemType != nil {
		itemType := store.ItemType(*body.ItemType)
		switch itemType {
		case store.ItemPerishable, store.ItemLongShelfLife, store.ItemNonPerishable:
			patch.ItemType = &itemType
		default:
			fields["item_type"] = append(fields["item_type"],
				`Must be "perishable", "long_shelf_life" or "non_perishable".`)
		}
	}

	if body.MinStock != nil {
		if *body.MinStock < 0 {
			fields["min_stock"] = append(fields["min_stock"], "Cannot be negative.")
		}
		patch.MinStock = body.MinStock
	}

	if body.DefaultShelfLifeDays != nil {
		patch.SetDefaultShelfLifeDays = true
		// The same parser the category rule uses (expiry.go), so the bounds a
		// product override is held to and the bounds a category rule is held
		// to cannot drift apart.
		days, failure := parseShelfLifeDays(body.DefaultShelfLifeDays)
		if failure != nil {
			for field, messages := range failure.Fields {
				fields[field] = append(fields[field], messages...)
			}
		}
		patch.DefaultShelfLifeDays = days
	}

	if body.IconName != nil {
		patch.SetIconName = true
		var raw *string
		if err := json.Unmarshal(body.IconName, &raw); err != nil {
			fields["icon_name"] = append(fields["icon_name"], "Must be a string or null.")
		} else if raw != nil {
			name := strings.TrimSpace(*raw)
			if name == "" {
				// Cleared by null, not by an empty string: two spellings of
				// "no icon" would be two states the client has to reconcile.
				fields["icon_name"] = append(fields["icon_name"], "Send null to clear the icon.")
			} else if len(name) > maxIconNameLength {
				fields["icon_name"] = append(fields["icon_name"], "Keep the icon name under 100 characters.")
			}
			patch.IconName = &name
		}
	}

	if len(fields) > 0 {
		h.errors.WriteError(w, r, ValidationFailed(fields, nil))
		return
	}

	_, recomputed, err := h.store.UpdateProduct(r.Context(), storageID, productID, patch)
	if err != nil {
		h.errors.WriteError(w, r, FromStoreError(err, "product or category not found in storage"))
		return
	}

	var counts productCounts
	if patch.SetDefaultShelfLifeDays {
		counts.Recomputed = &recomputed
	}
	detail, failure := h.detail(r, storageID, productID, counts)
	if failure != nil {
		h.errors.WriteError(w, r, failure)
		return
	}
	writeJSON(w, http.StatusOK, detail)
}

// maxProductNameLength and maxIconNameLength mirror the VARCHAR widths in
// migrations/00002_core_schema.sql.
//
// They are checked here so an over-long value is the 422 it is rather than the
// 500 a database-level truncation error would become. The column is still the
// authority; this is the caller-facing half of the same number.
const (
	maxProductNameLength = 255
	maxIconNameLength    = 100
)

// Merge serves POST /api/storages/{storage_id}/products/{product_id}/merge —
// the duplicate cleanup of docs/specs/16-product-maintenance.md.
//
// Body: {"source_product_id": "<uuid>"}. **The URL names the survivor; the body
// names the duplicate that disappears into it.** The survivor's own fields are
// kept unchanged — a merge says "these were the same thing all along, and
// *this* is its description".
//
// 200 with the survivor, its moved batch count and its recomputed date count.
// 404 when either id is not in this storage, indistinguishable from one that
// does not exist. 422 for a self-merge or an unknown field.
//
// No inventory_logs row is written: nothing about how much stock exists
// changed, only which product row it belongs to (store.MergeProducts).
func (h *ProductHandler) Merge(w http.ResponseWriter, r *http.Request) {
	storageID, ok := StorageIDFrom(r.Context())
	if !ok {
		h.errors.WriteError(w, r, Internal(errNoStorageInContext))
		return
	}
	survivorID, failure := idFromPath(r, "product_id", "malformed product id")
	if failure != nil {
		h.errors.WriteError(w, r, failure)
		return
	}

	var body struct {
		SourceProductID string `json:"source_product_id"`
	}
	if failure := decodeJSONStrict(w, r, &body); failure != nil {
		h.errors.WriteError(w, r, failure)
		return
	}

	sourceID, err := uuid.Parse(body.SourceProductID)
	if err != nil {
		h.errors.WriteError(w, r, ValidationFailed(map[string][]string{
			"source_product_id": {"Must be a UUID."},
		}, err))
		return
	}
	// Checked before either lookup, so the answer to "can I merge a product
	// into itself" does not depend on whether the id exists: input validation
	// precedes existence, here as everywhere.
	if sourceID == survivorID {
		h.errors.WriteError(w, r, ValidationFailed(map[string][]string{
			"source_product_id": {"A product cannot be merged into itself."},
		}, nil))
		return
	}

	result, err := h.store.MergeProducts(r.Context(), storageID, survivorID, sourceID)
	if err != nil {
		h.errors.WriteError(w, r, FromStoreError(err, "product not found in storage"))
		return
	}
	// The file goes only after the transaction committed, and only when no
	// product still points at it: a file delete cannot be rolled back.
	h.removeStoredPicture(r.Context(), result.OrphanedImage)

	detail, failure := h.detail(r, storageID, survivorID, productCounts{
		Moved: &result.MovedBatches, Recomputed: &result.RecomputedBatches,
	})
	if failure != nil {
		h.errors.WriteError(w, r, failure)
		return
	}
	writeJSON(w, http.StatusOK, detail)
}

// Delete serves DELETE /api/storages/{storage_id}/products/{product_id}.
//
// Allowed even with stock on hand — the schema already says so
// (inventory_batches.product_id ON DELETE CASCADE), and "this was never a
// thing we should track" legitimately includes things currently on a shelf.
// Confirming is the frontend's job, with the stock and the history it is about
// to take named in the prompt.
//
// 204 on success, 404 for a product not in this storage. Batches and logs go
// with it, a tombstone is written in the same transaction so delta-sync clients
// drop it (docs/specs/12-client-api-contract.md), and the catalog row it may
// have seeded is untouched.
func (h *ProductHandler) Delete(w http.ResponseWriter, r *http.Request) {
	storageID, ok := StorageIDFrom(r.Context())
	if !ok {
		h.errors.WriteError(w, r, Internal(errNoStorageInContext))
		return
	}
	productID, failure := idFromPath(r, "product_id", "malformed product id")
	if failure != nil {
		h.errors.WriteError(w, r, failure)
		return
	}

	orphaned, err := h.store.DeleteProduct(r.Context(), storageID, productID)
	if err != nil {
		h.errors.WriteError(w, r, FromStoreError(err, "product not found in storage"))
		return
	}
	h.removeStoredPicture(r.Context(), orphaned)

	writeJSON(w, http.StatusNoContent, nil)
}

// removeStoredPicture deletes the file behind a product image_url that no
// product references any more.
//
// The store hands back the URL rather than a filename because the URL is what
// the column holds; the filename is its last segment
// (productImageURL, productimages.go). A URL this server did not generate —
// nothing writes one today, since a picture only reaches a product by being
// promoted into permanent storage — is ignored rather than turned into a path.
//
// A failure leaves an unreferenced file behind, which costs disk space and is
// unreachable, so it is logged rather than reported: the row is already gone,
// and failing the response would describe a deletion that did happen as one
// that did not.
func (h *ProductHandler) removeStoredPicture(ctx context.Context, imageURL string) {
	if imageURL == "" {
		return
	}
	prefix := "/api/storages/"
	marker := "/product-images/"
	if !strings.HasPrefix(imageURL, prefix) {
		return
	}
	index := strings.Index(imageURL, marker)
	if index < 0 {
		return
	}
	name := imageURL[index+len(marker):]
	if name == "" || strings.Contains(name, "/") {
		return
	}
	h.pictures.remove(ctx, h.errors, name)
}
