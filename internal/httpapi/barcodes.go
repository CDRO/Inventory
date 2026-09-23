package httpapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/CDRO/Inventory/internal/barcode"
	"github.com/CDRO/Inventory/internal/store"
)

// BarcodeStore is the slice of the store the barcode routes use
// (docs/specs/20-barcode-recall.md).
type BarcodeStore interface {
	AssociateBarcode(ctx context.Context, storageID, productID uuid.UUID, code string) (*store.ProductBarcode, error)
	DeleteProductBarcode(ctx context.Context, storageID, productID uuid.UUID, code string) error
	ProductBarcodes(ctx context.Context, storageID, productID uuid.UUID) ([]store.ProductBarcode, error)
	LookupBarcode(ctx context.Context, storageID uuid.UUID, code string) (*store.BarcodeLookup, error)
	LogBarcode(ctx context.Context, storageID uuid.UUID, code string, userID *uuid.UUID, in store.BarcodeLogInput) (*store.BarcodeLogResult, error)
	CreateProductFromBarcodeHint(ctx context.Context, storageID uuid.UUID, code string) (*store.Product, error)
	CatalogVariants(ctx context.Context, id uuid.UUID, limit int) ([]string, error)
}

// PhotoDecoder reads a barcode out of image bytes.
//
// It is an interface for one reason: a test must be able to prove that the
// 422 path writes nothing to disk without depending on a real decode
// failing. internal/barcode.Decode is the only production implementation.
type PhotoDecoder interface {
	Decode(data []byte) (string, error)
}

// pixelDecoder adapts internal/barcode's package function to PhotoDecoder.
type pixelDecoder struct{}

func (pixelDecoder) Decode(data []byte) (string, error) { return barcode.Decode(data) }

// BarcodeHandler serves docs/specs/20-barcode-recall.md: the association
// routes, the recall lookup, the quick-log sheet's one write, and the
// server-side decode fallback.
//
// # What is not here, on purpose
//
// There is no external-database lookup of any kind, and no route that could
// grow into one. docs/specs/00-overview.md's non-goal was amended in exactly
// one direction: a barcode is a **recall key** for something this system has
// already identified. Identifying something it has never seen is still
// vision-first. Nothing in this file makes an outbound call.
type BarcodeHandler struct {
	store    BarcodeStore
	decoder  PhotoDecoder
	pictures productPictures
	errors   *ErrorWriter
}

// NewBarcodeHandler wires the barcode routes. decoder may be nil, in which
// case the package decoder is used; cache and productImages may be nil, and a
// catalogue card then simply shows no picture.
func NewBarcodeHandler(s BarcodeStore, decoder PhotoDecoder, cache ImageCache, productImages PhotoStore, errs *ErrorWriter) *BarcodeHandler {
	if decoder == nil {
		decoder = pixelDecoder{}
	}
	return &BarcodeHandler{
		store:    s,
		decoder:  decoder,
		pictures: productPictures{images: productImages, cache: cache},
		errors:   errs,
	}
}

// barcodeResponse is one storage-local association.
//
// storage_id is absent: the caller named the storage in the URL, and echoing
// it back adds nothing but a second place for it to disagree.
type barcodeResponse struct {
	Barcode   string    `json:"barcode"`
	ProductID uuid.UUID `json:"product_id"`
	CreatedAt time.Time `json:"created_at"`
}

// barcodeProduct is a local recall hit: this storage's own product, plus the
// live stock the sheet opens on.
//
// Every field here is this storage's own data, which is why it may carry ids
// and counts where catalogCard may not. The two shapes are deliberately
// different types for that reason — see catalogCard's doc comment.
type barcodeProduct struct {
	ProductID            uuid.UUID  `json:"product_id"`
	Name                 string     `json:"name"`
	CategoryID           *uuid.UUID `json:"category_id"`
	ItemType             string     `json:"item_type"`
	DefaultShelfLifeDays *int       `json:"default_shelf_life_days"`
	MinStock             int        `json:"min_stock"`
	ImageURL             *string    `json:"image_url"`
	IconName             *string    `json:"icon_name"`
	CurrentStock         int        `json:"current_stock"`
}

// barcodeLookupResponse is the answer to a scan.
//
// Both keys are always present, one of them null — the same reasoning
// next_cursor follows (respond.go): a client written against "read the key,
// branch on null" cannot tell an absent key from a malformed response.
type barcodeLookupResponse struct {
	Product           *barcodeProduct `json:"product"`
	CatalogSuggestion *catalogCard    `json:"catalog_suggestion"`
}

// Associate serves POST /api/storages/{storage_id}/products/{product_id}/barcodes.
//
// Body: {"barcode": "..."}. 201 with the association, 404 for a product that
// is not in this storage (indistinguishable from one that does not exist),
// 409 for a code already mapped to another product here, 422 for a code
// outside the stored charset.
//
// **This is the write the capture-time offer's "do it now" performs, and it
// records no gamification contribution** — see store.AssociateBarcode for why
// the absence is the rule rather than an omission.
func (h *BarcodeHandler) Associate(w http.ResponseWriter, r *http.Request) {
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

	var body struct {
		Barcode string `json:"barcode"`
	}
	if failure := decodeJSONStrict(w, r, &body); failure != nil {
		h.errors.WriteError(w, r, failure)
		return
	}

	association, err := h.store.AssociateBarcode(r.Context(), storageID, productID, body.Barcode)
	if err != nil {
		h.errors.WriteError(w, r, barcodeFailure(err, "associate barcode with product "+productID.String()))
		return
	}

	writeJSON(w, http.StatusCreated, barcodeResponse{
		Barcode:   association.Barcode,
		ProductID: association.ProductID,
		CreatedAt: association.CreatedAt,
	})
}

// ListForProduct serves GET /api/storages/{storage_id}/products/{product_id}/barcodes —
// the list the product edit screen renders
// (docs/specs/16-product-maintenance.md's edit surface).
//
// A separate route rather than a field on the product detail: spec 16's
// response shape is that spec's contract, and this spec has no business
// widening it.
func (h *BarcodeHandler) ListForProduct(w http.ResponseWriter, r *http.Request) {
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

	rows, err := h.store.ProductBarcodes(r.Context(), storageID, productID)
	if err != nil {
		h.errors.WriteError(w, r, FromStoreError(err, "product not found in storage"))
		return
	}

	items := make([]barcodeResponse, 0, len(rows))
	for _, row := range rows {
		items = append(items, barcodeResponse{
			Barcode:   row.Barcode,
			ProductID: row.ProductID,
			CreatedAt: row.CreatedAt,
		})
	}
	writeJSON(w, http.StatusOK, collection[barcodeResponse]{Items: items})
}

// Delete serves DELETE /api/storages/{storage_id}/products/{product_id}/barcodes/{barcode}.
//
// The catalogue hint is untouched, which is what makes re-associating the same
// code afterwards succeed.
func (h *BarcodeHandler) Delete(w http.ResponseWriter, r *http.Request) {
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

	code := chi.URLParam(r, "barcode")
	if err := h.store.DeleteProductBarcode(r.Context(), storageID, productID, code); err != nil {
		h.errors.WriteError(w, r, FromStoreError(err, "barcode not associated with product "+productID.String()))
		return
	}
	writeJSON(w, http.StatusNoContent, nil)
}

// Lookup serves GET /api/storages/{storage_id}/barcodes/{code}.
//
// Three outcomes, in order: this storage's own product, the anonymous
// catalogue card, or the one indistinguishable 404. No stage calls Gemini,
// SerpAPI or anything else — the whole promise of the feature is that the
// second scan of anything is free.
func (h *BarcodeHandler) Lookup(w http.ResponseWriter, r *http.Request) {
	storageID, ok := StorageIDFrom(r.Context())
	if !ok {
		h.errors.WriteError(w, r, Internal(errNoStorageInContext))
		return
	}

	code := chi.URLParam(r, "code")
	hit, err := h.store.LookupBarcode(r.Context(), storageID, code)
	if err != nil {
		h.errors.WriteError(w, r, FromStoreError(err, "barcode not known in storage"))
		return
	}

	out := barcodeLookupResponse{}
	switch {
	case hit.Product != nil:
		out.Product = &barcodeProduct{
			ProductID:            hit.Product.ID,
			Name:                 hit.Product.Name,
			CategoryID:           hit.Product.CategoryID,
			ItemType:             string(hit.Product.ItemType),
			DefaultShelfLifeDays: hit.Product.DefaultShelfLifeDays,
			MinStock:             hit.Product.MinStock,
			ImageURL:             hit.Product.ImageURL,
			IconName:             hit.Product.IconName,
			CurrentStock:         hit.CurrentStock,
		}
	case hit.Catalog != nil:
		out.CatalogSuggestion = h.catalogCardFor(r.Context(), storageID, hit.Catalog)
	default:
		// A lookup with neither half is a store bug, not a miss: reporting it
		// as a 404 would hide it behind the one answer that is supposed to
		// mean "nothing here".
		h.errors.WriteError(w, r, Internal(errors.New("httpapi: barcode lookup returned neither a product nor a catalog hit")))
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// catalogCardFor renders a catalogue hit as the stage-2 card of
// docs/specs/07-shopping-list-reconciliation.md.
//
// It builds catalogCard field by field, and reuses that exact type rather than
// declaring a parallel one, so the catalogue's privacy rule is enforced by the
// same struct in both places: display fields only, no catalog id, no
// created_at, no count, no hint that another storage described this first
// (docs/specs/02-data-model.md). The image URL is our own cached address, never
// the provider URL the catalogue stores.
func (h *BarcodeHandler) catalogCardFor(ctx context.Context, storageID uuid.UUID, c *store.CatalogProduct) *catalogCard {
	variants := make([]string, 0)
	// A failure here is not worth refusing the card over: the siblings are a
	// convenience, and the card without them is still the answer to the scan.
	if names, err := h.store.CatalogVariants(ctx, c.ID, 0); err == nil {
		variants = append(variants, names...)
	}
	return &catalogCard{
		DisplayName:          c.DisplayName,
		CategoryPath:         c.CategoryPath,
		ItemType:             string(c.ItemType),
		ImageURL:             h.pictures.cardImageURL(ctx, storageID, c.ImageURL),
		IconName:             c.IconName,
		DefaultShelfLifeDays: c.DefaultShelfLifeDays,
		Variants:             variants,
	}
}

// barcodeLogRequest is the confirmed quick-log sheet
// (docs/specs/20-barcode-recall.md, "Scan-and-log").
type barcodeLogRequest struct {
	Direction      string                  `json:"direction"`
	Quantity       int                     `json:"quantity"`
	LocationID     *uuid.UUID              `json:"location_id"`
	ExpirationDate *string                 `json:"expiration_date"`
	Decrements     []barcodeLogDecrement   `json:"decrements"`
}

type barcodeLogDecrement struct {
	BatchID  uuid.UUID `json:"batch_id"`
	Quantity int       `json:"quantity"`
}

// barcodeLogResponse is what the confirm wrote.
type barcodeLogResponse struct {
	ProductID       uuid.UUID   `json:"product_id"`
	CreatedBatchID  *uuid.UUID  `json:"created_batch_id"`
	TouchedBatchIDs []uuid.UUID `json:"touched_batch_ids"`
	CurrentStock    int         `json:"current_stock"`
}

// Log serves POST /api/storages/{storage_id}/barcodes/{code}/log.
//
// **This is the only write in the scan flow, and it happens on the confirm
// tap.** Scanning a code reaches Lookup above, which is a read; nothing
// anywhere mutates inventory from the scan event itself
// (docs/specs/06-vision-shelf-ingestion.md's and
// docs/specs/09-consumption-logging.md's invariant, applied here).
//
// One endpoint for both directions, so a native client
// (docs/specs/12-client-api-contract.md) gets the same single round trip the
// PWA has. It carries Idempotency-Key like every write, via the group's
// middleware.
//
// A code this storage does not know is 404: the quick flow only logs against
// products it already has.
func (h *BarcodeHandler) Log(w http.ResponseWriter, r *http.Request) {
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

	var body barcodeLogRequest
	if failure := decodeJSONStrict(w, r, &body); failure != nil {
		h.errors.WriteError(w, r, failure)
		return
	}

	fields := map[string][]string{}
	direction := store.BarcodeLogDirection(body.Direction)
	if direction != store.BarcodeLogIn && direction != store.BarcodeLogOut {
		fields["direction"] = append(fields["direction"], `Must be "in" or "out".`)
	}
	if body.Quantity < 1 {
		fields["quantity"] = append(fields["quantity"], "Must be at least 1.")
	}
	if direction == store.BarcodeLogIn && body.LocationID == nil {
		fields["location_id"] = append(fields["location_id"], "A location is required when stocking up.")
	}

	// An expiration_date the sheet carries is a date a person saw and left or
	// changed. Present means "this is the date" — stored as 'user', because the
	// cascade in docs/specs/08-expiration-and-classification.md must never
	// overwrite a date somebody chose; absent leaves the rules to resolve a
	// 'derived' one.
	var expiration *time.Time
	if body.ExpirationDate != nil {
		parsed, err := time.Parse(time.DateOnly, *body.ExpirationDate)
		if err != nil {
			fields["expiration_date"] = append(fields["expiration_date"], "Must be a YYYY-MM-DD date.")
		} else {
			expiration = &parsed
		}
	}
	if len(fields) > 0 {
		h.errors.WriteError(w, r, ValidationFailed(fields, nil))
		return
	}

	decrements := make([]store.ConsumeBatchDecrement, 0, len(body.Decrements))
	for _, d := range body.Decrements {
		decrements = append(decrements, store.ConsumeBatchDecrement{BatchID: d.BatchID, Quantity: d.Quantity})
	}

	result, err := h.store.LogBarcode(r.Context(), storageID, chi.URLParam(r, "code"), &user.ID, store.BarcodeLogInput{
		Direction:        direction,
		Quantity:         body.Quantity,
		LocationID:       body.LocationID,
		ExpirationDate:   expiration,
		ExpirationEdited: body.ExpirationDate != nil,
		Decrements:       decrements,
	})
	if err != nil {
		h.errors.WriteError(w, r, FromStoreError(err, "log against scanned barcode"))
		return
	}

	writeJSON(w, http.StatusCreated, barcodeLogResponse{
		ProductID:       result.ProductID,
		CreatedBatchID:  result.CreatedBatchID,
		TouchedBatchIDs: result.TouchedBatchIDs,
		CurrentStock:    result.CurrentStock,
	})
}

// AcceptCatalog serves POST /api/storages/{storage_id}/barcodes/{code}/product.
//
// It turns the anonymous catalogue card a scan returned into this storage's
// own product and attaches the scanned code to it — "accepting it creates the
// storage-local product the same way a shopping-list catalog card does, then
// associates the scanned code with the new product"
// (docs/specs/20-barcode-recall.md).
//
// # Why the server has to do this
//
// The shopping list accepts a card by its catalog id, which the server holds
// against the list item. A scan has no list item, and the card a scan returns
// is forbidden from carrying that id (docs/specs/02-data-model.md) — a client
// that knew it could correlate catalogue rows across requests and start
// inferring how many storages exist. So the code-to-catalogue resolution
// happens in the store, where the id never leaves.
//
// 201 with the created product, 409 when the code already names a product
// here — there is nothing to create — and 404 when it carries no catalogue
// hint, which is the same answer the lookup gives an unknown code.
func (h *BarcodeHandler) AcceptCatalog(w http.ResponseWriter, r *http.Request) {
	storageID, ok := StorageIDFrom(r.Context())
	if !ok {
		h.errors.WriteError(w, r, Internal(errNoStorageInContext))
		return
	}

	product, err := h.store.CreateProductFromBarcodeHint(r.Context(), storageID, chi.URLParam(r, "code"))
	if err != nil {
		if errors.Is(err, store.ErrConflict) {
			h.errors.WriteError(w, r, &Failure{
				Status:  http.StatusConflict,
				Code:    CodeConflict,
				Message: "That barcode already names a product here.",
				Reason:  "barcode already resolves locally",
				Err:     err,
			})
			return
		}
		h.errors.WriteError(w, r, FromStoreError(err, "barcode carries no catalog hint"))
		return
	}

	// The same shape the lookup's local hit uses, so a client that accepts a
	// card lands on exactly the object a second scan would have given it.
	writeJSON(w, http.StatusCreated, barcodeProduct{
		ProductID:            product.ID,
		Name:                 product.Name,
		CategoryID:           product.CategoryID,
		ItemType:             string(product.ItemType),
		DefaultShelfLifeDays: product.DefaultShelfLifeDays,
		MinStock:             product.MinStock,
		ImageURL:             product.ImageURL,
		IconName:             product.IconName,
		CurrentStock:         0,
	})
}

// maxDecodeUploadBytes caps the decode fallback's upload.
//
// Lower than MaxUploadBytes on purpose. A shelf photo has to be admitted at
// full phone-camera resolution because a model reads it; this one is a
// close-up of a printed number, and every byte of it is held in RAM for the
// duration of the call — see DecodePhoto for why there is no spill to disk to
// fall back on.
const maxDecodeUploadBytes = 8 << 20

// DecodePhoto serves POST /api/storages/{storage_id}/barcodes/decode.
//
// # The photograph is never written to disk
//
// Not to /data/uploads, not to /data/cache, and — the part that is easy to get
// wrong — not to the operating system's temp directory either. That last one
// is why this route does **not** use ReadImageUpload or ParseMultipartForm:
// ParseMultipartForm spills any part above its memory limit to a file in
// os.TempDir(), which is neither of the two image areas an acceptance test
// would think to check, so the guarantee would be broken while the test
// passed. MultipartReader streams the part into a bounded in-memory buffer and
// touches no filesystem at all.
//
// There is no EXIF stripping step here, and that is not an omission: stripping
// exists so a *persisted* file does not keep the GPS coordinates, and nothing
// is persisted. The bytes are unreferenced the moment this function returns.
//
// A plain synchronous call, not a job and not an AI call: decoding a barcode
// locally is neither slow nor external (docs/specs/04-backend-api-conventions.md's
// job machinery is for the ones that are).
//
// 200 {"barcode": "..."} on a read, 422 when nothing decodable is found —
// never a guessed value and never an empty string.
func (h *BarcodeHandler) DecodePhoto(w http.ResponseWriter, r *http.Request) {
	if _, ok := StorageIDFrom(r.Context()); !ok {
		h.errors.WriteError(w, r, Internal(errNoStorageInContext))
		return
	}

	data, failure := readImageBytes(w, r, "image")
	if failure != nil {
		h.errors.WriteError(w, r, failure)
		return
	}

	code, err := h.decoder.Decode(data)
	if err != nil {
		// The decoder's own error is logged, never serialized: "no barcode
		// found" is all a caller can act on, and the rest is noise that
		// differs between library versions.
		h.errors.WriteError(w, r, &Failure{
			Status:  http.StatusUnprocessableEntity,
			Code:    CodeValidationFailed,
			Message: "No barcode could be read from that photo.",
			Fields:  map[string][]string{"image": {"No barcode could be read from that photo."}},
			Reason:  "barcode decode found nothing",
			Err:     err,
		})
		return
	}
	// Belt and braces against a decoder that reports success with nothing:
	// docs/specs/20-barcode-recall.md forbids an empty-string result, and a
	// caller that trusted one would associate "" with a product.
	if err := store.ValidateBarcode(code); err != nil {
		h.errors.WriteError(w, r, &Failure{
			Status:  http.StatusUnprocessableEntity,
			Code:    CodeValidationFailed,
			Message: "No barcode could be read from that photo.",
			Fields:  map[string][]string{"image": {"No barcode could be read from that photo."}},
			Reason:  "barcode decode produced an unusable value",
			Err:     err,
		})
		return
	}

	writeJSON(w, http.StatusOK, struct {
		Barcode string `json:"barcode"`
	}{Barcode: code})
}

// readImageBytes reads one multipart file part wholly into memory, without
// ever creating a temporary file.
//
// It is deliberately not a variant of ReadImageUpload: that function's job is
// to prepare bytes for *storage*, so it strips metadata and generates a
// filename, and it parses the form the way every storing route needs. This one
// exists for the single route whose contract is that no file is produced at
// all, and its whole implementation is the difference.
func readImageBytes(w http.ResponseWriter, r *http.Request, field string) ([]byte, *Failure) {
	r.Body = http.MaxBytesReader(w, r.Body, maxDecodeUploadBytes)

	reader, err := r.MultipartReader()
	if err != nil {
		return nil, ValidationFailed(
			map[string][]string{field: {"The upload could not be read."}},
			fmt.Errorf("open multipart reader: %w", err))
	}

	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				return nil, PayloadTooLarge()
			}
			return nil, ValidationFailed(
				map[string][]string{field: {"The upload could not be read."}},
				fmt.Errorf("read multipart part: %w", err))
		}
		if part.FormName() != field {
			_ = part.Close()
			continue
		}

		// One byte past the cap, so a file exactly at the limit is accepted
		// and anything beyond it is refused rather than silently truncated
		// into an undecodable image.
		data, err := io.ReadAll(io.LimitReader(part, maxDecodeUploadBytes+1))
		_ = part.Close()
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				return nil, PayloadTooLarge()
			}
			return nil, Internal(fmt.Errorf("read upload: %w", err))
		}
		if len(data) > maxDecodeUploadBytes {
			return nil, PayloadTooLarge()
		}
		if len(data) == 0 {
			return nil, ValidationFailed(
				map[string][]string{field: {"An image file is required."}}, nil)
		}
		return data, nil
	}

	return nil, ValidationFailed(
		map[string][]string{field: {"An image file is required."}}, nil)
}

// barcodeFailure maps the store's errors for the association route, where a
// conflict has a message worth showing and a field to hang it on.
//
// Everything else defers to FromStoreError, so the 404-not-403 rule and the
// validation mapping stay in one place.
func barcodeFailure(err error, reason string) *Failure {
	if errors.Is(err, store.ErrValidation) {
		return ValidationFailed(map[string][]string{
			"barcode": {"Must be 1–64 characters of letters, digits, and . _ / -"},
		}, err)
	}
	if errors.Is(err, store.ErrConflict) {
		return &Failure{
			Status:  http.StatusConflict,
			Code:    CodeConflict,
			Message: "That barcode already belongs to another product here.",
			Reason:  reason,
			Err:     err,
		}
	}
	return FromStoreError(err, reason)
}
