package httpapi

import (
	"context"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/CDRO/Inventory/internal/matching"
	"github.com/CDRO/Inventory/internal/store"
)

// maxShoppingListLines bounds one submission.
//
// A pasted list is a person's shopping, not a data feed. The cap exists so a
// paste accident cannot start hundreds of trigram queries in one request.
const maxShoppingListLines = 200

// ShoppingListStore is the slice of the store these handlers use.
type ShoppingListStore interface {
	CreateShoppingList(ctx context.Context, storageID uuid.UUID, source store.ShoppingListSource, createdBy *uuid.UUID, items []store.NewShoppingListItem) (*store.ShoppingList, []store.ShoppingListItem, error)
	ShoppingListWithItems(ctx context.Context, storageID, listID uuid.UUID) (*store.ShoppingList, []store.ShoppingListItem, error)
	ShoppingListItemByID(ctx context.Context, storageID, itemID uuid.UUID) (*store.ShoppingListItem, error)
	RematchShoppingListItem(ctx context.Context, storageID, itemID uuid.UUID, rawText string, status store.ShoppingListItemStatus, matchedProductID *uuid.UUID) (*store.ShoppingListItem, error)
	ResolveShoppingListItem(ctx context.Context, storageID, itemID uuid.UUID, productID *uuid.UUID, quantity int) (*store.ShoppingListItem, error)
}

// Matcher is the matching service, as these handlers need it.
type Matcher interface {
	MatchProductCandidates(ctx context.Context, storageID uuid.UUID, text string) (matching.Result, error)
}

// ShoppingListHandler serves docs/specs/07-shopping-list-reconciliation.md.
type ShoppingListHandler struct {
	store   ShoppingListStore
	matcher Matcher
	errors  *ErrorWriter
}

// NewShoppingListHandler wires the handlers to their collaborators.
func NewShoppingListHandler(s ShoppingListStore, m Matcher, errs *ErrorWriter) *ShoppingListHandler {
	return &ShoppingListHandler{store: s, matcher: m, errors: errs}
}

// catalogCard is a catalog hit as a client is allowed to see it.
//
// **This type is the enforcement point for the catalog's privacy rule.** It
// carries display fields and nothing else: no catalog id, no created_at, no
// counts, no hint that another storage described this product first
// (docs/specs/03-auth-and-multi-tenancy.md).
//
// It exists as a separate struct rather than as json tags on
// matching.CatalogMatch precisely so that adding a field to the matcher cannot
// silently widen what goes over the wire. A new field has to be added here,
// deliberately, to be disclosed.
type catalogCard struct {
	DisplayName          string   `json:"display_name"`
	CategoryPath         *string  `json:"category_path"`
	ItemType             string   `json:"item_type"`
	ImageURL             *string  `json:"image_url"`
	IconName             *string  `json:"icon_name"`
	DefaultShelfLifeDays *int     `json:"default_shelf_life_days"`
	Variants             []string `json:"variants"`
}

// productRef is a product of this storage, by id and name.
type productRef struct {
	ID   uuid.UUID `json:"id"`
	Name string    `json:"name"`
}

// shoppingListItemResponse is one line and everything the resolution UI needs
// to act on it.
type shoppingListItemResponse struct {
	ID       uuid.UUID `json:"id"`
	RawText  string    `json:"raw_text"`
	Status   string    `json:"status"`
	Quantity int       `json:"quantity"`

	MatchedProduct   *productRef  `json:"matched_product"`
	Candidates       []productRef `json:"candidates"`
	Catalog          *catalogCard `json:"catalog"`
	NeedsImageSearch bool         `json:"needs_image_search"`
	ResolvedQuantity *int         `json:"resolved_quantity"`
}

type shoppingListResponse struct {
	ID        uuid.UUID                  `json:"id"`
	Source    string                     `json:"source"`
	CreatedAt time.Time                  `json:"created_at"`
	Items     []shoppingListItemResponse `json:"items"`
}

// Create serves POST /api/storages/{storage_id}/shopping-lists.
//
// Only the text source is implemented. The photo variant needs the background
// job runner and a Gemini OCR call, neither of which exists yet (issue #28);
// it is refused explicitly rather than silently treated as text, which would
// file an image's bytes as somebody's shopping.
func (h *ShoppingListHandler) Create(w http.ResponseWriter, r *http.Request) {
	storageID, ok := StorageIDFrom(r.Context())
	if !ok {
		h.errors.WriteError(w, r, Internal(errNoStorageInContext))
		return
	}

	var body struct {
		Source  string `json:"source"`
		RawText string `json:"raw_text"`
	}
	if failure := decodeJSON(w, r, &body); failure != nil {
		h.errors.WriteError(w, r, failure)
		return
	}

	if body.Source == string(store.SourcePhoto) {
		h.errors.WriteError(w, r, &Failure{
			Status:  http.StatusNotImplemented,
			Code:    "not_implemented",
			Message: "Photographed shopping lists are not available yet.",
			Reason:  "photo ingestion needs the background job runner (#28) and a Gemini OCR client",
		})
		return
	}
	if body.Source != "" && body.Source != string(store.SourceText) {
		h.errors.WriteError(w, r, ValidationFailed(
			map[string][]string{"source": {`Must be "text" or "photo".`}}, nil))
		return
	}

	lines := matching.SplitLines(body.RawText)
	if len(lines) == 0 {
		h.errors.WriteError(w, r, ValidationFailed(
			map[string][]string{"raw_text": {"Add at least one line."}}, nil))
		return
	}
	if len(lines) > maxShoppingListLines {
		h.errors.WriteError(w, r, ValidationFailed(
			map[string][]string{"raw_text": {"That is more lines than one list can hold."}}, nil))
		return
	}

	// Match every line before writing anything, so the whole list lands in one
	// transaction with its statuses already decided. This is also the only
	// place matching runs for these lines: it is explicitly not re-run when
	// the list is read back.
	items := make([]store.NewShoppingListItem, 0, len(lines))
	results := make([]matching.Result, 0, len(lines))

	for _, line := range lines {
		text, _ := matching.ParseLine(line)

		result, err := h.matcher.MatchProductCandidates(r.Context(), storageID, text)
		if err != nil {
			h.errors.WriteError(w, r, Internal(err))
			return
		}
		results = append(results, result)

		item := store.NewShoppingListItem{
			RawText: line,
			Status:  store.ShoppingListItemStatus(result.Status),
		}
		if result.Product != nil {
			item.MatchedProductID = &result.Product.ProductID
		}
		items = append(items, item)
	}

	list, created, err := h.store.CreateShoppingList(r.Context(), storageID, store.SourceText, actingUser(r), items)
	if err != nil {
		h.errors.WriteError(w, r, FromStoreError(err, "shopping list could not be created"))
		return
	}

	writeJSON(w, http.StatusCreated, buildListResponse(list, created, results))
}

// Get serves GET /api/storages/{storage_id}/shopping-lists/{id}.
//
// The stored status is returned exactly as ingestion decided it. The candidate
// lists and catalog card are recomputed for display, because the schema keeps
// only the decision and not the evidence behind it — and a review UI that is
// deep-linkable and openable days later on another device has to be able to
// render the choices again.
//
// Recomputation is display-only and never writes: a catalog row added by
// another storage in the meantime can change which alternatives are offered,
// but it cannot move a line from `new_item` to `exact_match` underneath a user
// who is looking at it. That is what
// docs/specs/07-shopping-list-reconciliation.md means by matching not being
// automatic once a list exists.
func (h *ShoppingListHandler) Get(w http.ResponseWriter, r *http.Request) {
	storageID, ok := StorageIDFrom(r.Context())
	if !ok {
		h.errors.WriteError(w, r, Internal(errNoStorageInContext))
		return
	}

	listID, failure := idFromPath(r, "id", "malformed shopping list id")
	if failure != nil {
		h.errors.WriteError(w, r, failure)
		return
	}

	list, items, err := h.store.ShoppingListWithItems(r.Context(), storageID, listID)
	if err != nil {
		h.errors.WriteError(w, r, FromStoreError(err, "shopping list not in this storage or nonexistent"))
		return
	}

	results := make([]matching.Result, 0, len(items))
	for _, item := range items {
		results = append(results, h.displayMatch(r.Context(), storageID, item))
	}

	writeJSON(w, http.StatusOK, buildListResponse(list, items, results))
}

// displayMatch recomputes the choices for one line, for rendering only.
//
// A resolved line needs no choices — the decision is made — so matching is
// skipped entirely, which also keeps a finished list cheap to open.
func (h *ShoppingListHandler) displayMatch(ctx context.Context, storageID uuid.UUID, item store.ShoppingListItem) matching.Result {
	if item.Status == store.ItemResolved {
		return matching.Result{Status: matching.Status(item.Status)}
	}

	text, _ := matching.ParseLine(item.RawText)
	result, err := h.matcher.MatchProductCandidates(ctx, storageID, text)
	if err != nil {
		// Display detail is a convenience. A line that cannot be re-matched
		// right now still renders with its stored status and can still be
		// resolved by hand, which is better than failing the whole page.
		return matching.Result{Status: matching.Status(item.Status)}
	}
	return result
}

// Rematch serves POST /api/storages/{storage_id}/shopping-lists/{id}/items/{item_id}/rematch.
//
// This is the explicit action the spec asks for: a user who corrects a misread
// line asks for it to be matched again, rather than the system re-running
// matching implicitly and changing what they were looking at.
func (h *ShoppingListHandler) Rematch(w http.ResponseWriter, r *http.Request) {
	storageID, ok := StorageIDFrom(r.Context())
	if !ok {
		h.errors.WriteError(w, r, Internal(errNoStorageInContext))
		return
	}

	itemID, failure := idFromPath(r, "item_id", "malformed shopping list item id")
	if failure != nil {
		h.errors.WriteError(w, r, failure)
		return
	}

	var body struct {
		RawText string `json:"raw_text"`
	}
	if failure := decodeJSON(w, r, &body); failure != nil {
		h.errors.WriteError(w, r, failure)
		return
	}

	text, _ := matching.ParseLine(body.RawText)
	if text == "" {
		h.errors.WriteError(w, r, ValidationFailed(
			map[string][]string{"raw_text": {"A line cannot be empty."}}, nil))
		return
	}

	result, err := h.matcher.MatchProductCandidates(r.Context(), storageID, text)
	if err != nil {
		h.errors.WriteError(w, r, Internal(err))
		return
	}

	var matchedID *uuid.UUID
	if result.Product != nil {
		matchedID = &result.Product.ProductID
	}

	updated, err := h.store.RematchShoppingListItem(r.Context(), storageID, itemID,
		body.RawText, store.ShoppingListItemStatus(result.Status), matchedID)
	if err != nil {
		h.errors.WriteError(w, r, conflictAs(err,
			"shopping list item not in this storage or nonexistent",
			"This line has already been resolved."))
		return
	}

	writeJSON(w, http.StatusOK, buildItemResponse(*updated, result))
}

// Resolve serves POST /api/storages/{storage_id}/shopping-lists/{id}/items/{item_id}/resolve.
//
// Nothing about a line touches inventory until this call: the spec's last
// acceptance criterion is that no products or inventory_batches row exists
// before the user explicitly confirms a resolution for that line.
//
// This PR resolves a line against a product that already exists, or records a
// dismissal with no product at all. Creating a product, its catalog entry and
// its opening batch from a resolution is the next slice of this spec and is
// tracked on the issue — a confirm that half-created things would be worse
// than one that is honest about not doing it yet.
func (h *ShoppingListHandler) Resolve(w http.ResponseWriter, r *http.Request) {
	storageID, ok := StorageIDFrom(r.Context())
	if !ok {
		h.errors.WriteError(w, r, Internal(errNoStorageInContext))
		return
	}

	itemID, failure := idFromPath(r, "item_id", "malformed shopping list item id")
	if failure != nil {
		h.errors.WriteError(w, r, failure)
		return
	}

	var body struct {
		ProductID *string `json:"product_id"`
		Quantity  *int    `json:"quantity"`
	}
	if failure := decodeJSON(w, r, &body); failure != nil {
		h.errors.WriteError(w, r, failure)
		return
	}

	fields := map[string][]string{}

	var productID *uuid.UUID
	if body.ProductID != nil {
		parsed, err := uuid.Parse(*body.ProductID)
		if err != nil {
			fields["product_id"] = append(fields["product_id"], "Must be a UUID or null.")
		} else {
			productID = &parsed
		}
	}

	quantity := 1
	if body.Quantity != nil {
		quantity = *body.Quantity
	}
	if quantity < 0 {
		fields["quantity"] = append(fields["quantity"], "Cannot be negative.")
	}

	if len(fields) > 0 {
		h.errors.WriteError(w, r, ValidationFailed(fields, nil))
		return
	}

	updated, err := h.store.ResolveShoppingListItem(r.Context(), storageID, itemID, productID, quantity)
	if err != nil {
		h.errors.WriteError(w, r, conflictAs(err,
			"shopping list item or product not in this storage or nonexistent",
			"This line has already been resolved."))
		return
	}

	writeJSON(w, http.StatusOK, buildItemResponse(*updated, matching.Result{Status: matching.Status(updated.Status)}))
}

// buildListResponse pairs stored rows with their match results positionally.
//
// The two slices are built from the same source in the same order, so index i
// of one describes index i of the other.
func buildListResponse(list *store.ShoppingList, items []store.ShoppingListItem, results []matching.Result) shoppingListResponse {
	out := shoppingListResponse{
		ID:        list.ID,
		Source:    string(list.Source),
		CreatedAt: list.CreatedAt,
		Items:     make([]shoppingListItemResponse, 0, len(items)),
	}
	for i, item := range items {
		var result matching.Result
		if i < len(results) {
			result = results[i]
		}
		out.Items = append(out.Items, buildItemResponse(item, result))
	}
	return out
}

// buildItemResponse renders one line.
//
// The catalog card is built field by field rather than by marshalling the
// matcher's struct — see catalogCard's doc comment for why that distinction is
// load-bearing rather than stylistic.
func buildItemResponse(item store.ShoppingListItem, result matching.Result) shoppingListItemResponse {
	_, quantity := matching.ParseLine(item.RawText)

	out := shoppingListItemResponse{
		ID:               item.ID,
		RawText:          item.RawText,
		Status:           string(item.Status),
		Quantity:         quantity,
		Candidates:       []productRef{},
		ResolvedQuantity: item.ResolvedQuantity,
		NeedsImageSearch: result.NeedsExternalLookup(),
	}

	if result.Product != nil {
		out.MatchedProduct = &productRef{ID: result.Product.ProductID, Name: result.Product.Name}
	} else if item.MatchedProductID != nil {
		// A stored match whose display detail was not recomputed — a resolved
		// line, or a re-match that could not run. The id is still this
		// storage's own product, so it is safe to return without a name.
		out.MatchedProduct = &productRef{ID: *item.MatchedProductID}
	}

	for _, candidate := range result.Candidates {
		out.Candidates = append(out.Candidates, productRef{ID: candidate.ProductID, Name: candidate.Name})
	}

	if result.Catalog != nil {
		variants := make([]string, 0, len(result.Catalog.Variants))
		for _, v := range result.Catalog.Variants {
			// Display name only. The variant's catalog id stays server-side,
			// exactly like the hit's own.
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

	return out
}

// idFromPath parses a UUID path parameter, answering 404 for anything
// unparseable — the same non-enumeration reasoning as locationIDFromPath.
func idFromPath(r *http.Request, param, reason string) (uuid.UUID, *Failure) {
	id, err := uuid.Parse(chi.URLParam(r, param))
	if err != nil {
		return uuid.Nil, NotFound(reason)
	}
	return id, nil
}

