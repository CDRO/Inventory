package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

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
	ResolveShoppingListItem(ctx context.Context, storageID, itemID uuid.UUID, in store.ResolveLine, userID *uuid.UUID) (*store.ResolveResult, error)
	// FindCatalogProduct reads a catalog row by name — here, only ever a
	// variant this line's card offered, never one a client names freely.
	FindCatalogProduct(ctx context.Context, name string) (*store.CatalogProduct, error)
}

// Matcher is the matching service, as these handlers need it.
type Matcher interface {
	MatchProductCandidates(ctx context.Context, storageID uuid.UUID, text string) (matching.Result, error)
}

// ShoppingListHandler serves docs/specs/07-shopping-list-reconciliation.md.
type ShoppingListHandler struct {
	store    ShoppingListStore
	matcher  Matcher
	pictures productPictures
	errors   *ErrorWriter
}

// NewShoppingListHandler wires the handlers to their collaborators. cache and
// productImages may be nil: catalog cards then show no picture, and a picked
// picture is refused rather than recorded by an evictable address.
func NewShoppingListHandler(s ShoppingListStore, m Matcher, cache ImageCache, productImages PhotoStore, errs *ErrorWriter) *ShoppingListHandler {
	return &ShoppingListHandler{
		store: s, matcher: m, errors: errs,
		pictures: productPictures{images: productImages, cache: cache},
	}
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
// Only the text source is implemented. The photo variant still needs its own
// upload route and a Gemini prompt that reads a list's lines (the job runner and
// the vision client it would use now exist, from photo ingestion); until then
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
			Reason:  "photo shopping lists need an upload route and a list-reading Gemini prompt",
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

	response := buildListResponse(list, created, results)
	h.showCatalogImages(r.Context(), storageID, response.Items, results)
	writeJSON(w, http.StatusCreated, response)
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

	response := buildListResponse(list, items, results)
	h.showCatalogImages(r.Context(), storageID, response.Items, results)
	writeJSON(w, http.StatusOK, response)
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

	response := []shoppingListItemResponse{buildItemResponse(*updated, result)}
	h.showCatalogImages(r.Context(), storageID, response, []matching.Result{result})
	writeJSON(w, http.StatusOK, response[0])
}

// resolveRequest is the body of POST .../items/{item_id}/resolve: what the
// person confirmed for one line (docs/specs/07-shopping-list-reconciliation.md,
// "Resolution UI per state").
type resolveRequest struct {
	// Quantity is what was bought; absent is the line's own parsed quantity.
	// Above 0 it needs LocationID, and becomes one batch there.
	Quantity   *int       `json:"quantity"`
	LocationID *uuid.UUID `json:"location_id"`

	// ProductID is an existing product: the exact match, or the candidate
	// picked for an ambiguous line. NewProduct creates one. Neither dismisses
	// the line, which then records nothing but its resolution.
	ProductID  *uuid.UUID          `json:"product_id"`
	NewProduct *resolveNewProduct `json:"new_product"`
}

// resolveNewProduct is a product the resolution creates.
type resolveNewProduct struct {
	// From is "catalog" — "Add this" on the line's catalog card, or on one of
	// its variants by Variant display name — or "manual" for everything a
	// person describes themselves: a new item with no card, "It's something
	// else", "Treat as new item", "Enter manually".
	From    string  `json:"from"`
	Variant *string `json:"variant"`

	// The manual fields. A catalog accept takes all of them from the card.
	Name       string     `json:"name"`
	CategoryID *uuid.UUID `json:"category_id"`
	ItemType   string     `json:"item_type"`
	MinStock   *int       `json:"min_stock"`
	// Image is the hash of a picked image suggestion, which is promoted into
	// permanent storage. Absent or null is no picture.
	Image *string `json:"image"`
}

const (
	resolveFromCatalog = "catalog"
	resolveFromManual  = "manual"
)

// resolveResponse is a resolved line plus what resolving it wrote.
type resolveResponse struct {
	shoppingListItemResponse
	ProductCreated bool       `json:"product_created"`
	BatchID        *uuid.UUID `json:"batch_id"`
}

// Resolve serves POST /api/storages/{storage_id}/shopping-lists/{id}/items/{item_id}/resolve.
//
// Nothing about a line touches inventory until this call — the spec's last
// acceptance criterion — and this call applies all of it at once: a new product
// with its catalog side, the batch, the purchase log row, and the line marked
// resolved, in one transaction.
//
// The client never names a catalog row. It was never given an id (see
// catalogCard), so accepting a card or declining one is decided here, by
// matching the line again and reading the card it produces. That is also how
// a declined card becomes the variant link: the server knows which row the
// person was shown.
func (h *ShoppingListHandler) Resolve(w http.ResponseWriter, r *http.Request) {
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

	itemID, failure := idFromPath(r, "item_id", "malformed shopping list item id")
	if failure != nil {
		h.errors.WriteError(w, r, failure)
		return
	}

	var body resolveRequest
	if failure := decodeJSON(w, r, &body); failure != nil {
		h.errors.WriteError(w, r, failure)
		return
	}
	if failure := body.validate(); failure != nil {
		h.errors.WriteError(w, r, failure)
		return
	}

	item, err := h.store.ShoppingListItemByID(r.Context(), storageID, itemID)
	if err != nil {
		h.errors.WriteError(w, r, FromStoreError(err, "shopping list item not in this storage or nonexistent"))
		return
	}

	line := store.ResolveLine{ProductID: body.ProductID, LocationID: body.LocationID}
	switch {
	case body.Quantity != nil:
		line.Quantity = *body.Quantity
	case body.ProductID != nil || body.NewProduct != nil:
		// "milk" on a list means one milk, and "eggs x2" means two.
		_, line.Quantity = matching.ParseLine(item.RawText)
	default:
		// No product: the line is being dismissed, and adds no stock.
		line.Quantity = 0
	}

	var picture string
	if body.NewProduct != nil {
		product, saved, failure := h.newProduct(r.Context(), storageID, *item, *body.NewProduct)
		if failure != nil {
			h.errors.WriteError(w, r, failure)
			return
		}
		line.NewProduct, picture = product, saved
	}

	userID := user.ID
	result, err := h.store.ResolveShoppingListItem(r.Context(), storageID, itemID, line, &userID)
	if err != nil {
		h.pictures.remove(r.Context(), h.errors, picture)
		h.errors.WriteError(w, r, conflictAs(err,
			"shopping list item, product, category or location not in this storage or nonexistent",
			"This line has already been resolved."))
		return
	}

	writeJSON(w, http.StatusOK, resolveResponse{
		shoppingListItemResponse: buildItemResponse(result.Item, matching.Result{Status: matching.Status(result.Item.Status)}),
		ProductCreated:           result.ProductCreated,
		BatchID:                  result.BatchID,
	})
}

// validate checks the body's shape. Whether its ids belong to this storage is
// the store's to decide, inside the transaction.
func (b resolveRequest) validate() *Failure {
	fields := map[string][]string{}
	add := func(field, msg string) { fields[field] = append(fields[field], msg) }

	if b.ProductID != nil && b.NewProduct != nil {
		add("product_id", "Give product_id or new_product, not both.")
	}
	if b.Quantity != nil {
		switch {
		case *b.Quantity < 0:
			add("quantity", "Cannot be negative.")
		case *b.Quantity > maxIngestQuantity:
			add("quantity", "Too large.")
		}
	}

	if p := b.NewProduct; p != nil {
		switch p.From {
		case resolveFromCatalog:
		case resolveFromManual:
			name := strings.TrimSpace(p.Name)
			switch {
			case name == "":
				add("new_product.name", "A name is required.")
			case utf8.RuneCountInString(name) > maxProductName:
				add("new_product.name", "Must be at most 255 characters.")
			}
			switch store.ItemType(p.ItemType) {
			case "", store.ItemPerishable, store.ItemLongShelfLife, store.ItemNonPerishable:
			default:
				add("new_product.item_type", "Must be perishable, long_shelf_life or non_perishable.")
			}
			if p.MinStock != nil && *p.MinStock < 0 {
				add("new_product.min_stock", "Cannot be negative.")
			}
		default:
			add("new_product.from", `Must be "catalog" or "manual".`)
		}
	}

	if len(fields) > 0 {
		return ValidationFailed(fields, nil)
	}
	return nil
}

// newProduct turns what the person described into the store's input, and
// promotes the picture it names. It returns the saved picture's name so a
// failed confirm can remove it.
func (h *ShoppingListHandler) newProduct(ctx context.Context, storageID uuid.UUID, item store.ShoppingListItem, in resolveNewProduct) (*store.ResolvedProduct, string, *Failure) {
	// The card this line shows. Matched again rather than trusted from the
	// client, which was never given a catalog id — and matched the same way
	// Get renders it, so the card accepted is the card that was on screen.
	var card *matching.CatalogMatch
	text, _ := matching.ParseLine(item.RawText)
	if result, err := h.matcher.MatchProductCandidates(ctx, storageID, text); err != nil {
		return nil, "", Internal(err)
	} else if result.Status == matching.StatusNewItem {
		card = result.Catalog
	}

	if in.From == resolveFromCatalog {
		return h.acceptCard(ctx, storageID, card, in.Variant)
	}

	product := &store.ResolvedProduct{
		Name:       strings.TrimSpace(in.Name),
		ItemType:   store.ItemType(in.ItemType),
		CategoryID: in.CategoryID,
	}
	if in.MinStock != nil {
		product.MinStock = *in.MinStock
	}
	entry := &store.NewCatalogProduct{DisplayName: product.Name, ItemType: product.ItemType}

	// Declining the card that was shown and naming the product differently
	// is the only way the variant graph grows (spec 07).
	if card != nil && store.NormalizeCatalogName(card.DisplayName) != store.NormalizeCatalogName(product.Name) {
		entry.ShownID = &card.ID
	}

	var saved string
	if in.Image != nil {
		name, url, source, err := h.pictures.promoteSuggestion(ctx, storageID, *in.Image)
		switch {
		case errors.Is(err, errPictureUnavailable):
			return nil, "", ValidationFailed(map[string][]string{
				"new_product.image": {"That picture is no longer available. Pick another, or continue without one."},
			}, err)
		case err != nil:
			return nil, "", Internal(err)
		}
		saved = name
		product.ImageURL = &url
		// The catalog may only record a provider picture (spec 02), and this
		// is one: a suggestion is always a provider's, never a user's photo.
		entry.ImageURL = &source
	}

	product.Catalog = entry
	return product, saved, nil
}

// acceptCard copies the line's catalog card — or one of its variants — into a
// new product: "Add this", with no external search.
func (h *ShoppingListHandler) acceptCard(ctx context.Context, storageID uuid.UUID, card *matching.CatalogMatch, variant *string) (*store.ResolvedProduct, string, *Failure) {
	if card == nil {
		return nil, "", ValidationFailed(map[string][]string{
			"new_product.from": {"This line has no catalog description to accept."},
		}, nil)
	}

	id, name, categoryPath, itemType := card.ID, card.DisplayName, card.CategoryPath, card.ItemType
	image, icon, shelfLife := card.ImageURL, card.IconName, card.DefaultShelfLifeDays

	if variant != nil {
		// Only a variant actually offered with this card can be accepted:
		// naming any other catalog row would let a client pull in a
		// description it was never shown.
		offered := false
		for _, v := range card.Variants {
			if store.NormalizeCatalogName(v.DisplayName) == store.NormalizeCatalogName(*variant) {
				offered = true
				break
			}
		}
		if !offered {
			return nil, "", ValidationFailed(map[string][]string{
				"new_product.variant": {"That is not one of this line's suggestions."},
			}, nil)
		}
		row, err := h.store.FindCatalogProduct(ctx, *variant)
		if err != nil {
			return nil, "", FromStoreError(err, "offered catalog variant vanished")
		}
		id, name, categoryPath, itemType = row.ID, row.DisplayName, row.CategoryPath, string(row.ItemType)
		image, icon, shelfLife = row.ImageURL, row.IconName, row.DefaultShelfLifeDays
	}

	saved, url := h.pictures.promoteSource(ctx, storageID, image)
	return &store.ResolvedProduct{
		Name:                 name,
		ItemType:             store.ItemType(itemType),
		CategoryPath:         categoryPath,
		DefaultShelfLifeDays: shelfLife,
		IconName:             icon,
		ImageURL:             url,
		AcceptedCatalogID:    &id,
	}, saved, nil
}

// showCatalogImages replaces each catalog card's provider picture with the
// address it is served from here. buildItemResponse never copies the
// provider URL into a card, so a caller that skips this shows no picture —
// it cannot leak one.
func (h *ShoppingListHandler) showCatalogImages(ctx context.Context, storageID uuid.UUID, items []shoppingListItemResponse, results []matching.Result) {
	for i := range items {
		if i >= len(results) || items[i].Catalog == nil || results[i].Catalog == nil {
			continue
		}
		items[i].Catalog.ImageURL = h.pictures.cardImageURL(ctx, storageID, results[i].Catalog.ImageURL)
	}
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
			// ImageURL is left nil: the catalog holds a provider URL, which
			// must never reach a browser. showCatalogImages fills in our own.
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
