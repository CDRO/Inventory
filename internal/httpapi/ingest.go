package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/CDRO/Inventory/internal/images"
	"github.com/CDRO/Inventory/internal/ingest"
	"github.com/CDRO/Inventory/internal/store"
)

// Confirm bounds. maxIngestRows is far above a real shelf and far below
// anything that would make one confirm an expensive transaction; the proposal
// the rows must match is bounded by what the model returned anyway.
const (
	maxIngestRows     = 500
	maxIngestQuantity = 100_000
	maxProductName    = 255
	maxLocationDepth  = 16
)

// Ingester starts photo ingestion (internal/ingest), and analyses a shelf or
// product photo again when asked.
type Ingester interface {
	Reanalyzer
	Start(ctx context.Context, u ingest.Upload) (*store.Job, error)
}

// IngestStore applies a reviewed proposal. Job and ProductImageInStorage are
// what taking a new product's picture from the reviewed photo needs: the
// photo and its boxes before the transaction, and which of the pictures
// written for it a product ended up using after.
type IngestStore interface {
	ConfirmIngestion(ctx context.Context, storageID, jobID uuid.UUID, userID *uuid.UUID, decisions []store.IngestDecision) (*store.IngestResult, error)
	Job(ctx context.Context, storageID, id uuid.UUID) (*store.Job, error)
	ProductImageStore
}

// IngestHandler serves the ingestion routes of
// docs/specs/06-vision-shelf-ingestion.md.
type IngestHandler struct {
	ingester Ingester
	store    IngestStore
	// photos holds the photos being reviewed, and productImages is permanent
	// storage for the product pictures cut from them. Either may be nil when
	// the upload volume is unusable; a confirm asking for a picture is then an
	// internal error, and every other confirm works as before.
	photos        PhotoStore
	productImages PhotoStore
	// cutouts holds background-removed pictures while their job is reviewed,
	// and backgrounds makes them. Both nil is a deployment without background
	// removal (docs/specs/09-consumption-logging.md): its routes are absent,
	// and a confirm naming a cutout finds none.
	cutouts     CutoutStore
	backgrounds BackgroundRemover
	errors      *ErrorWriter
}

// NewIngestHandler wires the ingestion routes.
func NewIngestHandler(i Ingester, s IngestStore, photos, productImages PhotoStore, cutouts CutoutStore, backgrounds BackgroundRemover, errs *ErrorWriter) *IngestHandler {
	return &IngestHandler{
		ingester: i, store: s, photos: photos, productImages: productImages,
		cutouts: cutouts, backgrounds: backgrounds, errors: errs,
	}
}

// Where a new product's picture comes from, as a confirm names it. Crop and
// photo are taken from the photo being reviewed and cost no further AI call
// (docs/specs/05-frontend-pwa-foundations.md, "Shared review component"); a
// cutout is one of those with its background removed, made before the confirm
// by POST .../cutouts so the reviewer saw it first.
const (
	productImageCrop   = "crop"   // the row's own bounding_box
	productImagePhoto  = "photo"  // the whole photo — for a single-product photo, usually the right one
	productImageCutout = "cutout" // a background-removed picture, named by cutout_id
)

// errProductImagesUnavailable is logged when a confirm asks for a picture but
// the upload volume could not be opened at startup (cmd/inventory/main.go).
var errProductImagesUnavailable = errors.New("httpapi: product pictures unavailable: upload volume unusable")

// productImageChoice is one accepted row that asked for a picture: which
// decision it is, from where, and — for a cutout — which one.
type productImageChoice struct {
	index  int
	source string
	cutout uuid.UUID
}

// ShelfPhoto serves POST /api/storages/{storage_id}/ingest/shelf-photos.
func (h *IngestHandler) ShelfPhoto(w http.ResponseWriter, r *http.Request) {
	h.upload(w, r, store.JobShelfIngestion)
}

// ProductPhoto serves POST /api/storages/{storage_id}/ingest/product-photos.
func (h *IngestHandler) ProductPhoto(w http.ResponseWriter, r *http.Request) {
	h.upload(w, r, store.JobProductPhoto)
}

// upload accepts one photo and answers 202 with its job id at once.
//
// The model check comes before the body is read: a deployment whose model has
// been withdrawn says so immediately, as a configuration problem for the
// admin, instead of taking the user's 20MB photo and failing it later
// (docs/specs/01-architecture-and-deployment.md).
func (h *IngestHandler) upload(w http.ResponseWriter, r *http.Request, kind store.JobKind) {
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

	if model, available := h.ingester.Available(r.Context()); !available {
		h.errors.WriteError(w, r, ModelUnavailable(model))
		return
	}

	// Metadata is stripped here, before the photo is ever written: the photo
	// on disk is already the clean one (docs/specs/04-backend-api-conventions.md).
	image, failure := ReadImageUpload(w, r, "image")
	if failure != nil {
		h.errors.WriteError(w, r, failure)
		return
	}

	var hint *uuid.UUID
	if raw := strings.TrimSpace(r.FormValue("location_id")); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			// A malformed id names nothing, and is refused the same way as a
			// well-formed one from another storage.
			h.errors.WriteError(w, r, NotFound("malformed location hint"))
			return
		}
		hint = &id
	}

	job, err := h.ingester.Start(r.Context(), ingest.Upload{
		StorageID:      storageID,
		Kind:           kind,
		CreatedBy:      user.ID,
		LocationHintID: hint,
		Filename:       image.Filename,
		Image:          image.Data,
	})
	if err != nil {
		h.errors.WriteError(w, r, FromStoreError(err, "location hint not found in storage"))
		return
	}

	writeJSON(w, http.StatusAccepted, struct {
		JobID uuid.UUID `json:"job_id"`
	}{JobID: job.ID})
}

// confirmRequest is the body of POST .../ingest/{job_id}/confirm.
type confirmRequest struct {
	Items *[]confirmItem `json:"items"`
}

type confirmItem struct {
	RowID    string `json:"row_id"`
	Decision string `json:"decision"`

	ProductID  *uuid.UUID `json:"product_id"`
	NewProduct *struct {
		Name       string     `json:"name"`
		CategoryID *uuid.UUID `json:"category_id"`
		ItemType   string     `json:"item_type"`
		// Image is "crop" or "photo" to give the new product a picture taken
		// from the photo under review, or "cutout" for one of those with its
		// background removed; absent or null for none.
		Image *string `json:"image"`
		// CutoutID names the cutout when Image is "cutout", as POST
		// .../cutouts returned it.
		CutoutID *uuid.UUID `json:"cutout_id"`
	} `json:"new_product"`

	Quantity int `json:"quantity"`

	LocationID *uuid.UUID `json:"location_id"`
	// NewLocation creates nodes the proposal marked as proposed: Names, root to
	// leaf, below ParentID (null for the storage's top level). Existing nodes
	// with the same name are reused.
	NewLocation *struct {
		ParentID *uuid.UUID `json:"parent_id"`
		Names    []string   `json:"names"`
	} `json:"new_location"`

	// ExpirationDate distinguishes absent from null. Absent accepts the
	// default and the batch gets a derived date; present — a date, or null for
	// "does not expire" — is the reviewer's own and recorded as a user date.
	ExpirationDate json.RawMessage `json:"expiration_date"`
}

// Confirm serves POST /api/storages/{storage_id}/ingest/{job_id}/confirm.
//
// The body decides every proposed row, accept or reject. Nothing is written to
// inventory before this call, and everything it writes lands in one
// transaction with the job marked consumed (docs/specs/06-vision-shelf-ingestion.md).
func (h *IngestHandler) Confirm(w http.ResponseWriter, r *http.Request) {
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
	jobID, failure := idFromPath(r, "job_id", "malformed job id")
	if failure != nil {
		h.errors.WriteError(w, r, failure)
		return
	}

	var body confirmRequest
	if failure := decodeJSON(w, r, &body); failure != nil {
		h.errors.WriteError(w, r, failure)
		return
	}

	decisions, choices, failure := parseDecisions(body)
	if failure != nil {
		h.errors.WriteError(w, r, failure)
		return
	}

	written, failure := h.writeProductImages(r.Context(), storageID, jobID, decisions, choices)
	if failure != nil {
		h.errors.WriteError(w, r, failure)
		return
	}

	userID := user.ID
	result, err := h.store.ConfirmIngestion(r.Context(), storageID, jobID, &userID, decisions)
	// Whatever the outcome, no picture written above may outlive the confirm
	// without a product using it.
	h.discardUnusedProductImages(r.Context(), storageID, written, err == nil)
	if err == nil {
		// Every cutout the review made is either a product picture now or
		// was not wanted: none is needed once the proposal is applied.
		h.removeCutouts(r.Context(), jobID)
	}
	switch {
	case errors.Is(err, store.ErrValidation):
		h.errors.WriteError(w, r, ValidationFailed(map[string][]string{
			"items": {"Every proposed row needs exactly one decision, and only proposed rows may be decided."},
		}, err))
		return
	case errors.Is(err, store.ErrConflict):
		h.errors.WriteError(w, r, conflictAs(err, "", "This proposal has already been confirmed, or is not ready for review."))
		return
	case err != nil:
		h.errors.WriteError(w, r, FromStoreError(err, "job or referenced id not found in storage"))
		return
	}

	// created_product_ids is additive to the shape
	// docs/specs/06-vision-shelf-ingestion.md documents, and it is there for
	// docs/specs/20-barcode-recall.md's capture-time offer: the offer applies
	// to a *new* product with no barcode yet, and products_created is a count,
	// which cannot say which product that is. Every id in it is this storage's
	// own, created by this very request.
	writeJSON(w, http.StatusOK, struct {
		BatchIDs          []uuid.UUID `json:"batch_ids"`
		ProductsCreated   int         `json:"products_created"`
		CreatedProductIDs []uuid.UUID `json:"created_product_ids"`
		LocationsCreated  int         `json:"locations_created"`
	}{result.BatchIDs, result.ProductsCreated, result.CreatedProductIDs, result.LocationsCreated})
}

// writeProductImages cuts each requested picture out of the reviewed photo,
// writes it to permanent storage, and points its decision's new product at it.
//
// Files cannot join the database transaction, so they are written first and
// the confirm follows. A picture written for a confirm that then fails — a
// double submit, a foreign category id — is removed by
// discardUnusedProductImages; a crash in between leaves an unreferenced file,
// which is unreachable (ProductImageInStorage) and costs only disk space. That
// is the safe direction: the reverse order could commit a product pointing at
// a picture that was never written.
//
// A job that is not ready for review gets no pictures cut at all: the confirm
// that follows is refused anyway, so there is nothing to spend a decode on.
func (h *IngestHandler) writeProductImages(ctx context.Context, storageID, jobID uuid.UUID, decisions []store.IngestDecision, choices []productImageChoice) ([]string, *Failure) {
	if len(choices) == 0 {
		return nil, nil
	}

	job, err := h.store.Job(ctx, storageID, jobID)
	if err != nil {
		return nil, FromStoreError(err, "job not found in storage")
	}
	if job.Status != store.JobDone {
		return nil, nil
	}

	noPhoto := func(index int) *Failure {
		return ValidationFailed(map[string][]string{
			fmt.Sprintf("items[%d].new_product.image", index): {"This proposal has no photo to take a picture from."},
		}, nil)
	}

	// An unusable upload volume is the server's problem, not the reviewer's:
	// answered as one, rather than telling them their proposal has no photo.
	if h.photos == nil || h.productImages == nil {
		return nil, Internal(errProductImagesUnavailable)
	}

	// The photo is read only when a picture is cut from it here. A cutout was
	// cut when it was made, and is still there when the photo is not.
	var (
		photo []byte
		boxes map[string]*images.Box
	)
	for _, choice := range choices {
		if choice.source == productImageCutout {
			continue
		}
		if job.ImageFilename == nil {
			return nil, noPhoto(choice.index)
		}
		photo, err = h.photos.Read(*job.ImageFilename)
		if errors.Is(err, os.ErrNotExist) {
			return nil, noPhoto(choice.index)
		}
		if err != nil {
			return nil, Internal(err)
		}
		if boxes, err = proposalBoxes(job.Payload); err != nil {
			return nil, Internal(err)
		}
		break
	}

	var written []string
	fail := func(f *Failure) ([]string, *Failure) {
		h.removeProductImages(ctx, written)
		return nil, f
	}

	for _, choice := range choices {
		d := &decisions[choice.index]

		var picture *images.Result
		if choice.source == productImageCutout {
			picture, err = h.readCutout(job.ID, choice.cutout)
			if errors.Is(err, os.ErrNotExist) {
				return fail(ValidationFailed(map[string][]string{
					fmt.Sprintf("items[%d].new_product.cutout_id", choice.index): {"This picture without its background is no longer available. Remove the background again, or keep the original."},
				}, nil))
			}
			if err != nil {
				return fail(Internal(err))
			}
		} else {
			var box *images.Box
			if choice.source == productImageCrop {
				box = boxes[d.RowID]
				if box == nil {
					return fail(ValidationFailed(map[string][]string{
						fmt.Sprintf("items[%d].new_product.image", choice.index): {"This item has no crop. Use the whole photo instead."},
					}, nil))
				}
			}

			picture, err = images.ProductImage(photo, box)
			if errors.Is(err, images.ErrEmptyCrop) {
				return fail(ValidationFailed(map[string][]string{
					fmt.Sprintf("items[%d].new_product.image", choice.index): {"This item's crop is empty. Use the whole photo instead."},
				}, nil))
			}
			if err != nil {
				return fail(Internal(err))
			}
		}

		id, err := uuid.NewV7()
		if err != nil {
			return fail(Internal(err))
		}
		name := id.String() + picture.Format.Extension()
		if err := h.productImages.Save(name, picture.Data); err != nil {
			return fail(Internal(err))
		}
		written = append(written, name)

		url := productImageURL(storageID, name)
		d.NewProduct.ImageURL = &url
	}
	return written, nil
}

// discardUnusedProductImages removes the pictures writeProductImages wrote
// that no product ended up using: all of them when the confirm failed, and
// after a success, any whose row was folded into another row's new product of
// the same name — the store creates that product once, with the first row's
// picture.
func (h *IngestHandler) discardUnusedProductImages(ctx context.Context, storageID uuid.UUID, written []string, confirmed bool) {
	if len(written) == 0 {
		return
	}
	if !confirmed {
		h.removeProductImages(ctx, written)
		return
	}

	var unused []string
	for _, name := range written {
		used, err := h.store.ProductImageInStorage(ctx, storageID, productImageURL(storageID, name))
		if err != nil {
			// Keep the file: an unreferenced picture only costs disk space,
			// and removing one a product does use would break its image.
			h.errors.Log(ctx, "checking whether a product picture is in use failed", err)
			continue
		}
		if !used {
			unused = append(unused, name)
		}
	}
	h.removeProductImages(ctx, unused)
}

func (h *IngestHandler) removeProductImages(ctx context.Context, names []string) {
	for _, name := range names {
		if err := h.productImages.Remove(name); err != nil {
			h.errors.Log(ctx, "removing an unused product picture failed", err)
		}
	}
}

// proposalBoxes reads each row's bounding_box out of a proposal, keyed by
// row_id. A row with no box — every row of a single-product photo, and any
// detection the model could not place — is absent.
func proposalBoxes(payload json.RawMessage) (map[string]*images.Box, error) {
	var p struct {
		Rows []struct {
			RowID       string      `json:"row_id"`
			BoundingBox *images.Box `json:"bounding_box"`
		} `json:"rows"`
	}
	if len(payload) > 0 {
		if err := json.Unmarshal(payload, &p); err != nil {
			return nil, fmt.Errorf("httpapi: read proposal boxes: %w", err)
		}
	}
	boxes := make(map[string]*images.Box, len(p.Rows))
	for _, row := range p.Rows {
		if row.BoundingBox != nil {
			boxes[row.RowID] = row.BoundingBox
		}
	}
	return boxes, nil
}

// parseDecisions validates the body's shape. Whether the rows match the
// proposal, and whether every id belongs to this storage, is the store's to
// decide under the job's lock.
//
// It also returns the new products that asked for a picture from the photo,
// by their index in the returned decisions.
func parseDecisions(body confirmRequest) ([]store.IngestDecision, []productImageChoice, *Failure) {
	if body.Items == nil {
		return nil, nil, ValidationFailed(map[string][]string{"items": {"A decision for every proposed row is required."}}, nil)
	}
	if len(*body.Items) > maxIngestRows {
		return nil, nil, ValidationFailed(map[string][]string{"items": {"Too many rows."}}, nil)
	}

	var choices []productImageChoice

	fields := map[string][]string{}
	add := func(i int, field, msg string) {
		key := fmt.Sprintf("items[%d].%s", i, field)
		fields[key] = append(fields[key], msg)
	}

	out := make([]store.IngestDecision, 0, len(*body.Items))
	for i, item := range *body.Items {
		d := store.IngestDecision{RowID: item.RowID}
		if item.RowID == "" {
			add(i, "row_id", "Required.")
		}

		switch item.Decision {
		case "reject":
			out = append(out, d)
			continue
		case "accept":
			d.Accept = true
		default:
			add(i, "decision", "Must be accept or reject.")
			continue
		}

		switch {
		case (item.ProductID == nil) == (item.NewProduct == nil):
			add(i, "product_id", "Give exactly one of product_id and new_product.")
		case item.ProductID != nil:
			d.ProductID = item.ProductID
		default:
			name := strings.TrimSpace(item.NewProduct.Name)
			itemType := store.ItemType(item.NewProduct.ItemType)
			switch {
			case name == "":
				add(i, "new_product.name", "A name is required.")
			case utf8.RuneCountInString(name) > maxProductName:
				add(i, "new_product.name", "Must be at most 255 characters.")
			}
			switch itemType {
			case "":
				itemType = store.ItemLongShelfLife
			case store.ItemPerishable, store.ItemLongShelfLife, store.ItemNonPerishable:
			default:
				add(i, "new_product.item_type", "Must be perishable, long_shelf_life or non_perishable.")
			}
			d.NewProduct = &store.NewIngestProduct{Name: name, CategoryID: item.NewProduct.CategoryID, ItemType: itemType}

			img, cutoutID := item.NewProduct.Image, item.NewProduct.CutoutID
			if img != nil {
				switch *img {
				case productImageCrop, productImagePhoto:
					choices = append(choices, productImageChoice{index: len(out), source: *img})
				case productImageCutout:
					if cutoutID == nil {
						add(i, "new_product.cutout_id", "Required for a cutout.")
					} else {
						choices = append(choices, productImageChoice{index: len(out), source: *img, cutout: *cutoutID})
					}
				default:
					add(i, "new_product.image", "Must be crop, photo, cutout or null.")
				}
			}
			if cutoutID != nil && (img == nil || *img != productImageCutout) {
				add(i, "new_product.cutout_id", "Only for a cutout.")
			}
		}

		if item.Quantity < 1 || item.Quantity > maxIngestQuantity {
			add(i, "quantity", "Must be a whole number of at least 1.")
		}
		d.Quantity = item.Quantity

		switch {
		case (item.LocationID == nil) == (item.NewLocation == nil):
			add(i, "location_id", "Give exactly one of location_id and new_location.")
		case item.LocationID != nil:
			d.LocationID = item.LocationID
		default:
			names := make([]string, 0, len(item.NewLocation.Names))
			for _, n := range item.NewLocation.Names {
				n = strings.TrimSpace(n)
				if n == "" || utf8.RuneCountInString(n) > maxLocationName {
					add(i, "new_location.names", "Each name must be 1 to 255 characters.")
					break
				}
				names = append(names, n)
			}
			if len(item.NewLocation.Names) == 0 || len(item.NewLocation.Names) > maxLocationDepth {
				add(i, "new_location.names", "Give between 1 and 16 names.")
			}
			d.NewLocation = &store.NewIngestLocation{ParentID: item.NewLocation.ParentID, Names: names}
		}

		if len(item.ExpirationDate) > 0 {
			date, failure := parseExpirationDate(item.ExpirationDate)
			if failure != nil {
				add(i, "expiration_date", "Must be a YYYY-MM-DD date or null.")
			}
			d.ExpirationEdited, d.ExpirationDate = true, date
		}

		out = append(out, d)
	}

	if len(fields) > 0 {
		return nil, nil, ValidationFailed(fields, nil)
	}
	return out, choices, nil
}
