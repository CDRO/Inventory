package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/CDRO/Inventory/internal/store"
)

// Confirm bounds for a walked shelf. A stocktake is one physical location, so
// both are far above anything a real one holds and far below a number that
// would make one confirm an expensive transaction — the same reasoning as
// maxIngestRows and maxConsumeRows.
const (
	maxStocktakeCounts = 500
	maxStocktakeFound  = 500
)

// StocktakeStore is the slice of the store the stocktake routes use.
type StocktakeStore interface {
	CreateBatch(ctx context.Context, storageID uuid.UUID, in store.NewBatch) (*store.Batch, error)
	LocationStocktake(ctx context.Context, storageID, locationID uuid.UUID) (*store.StocktakeSheet, error)
	ConfirmStocktake(ctx context.Context, storageID, locationID uuid.UUID, userID *uuid.UUID, counts []store.StocktakeCount, found []store.FoundStock) (*store.StocktakeResult, error)
}

// StocktakeHandler serves docs/specs/13-stocktake-and-audit.md: manual batch
// creation for stock the system never recorded, and the guided walk of one
// location.
//
// Every route is mounted behind RequireStorageMember and resolves every id in
// the URL and the body against the storage in the request context, so a
// foreign location, batch or product is ErrNotFound in the store and a 404 on
// the wire — indistinguishable from an id that never existed.
type StocktakeHandler struct {
	store  StocktakeStore
	errors *ErrorWriter
}

// NewStocktakeHandler wires the handlers to a store and the one error writer.
func NewStocktakeHandler(s StocktakeStore, errs *ErrorWriter) *StocktakeHandler {
	return &StocktakeHandler{store: s, errors: errs}
}

// CreateBatch serves POST /api/storages/{storage_id}/inventory-batches —
// "found stock": three jars on the shelf that no flow ever recorded.
//
// The log row is 'audit', never 'purchase'. Nothing was bought in this moment;
// the record is being corrected to match reality, and turnover analytics
// (docs/specs/11-reporting-and-analytics.md) must not read a correction as
// shopping.
func (h *StocktakeHandler) CreateBatch(w http.ResponseWriter, r *http.Request) {
	storageID, ok := StorageIDFrom(r.Context())
	if !ok {
		h.errors.WriteError(w, r, Internal(errNoStorageInContext))
		return
	}

	var body struct {
		ProductID      string          `json:"product_id"`
		LocationID     string          `json:"location_id"`
		Quantity       int             `json:"quantity"`
		ExpirationDate json.RawMessage `json:"expiration_date"`
	}
	if failure := decodeJSON(w, r, &body); failure != nil {
		h.errors.WriteError(w, r, failure)
		return
	}

	fields := map[string][]string{}

	productID, err := uuid.Parse(body.ProductID)
	if err != nil {
		fields["product_id"] = append(fields["product_id"], "Must be a UUID.")
	}
	locationID, err := uuid.Parse(body.LocationID)
	if err != nil {
		fields["location_id"] = append(fields["location_id"], "Must be a UUID.")
	}
	if body.Quantity < 1 || body.Quantity > maxBatchQuantity {
		// A batch is a quantity of something in a place, so it starts at one.
		// Zero belongs to the correction PATCH, which deletes the row.
		fields["quantity"] = append(fields["quantity"], "Must be a whole number of at least 1.")
	}

	date, stated, failure := parseStatedExpiration(body.ExpirationDate)
	if failure != nil {
		fields["expiration_date"] = append(fields["expiration_date"], failure...)
	}

	if len(fields) > 0 {
		h.errors.WriteError(w, r, ValidationFailed(fields, nil))
		return
	}

	source := store.ExpirationDerived
	if stated {
		source = store.ExpirationUser
	}

	created, err := h.store.CreateBatch(r.Context(), storageID, store.NewBatch{
		ProductID:        productID,
		LocationID:       locationID,
		Quantity:         body.Quantity,
		ExpirationDate:   date,
		ExpirationSource: source,
		Reason:           store.ReasonAudit,
		CreatedBy:        actingUser(r),
	})
	if err != nil {
		h.errors.WriteError(w, r, FromStoreError(err, "product or location not in this storage or nonexistent"))
		return
	}

	writeJSON(w, http.StatusCreated, newBatchResponse(*created))
}

// stocktakeSheetResponse is the sheet a person walks the shelf with.
type stocktakeSheetResponse struct {
	Location stocktakeLocation   `json:"location"`
	Batches  []stocktakeBatchRow `json:"batches"`
}

type stocktakeLocation struct {
	ID            uuid.UUID  `json:"id"`
	Name          string     `json:"name"`
	LastAuditedAt *time.Time `json:"last_audited_at"`
}

type stocktakeBatchRow struct {
	ID               uuid.UUID `json:"id"`
	ProductID        uuid.UUID `json:"product_id"`
	ProductName      string    `json:"product_name"`
	ImageURL         *string   `json:"image_url"`
	Quantity         int       `json:"quantity"`
	ExpirationDate   *string   `json:"expiration_date"`
	ExpirationSource string    `json:"expiration_source"`
}

// Sheet serves GET /api/storages/{storage_id}/locations/{id}/stocktake — the
// batches directly at this location, not its descendants: a stocktake mirrors
// one physical shelf, and a child location is its own walk.
func (h *StocktakeHandler) Sheet(w http.ResponseWriter, r *http.Request) {
	storageID, ok := StorageIDFrom(r.Context())
	if !ok {
		h.errors.WriteError(w, r, Internal(errNoStorageInContext))
		return
	}
	locationID, failure := locationIDFromPath(r)
	if failure != nil {
		h.errors.WriteError(w, r, failure)
		return
	}

	sheet, err := h.store.LocationStocktake(r.Context(), storageID, locationID)
	if err != nil {
		h.errors.WriteError(w, r, FromStoreError(err, "location not in this storage or nonexistent"))
		return
	}

	rows := make([]stocktakeBatchRow, 0, len(sheet.Batches))
	for _, b := range sheet.Batches {
		row := stocktakeBatchRow{
			ID:               b.ID,
			ProductID:        b.ProductID,
			ProductName:      b.ProductName,
			ImageURL:         b.ImageURL,
			Quantity:         b.Quantity,
			ExpirationSource: string(b.ExpirationSource),
		}
		if b.ExpirationDate != nil {
			// A DATE column is a calendar day, not an instant — the same
			// reasoning as newBatchResponse.
			day := b.ExpirationDate.Format(time.DateOnly)
			row.ExpirationDate = &day
		}
		rows = append(rows, row)
	}

	writeJSON(w, http.StatusOK, stocktakeSheetResponse{
		Location: stocktakeLocation{
			ID:            sheet.Location.ID,
			Name:          sheet.Location.Name,
			LastAuditedAt: sheet.Location.LastAuditedAt,
		},
		Batches: rows,
	})
}

// stocktakeConfirmRequest is the body of POST .../locations/{id}/stocktake.
//
// batches is a pointer so an absent key is distinguishable from an empty
// array. They mean different things: an empty array is the correct statement
// for a shelf that holds nothing, while an absent one is a caller who has not
// decided anything — and the exact-row-set rule cannot treat silence as a
// decision.
type stocktakeConfirmRequest struct {
	Batches *[]stocktakeCountItem `json:"batches"`
	Found   []stocktakeFoundItem  `json:"found"`
}

type stocktakeCountItem struct {
	BatchID  string `json:"batch_id"`
	Quantity int    `json:"quantity"`
}

type stocktakeFoundItem struct {
	ProductID      string          `json:"product_id"`
	Quantity       int             `json:"quantity"`
	ExpirationDate json.RawMessage `json:"expiration_date"`
}

// stocktakeConfirmResponse reports what the walk wrote.
type stocktakeConfirmResponse struct {
	LastAuditedAt   time.Time   `json:"last_audited_at"`
	Corrected       int         `json:"corrected"`
	CreatedBatchIDs []uuid.UUID `json:"created_batch_ids"`
}

// Confirm serves POST /api/storages/{storage_id}/locations/{id}/stocktake.
//
// The body states a decision for every batch the location currently holds,
// plus any found stock, and the whole thing lands in one transaction. A
// batches array that does not exactly match the shelf is 422 and writes
// nothing — never a silent skip, which on a flow whose purpose is correctness
// would quietly discard another member's change
// (docs/specs/09-consumption-logging.md's exact-row-set rule).
func (h *StocktakeHandler) Confirm(w http.ResponseWriter, r *http.Request) {
	storageID, ok := StorageIDFrom(r.Context())
	if !ok {
		h.errors.WriteError(w, r, Internal(errNoStorageInContext))
		return
	}
	locationID, failure := locationIDFromPath(r)
	if failure != nil {
		h.errors.WriteError(w, r, failure)
		return
	}

	var body stocktakeConfirmRequest
	if failure := decodeJSON(w, r, &body); failure != nil {
		h.errors.WriteError(w, r, failure)
		return
	}

	counts, found, failure := parseStocktakeConfirm(body)
	if failure != nil {
		h.errors.WriteError(w, r, failure)
		return
	}

	result, err := h.store.ConfirmStocktake(r.Context(), storageID, locationID, actingUser(r), counts, found)
	if err != nil {
		// A store validation error here is the exact-row-set refusal: the
		// counted ids are not the ones on the shelf. The message says what to
		// do about it; the store's own wording reaches the log and, in dev,
		// debug_reason — the serializer decides that, as always.
		failure := FromStoreError(err, "location not in this storage or nonexistent")
		if failure != nil && failure.Status == http.StatusUnprocessableEntity {
			failure = ValidationFailed(map[string][]string{
				"batches": {"This shelf changed since the sheet was fetched. Reload it and count again."},
			}, err)
		}
		h.errors.WriteError(w, r, failure)
		return
	}

	writeJSON(w, http.StatusOK, stocktakeConfirmResponse{
		LastAuditedAt:   result.AuditedAt,
		Corrected:       result.Corrected,
		CreatedBatchIDs: result.CreatedBatchIDs,
	})
}

// parseStocktakeConfirm validates the body's shape only. Whether the counted
// ids are exactly the ones the location holds is the store's to decide, under
// the row locks it takes — a check made out here would read an unlocked set
// that could be stale by the time the transaction ran.
func parseStocktakeConfirm(body stocktakeConfirmRequest) ([]store.StocktakeCount, []store.FoundStock, *Failure) {
	if body.Batches == nil {
		return nil, nil, ValidationFailed(map[string][]string{
			"batches": {"A count for every batch at this location is required."},
		}, nil)
	}
	if len(*body.Batches) > maxStocktakeCounts {
		return nil, nil, ValidationFailed(map[string][]string{"batches": {"Too many batches."}}, nil)
	}
	if len(body.Found) > maxStocktakeFound {
		return nil, nil, ValidationFailed(map[string][]string{"found": {"Too many found items."}}, nil)
	}

	fields := map[string][]string{}
	add := func(group string, i int, field, msg string) {
		key := fmt.Sprintf("%s[%d].%s", group, i, field)
		fields[key] = append(fields[key], msg)
	}

	counts := make([]store.StocktakeCount, 0, len(*body.Batches))
	for i, item := range *body.Batches {
		batchID, err := uuid.Parse(item.BatchID)
		if err != nil {
			add("batches", i, "batch_id", "Must be a UUID.")
			continue
		}
		if item.Quantity < 0 || item.Quantity > maxBatchQuantity {
			add("batches", i, "quantity", "Must be a whole number of zero or more.")
			continue
		}
		counts = append(counts, store.StocktakeCount{BatchID: batchID, Quantity: item.Quantity})
	}

	found := make([]store.FoundStock, 0, len(body.Found))
	for i, item := range body.Found {
		productID, err := uuid.Parse(item.ProductID)
		if err != nil {
			add("found", i, "product_id", "Must be a UUID.")
			continue
		}
		if item.Quantity < 1 || item.Quantity > maxBatchQuantity {
			add("found", i, "quantity", "Must be a whole number of at least 1.")
			continue
		}
		date, stated, problems := parseStatedExpiration(item.ExpirationDate)
		if problems != nil {
			for _, msg := range problems {
				add("found", i, "expiration_date", msg)
			}
			continue
		}
		found = append(found, store.FoundStock{
			ProductID:        productID,
			Quantity:         item.Quantity,
			ExpirationDate:   date,
			StatedExpiration: stated,
		})
	}

	if len(fields) > 0 {
		return nil, nil, ValidationFailed(fields, nil)
	}
	return counts, found, nil
}

// parseStatedExpiration separates the three things an expiration_date field
// can say, which a plain *string collapses into two
// (docs/specs/13-stocktake-and-audit.md):
//
//   - absent — the caller said nothing, so the shelf-life rules resolve a
//     default and it is stored as 'derived'
//     (docs/specs/08-expiration-and-classification.md);
//   - explicit null — the caller stated that this does not expire, which is
//     a decision and is stored as 'user';
//   - a date — likewise 'user'.
//
// The distinction is load-bearing: the expiry cascade may recompute a
// 'derived' date and must never touch a 'user' one, so collapsing an absent
// field into an explicit null would silently freeze a date nobody chose.
func parseStatedExpiration(raw json.RawMessage) (date *time.Time, stated bool, problems []string) {
	if raw == nil {
		return nil, false, nil
	}

	var text *string
	if err := json.Unmarshal(raw, &text); err != nil {
		return nil, false, []string{"Must be a YYYY-MM-DD date or null."}
	}
	if text == nil {
		return nil, true, nil
	}

	parsed, err := time.Parse(time.DateOnly, *text)
	if err != nil {
		return nil, false, []string{"Must be a YYYY-MM-DD date or null."}
	}
	return &parsed, true, nil
}
