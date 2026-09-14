package httpapi

import (
	"context"
	"encoding/csv"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/CDRO/Inventory/internal/matching"
	"github.com/CDRO/Inventory/internal/store"
)

// ReorderStore is the slice of the store these handlers use.
type ReorderStore interface {
	ReorderProducts(ctx context.Context, storageID uuid.UUID) ([]store.ReorderProduct, error)
	ProductMinStock(ctx context.Context, storageID, id uuid.UUID) (int, error)
	UpdateProductMinStockAsUser(ctx context.Context, storageID, id uuid.UUID, minStock int, userID uuid.UUID) (*store.Product, error)
	CreateProduct(ctx context.Context, storageID uuid.UUID, in store.NewProduct) (*store.Product, error)
}

// ReorderHandler serves docs/specs/10-reorder-and-shopping-export.md.
type ReorderHandler struct {
	store   ReorderStore
	matcher Matcher
	errors  *ErrorWriter
}

// NewReorderHandler wires the handlers to their collaborators. matcher may be
// nil; the two routes that need it (Match and AddItem) are only registered by
// the router when it is present, the same rule the shopping-list routes
// follow.
func NewReorderHandler(s ReorderStore, matcher Matcher, errs *ErrorWriter) *ReorderHandler {
	return &ReorderHandler{store: s, matcher: matcher, errors: errs}
}

// outOfStockItem is one row of the dashboard's out_of_stock list. current_stock
// is always 0 here — that is the definition of the bucket — so it is not
// carried in the response at all, matching the shape in the spec.
type outOfStockItem struct {
	ProductID uuid.UUID `json:"product_id"`
	Name      string    `json:"name"`
	MinStock  int       `json:"min_stock"`
}

// lowStockItem is one row of the dashboard's low_stock list.
type lowStockItem struct {
	ProductID    uuid.UUID `json:"product_id"`
	Name         string    `json:"name"`
	CurrentStock int       `json:"current_stock"`
	MinStock     int       `json:"min_stock"`
}

// classifyReorder splits ReorderProducts' rows into the dashboard's two
// mutually-exclusive buckets.
//
// This is the one place that decides "out of stock" versus "low stock",
// shared by the dashboard and the export handler below — the acceptance
// criterion that the two must never disagree is enforced by there being only
// one function that could disagree with itself.
func classifyReorder(rows []store.ReorderProduct) ([]outOfStockItem, []lowStockItem) {
	outOfStock := []outOfStockItem{}
	lowStock := []lowStockItem{}
	for _, row := range rows {
		switch {
		// A zero threshold means "not tracked for reorder" regardless of
		// actual stock. ReorderProducts already filters this at the query, but
		// the acceptance criterion is about the row never appearing in either
		// bucket — checked again here so that guarantee does not depend on the
		// query being the only thing that ever produces this slice.
		case row.MinStock <= 0:
			continue
		case row.CurrentStock == 0:
			outOfStock = append(outOfStock, outOfStockItem{ProductID: row.ProductID, Name: row.Name, MinStock: row.MinStock})
		case row.CurrentStock < row.MinStock:
			lowStock = append(lowStock, lowStockItem{ProductID: row.ProductID, Name: row.Name, CurrentStock: row.CurrentStock, MinStock: row.MinStock})
		}
	}
	return outOfStock, lowStock
}

// Dashboard serves GET /api/storages/{storage_id}/dashboard/reorder.
func (h *ReorderHandler) Dashboard(w http.ResponseWriter, r *http.Request) {
	storageID, ok := StorageIDFrom(r.Context())
	if !ok {
		h.errors.WriteError(w, r, Internal(errNoStorageInContext))
		return
	}

	rows, err := h.store.ReorderProducts(r.Context(), storageID)
	if err != nil {
		h.errors.WriteError(w, r, FromStoreError(err, "list reorder products"))
		return
	}

	outOfStock, lowStock := classifyReorder(rows)
	writeJSON(w, http.StatusOK, struct {
		OutOfStock []outOfStockItem `json:"out_of_stock"`
		LowStock   []lowStockItem   `json:"low_stock"`
	}{OutOfStock: outOfStock, LowStock: lowStock})
}

// Export serves GET /api/storages/{storage_id}/dashboard/reorder/export.
//
// CSV only — the spec is explicit that a server-side PDF endpoint is not
// built, because the PDF export is generated client-side from this same JSON
// the dashboard already fetched, and a second renderer of the same document
// is exactly the drift this spec avoids.
func (h *ReorderHandler) Export(w http.ResponseWriter, r *http.Request) {
	storageID, ok := StorageIDFrom(r.Context())
	if !ok {
		h.errors.WriteError(w, r, Internal(errNoStorageInContext))
		return
	}

	if format := r.URL.Query().Get("format"); format != "" && format != "csv" {
		h.errors.WriteError(w, r, ValidationFailed(
			map[string][]string{"format": {`Must be "csv" — PDF export is generated in the browser, not by this endpoint.`}}, nil))
		return
	}

	rows, err := h.store.ReorderProducts(r.Context(), storageID)
	if err != nil {
		h.errors.WriteError(w, r, FromStoreError(err, "list reorder products"))
		return
	}
	outOfStock, lowStock := classifyReorder(rows)

	filename := fmt.Sprintf("reorder-list-%s.csv", time.Now().UTC().Format("2006-01-02"))
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)

	cw := csv.NewWriter(w)
	_ = cw.Write([]string{"Name", "Current stock", "Min stock", "Suggested reorder qty"})
	for _, item := range outOfStock {
		_ = cw.Write([]string{item.Name, "0", strconv.Itoa(item.MinStock), strconv.Itoa(reorderQty(0, item.MinStock))})
	}
	for _, item := range lowStock {
		_ = cw.Write([]string{item.Name, strconv.Itoa(item.CurrentStock), strconv.Itoa(item.MinStock), strconv.Itoa(reorderQty(item.CurrentStock, item.MinStock))})
	}
	// A write failure here means the client hung up mid-body; the status line
	// is already sent, so there is nothing left to report.
	cw.Flush()
}

// reorderQty is the suggested purchase quantity: the gap to the threshold,
// clamped to at least 1 — the spec's rule for a row that is already at or
// past its threshold in either direction (which cannot actually happen for a
// row that reached this list, but the clamp is what the spec asks for).
func reorderQty(current, min int) int {
	q := min - current
	if q < 1 {
		q = 1
	}
	return q
}

// reorderProductRef is a local product as the reorder match preview shows it —
// productRef plus min_stock. The extra field is why this is its own type
// rather than a reuse of shoppinglists.go's productRef: the confirm step's
// min_stock input needs the product's actual current threshold to prefill,
// not a hardcoded 1, because a product can be a confident local match while
// sitting outside both dashboard buckets (already well-stocked, or not yet
// tracked at min_stock = 0) — nowhere else already carries that number back
// to the caller.
type reorderProductRef struct {
	ID       uuid.UUID `json:"id"`
	Name     string    `json:"name"`
	MinStock int       `json:"min_stock"`
}

// reorderMatchResponse mirrors shoppingListItemResponse's per-line shape, cut
// down to what a single ad hoc name needs — no raw text, quantity or id, since
// nothing is stored until AddItem is called.
type reorderMatchResponse struct {
	Status           string              `json:"status"`
	MatchedProduct   *reorderProductRef  `json:"matched_product"`
	Candidates       []reorderProductRef `json:"candidates"`
	Catalog          *catalogCard        `json:"catalog"`
	NeedsImageSearch bool                `json:"needs_image_search"`
}

// Match serves POST /api/storages/{storage_id}/dashboard/reorder/items/match.
//
// This runs the same matching.Service pipeline
// (docs/specs/07-shopping-list-reconciliation.md) the shopping list uses, so
// the "Add item" input on the reorder dashboard offers the identical three
// stages — a confident local product, a catalog hit ready for one-click
// acceptance with no external call, or a miss that is worth an image search —
// rather than a second, thinner classifier that could disagree with the one
// spec 07 already built and tuned.
func (h *ReorderHandler) Match(w http.ResponseWriter, r *http.Request) {
	storageID, ok := StorageIDFrom(r.Context())
	if !ok {
		h.errors.WriteError(w, r, Internal(errNoStorageInContext))
		return
	}

	var body struct {
		Name string `json:"name"`
	}
	if failure := decodeJSON(w, r, &body); failure != nil {
		h.errors.WriteError(w, r, failure)
		return
	}

	name := strings.TrimSpace(body.Name)
	if name == "" {
		h.errors.WriteError(w, r, ValidationFailed(
			map[string][]string{"name": {"A product name is required."}}, nil))
		return
	}

	result, err := h.matcher.MatchProductCandidates(r.Context(), storageID, name)
	if err != nil {
		h.errors.WriteError(w, r, Internal(err))
		return
	}

	response, err := h.buildReorderMatchResponse(r.Context(), storageID, result)
	if err != nil {
		h.errors.WriteError(w, r, Internal(err))
		return
	}
	writeJSON(w, http.StatusOK, response)
}

// buildReorderMatchResponse reads each matched product's own min_stock rather
// than defaulting the field to a placeholder — see reorderProductRef's doc
// comment for why a stale default here is a real overwrite risk, not just an
// odd first paint.
func (h *ReorderHandler) buildReorderMatchResponse(ctx context.Context, storageID uuid.UUID, result matching.Result) (reorderMatchResponse, error) {
	out := reorderMatchResponse{
		Status:           string(result.Status),
		Candidates:       []reorderProductRef{},
		NeedsImageSearch: result.NeedsExternalLookup(),
	}

	if result.Product != nil {
		ref, err := h.reorderRef(ctx, storageID, result.Product.ProductID, result.Product.Name)
		if err != nil {
			return reorderMatchResponse{}, err
		}
		out.MatchedProduct = &ref
	}
	for _, candidate := range result.Candidates {
		ref, err := h.reorderRef(ctx, storageID, candidate.ProductID, candidate.Name)
		if err != nil {
			return reorderMatchResponse{}, err
		}
		out.Candidates = append(out.Candidates, ref)
	}
	if result.Catalog != nil {
		variants := make([]string, 0, len(result.Catalog.Variants))
		for _, v := range result.Catalog.Variants {
			variants = append(variants, v.DisplayName)
		}
		out.Catalog = &catalogCard{
			DisplayName:          result.Catalog.DisplayName,
			CategoryPath:         result.Catalog.CategoryPath,
			ItemType:             result.Catalog.ItemType,
			ImageURL:             result.Catalog.ImageURL,
			IconName:             result.Catalog.IconName,
			DefaultShelfLifeDays: result.Catalog.DefaultShelfLifeDays,
			Variants:             variants,
		}
	}
	return out, nil
}

// reorderRef looks up id's current min_stock and pairs it with the name the
// matcher already resolved, rather than a second name lookup — the matcher's
// own trigram query is the source of truth for the display name, this call is
// only for the number that query does not carry.
func (h *ReorderHandler) reorderRef(ctx context.Context, storageID, id uuid.UUID, name string) (reorderProductRef, error) {
	minStock, err := h.store.ProductMinStock(ctx, storageID, id)
	if err != nil {
		return reorderProductRef{}, err
	}
	return reorderProductRef{ID: id, Name: name, MinStock: minStock}, nil
}

// reorderProductResponse is the outcome of AddItem: the product that now
// carries the threshold, and whether it was just created.
type reorderProductResponse struct {
	ProductID uuid.UUID `json:"product_id"`
	Name      string    `json:"name"`
	MinStock  int       `json:"min_stock"`
	Created   bool      `json:"created"`
}

// AddItem serves POST /api/storages/{storage_id}/dashboard/reorder/items — the
// reorder dashboard's "Add item" action.
//
// Two outcomes, per the spec's acceptance criteria:
//
//   - A product by this name already exists: its min_stock is updated and
//     nothing is created. This is re-checked here with the same matcher
//     rather than only trusted from ProductID, so the "no duplicate name"
//     guarantee holds even if a caller does not send one — an exact
//     case/whitespace-insensitive match to an existing name always resolves
//     to StatusExactMatch at a similarity of 1.0, well past the exact-match
//     threshold.
//   - Otherwise a new product is created with **zero inventory_batches rows
//     and no inventory_logs row**: there is no physical stock to record, only
//     a threshold that says this product should be watched for reorder.
//     min_stock is clamped to at least 1, since a product added here with
//     min_stock = 0 would vanish from the very list it was created on.
func (h *ReorderHandler) AddItem(w http.ResponseWriter, r *http.Request) {
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

	var body struct {
		Name                 string  `json:"name"`
		ProductID            *string `json:"product_id"`
		MinStock             *int    `json:"min_stock"`
		CategoryID           *string `json:"category_id"`
		ItemType             string  `json:"item_type"`
		ImageURL             *string `json:"image_url"`
		IconName             *string `json:"icon_name"`
		DefaultShelfLifeDays *int    `json:"default_shelf_life_days"`
	}
	if failure := decodeJSON(w, r, &body); failure != nil {
		h.errors.WriteError(w, r, failure)
		return
	}

	fields := map[string][]string{}

	name := strings.TrimSpace(body.Name)
	if name == "" {
		fields["name"] = append(fields["name"], "A product name is required.")
	}

	minStock := 1
	if body.MinStock != nil {
		minStock = *body.MinStock
	}
	if minStock < 1 {
		fields["min_stock"] = append(fields["min_stock"], "Must be at least 1.")
	}

	var productID *uuid.UUID
	if body.ProductID != nil {
		parsed, err := uuid.Parse(*body.ProductID)
		if err != nil {
			fields["product_id"] = append(fields["product_id"], "Must be a UUID or null.")
		} else {
			productID = &parsed
		}
	}

	var categoryID *uuid.UUID
	if body.CategoryID != nil {
		parsed, err := uuid.Parse(*body.CategoryID)
		if err != nil {
			fields["category_id"] = append(fields["category_id"], "Must be a UUID or null.")
		} else {
			categoryID = &parsed
		}
	}

	itemType := store.ItemType(body.ItemType)
	switch itemType {
	case "", store.ItemPerishable, store.ItemLongShelfLife, store.ItemNonPerishable:
	default:
		fields["item_type"] = append(fields["item_type"], "Must be perishable, long_shelf_life or non_perishable.")
	}

	if len(fields) > 0 {
		h.errors.WriteError(w, r, ValidationFailed(fields, nil))
		return
	}

	// No explicit target: check whether this name already names a product in
	// this storage before creating a second one. See the doc comment above —
	// this is what makes "adjusts min_stock instead of duplicating" a server
	// guarantee rather than a UI convention a caller could bypass.
	if productID == nil {
		result, err := h.matcher.MatchProductCandidates(r.Context(), storageID, name)
		if err != nil {
			h.errors.WriteError(w, r, Internal(err))
			return
		}
		if result.Status == matching.StatusExactMatch && result.Product != nil {
			productID = &result.Product.ProductID
		}
	}

	if productID != nil {
		updated, err := h.store.UpdateProductMinStockAsUser(r.Context(), storageID, *productID, minStock, user.ID)
		if err != nil {
			h.errors.WriteError(w, r, FromStoreError(err, "product not in this storage or nonexistent"))
			return
		}
		writeJSON(w, http.StatusOK, reorderProductResponse{
			ProductID: updated.ID, Name: updated.Name, MinStock: updated.MinStock, Created: false,
		})
		return
	}

	created, err := h.store.CreateProduct(r.Context(), storageID, store.NewProduct{
		Name:                 name,
		CategoryID:           categoryID,
		ItemType:             itemType,
		DefaultShelfLifeDays: body.DefaultShelfLifeDays,
		MinStock:             minStock,
		ImageURL:             body.ImageURL,
		IconName:             body.IconName,
	})
	if err != nil {
		h.errors.WriteError(w, r, FromStoreError(err, "product could not be created"))
		return
	}
	writeJSON(w, http.StatusCreated, reorderProductResponse{
		ProductID: created.ID, Name: created.Name, MinStock: created.MinStock, Created: true,
	})
}
