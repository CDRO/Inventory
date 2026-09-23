package httpapi

import (
	"context"
	"net/http"

	"github.com/google/uuid"

	"github.com/CDRO/Inventory/internal/store"
)

// BarcodePromptStore is the slice of the store the capture-time offer needs
// (docs/specs/20-barcode-recall.md, "Offering a barcode at first capture").
type BarcodePromptStore interface {
	BarcodePromptState(ctx context.Context, userID uuid.UUID) (store.BarcodePrompt, error)
	SetBarcodePromptEnabled(ctx context.Context, userID uuid.UUID, enabled bool) (store.BarcodePrompt, error)
	MarkBarcodePromptShown(ctx context.Context, userID uuid.UUID) (store.BarcodePrompt, error)
}

// BarcodePromptHandler serves the three user-scoped routes behind the
// capture-time barcode offer.
//
// # Why this is not a field on PATCH /api/auth/me
//
// docs/specs/20-barcode-recall.md is explicit: turning the offer off is its
// own route, not a new field on spec 14's profile patch. That route refuses
// unknown fields by design (decodeJSONStrict), and its contract is "display
// name and nothing else"; a preference belonging to a different spec riding
// along in it would make both specs' contracts depend on each other's edits.
//
// # Why there are three routes where the spec names one
//
// The spec defines PATCH — "turn this off", and the re-enable from
// settings.html. Two of its acceptance criteria need more than that:
//
//   - "settings.html's Account section shows the current state" needs a read,
//     hence GET.
//   - "barcode_prompt_seen_at is set exactly once, on the first time the offer
//     is shown to a user" needs a moment at which "shown" is recorded, and
//     the spec names no endpoint for it. POST .../shown is that moment. It
//     answers the question the client is asking anyway — should I show this,
//     and in which tone — and records the answer in the same statement, which
//     is what makes "never shown twice" a database condition rather than a
//     promise.
//
// Neither is a new capability: both are the minimum surface the spec's own
// criteria require.
//
// # Nothing here is scored
//
// Accepting, declining or disabling the offer writes no gamification
// contribution of any kind (docs/specs/20-barcode-recall.md). There is no call
// into the gamification package from this file or from
// store.AssociateBarcode, which is where accepting lands.
type BarcodePromptHandler struct {
	store  BarcodePromptStore
	errors *ErrorWriter
}

// NewBarcodePromptHandler wires the routes.
func NewBarcodePromptHandler(s BarcodePromptStore, errs *ErrorWriter) *BarcodePromptHandler {
	return &BarcodePromptHandler{store: s, errors: errs}
}

// barcodePromptResponse is the offer's state for the calling user.
//
// first_time is "the playful, explanatory copy applies", not "you have never
// scanned anything": it is derived from barcode_prompt_seen_at alone.
type barcodePromptResponse struct {
	Enabled   bool `json:"enabled"`
	FirstTime bool `json:"first_time"`
}

// Get serves GET /api/auth/barcode-prompt — what settings.html's Account
// section renders. Read-only: it never marks the offer as seen, because
// opening a settings page is not being offered anything.
func (h *BarcodePromptHandler) Get(w http.ResponseWriter, r *http.Request) {
	user, ok := UserFrom(r.Context())
	if !ok {
		h.errors.WriteError(w, r, Unauthorized(ReasonSessionMissing))
		return
	}

	state, err := h.store.BarcodePromptState(r.Context(), user.ID)
	if err != nil {
		h.errors.WriteError(w, r, FromStoreError(err, "read barcode prompt state for "+user.ID.String()))
		return
	}
	writeJSON(w, http.StatusOK, barcodePromptResponse{Enabled: state.Enabled, FirstTime: state.FirstTime})
}

// Patch serves PATCH /api/auth/barcode-prompt — the offer's "turn this off",
// and the re-enable from the profile.
//
// Body: {"enabled": bool}, and nothing else; an unknown field is a 422 naming
// it, so a client that believed it was setting something finds out. The route
// acts on the row the session names — there is no user id in the request to
// scope or tamper with.
//
// Turning the offer off must never disable or degrade any inventory feature.
// It does not: the only thing that reads this flag is the offer itself, and
// every association path — the product edit screen included — stays open.
func (h *BarcodePromptHandler) Patch(w http.ResponseWriter, r *http.Request) {
	user, ok := UserFrom(r.Context())
	if !ok {
		h.errors.WriteError(w, r, Unauthorized(ReasonSessionMissing))
		return
	}

	// A pointer, so an absent field is a PATCH that asks for nothing rather
	// than a silent "false" — the same distinction PATCH /api/auth/me draws.
	var body struct {
		Enabled *bool `json:"enabled"`
	}
	if failure := decodeJSONStrict(w, r, &body); failure != nil {
		h.errors.WriteError(w, r, failure)
		return
	}
	if body.Enabled == nil {
		h.errors.WriteError(w, r, ValidationFailed(map[string][]string{
			"enabled": {"A true or false value is required."},
		}, nil))
		return
	}

	state, err := h.store.SetBarcodePromptEnabled(r.Context(), user.ID, *body.Enabled)
	if err != nil {
		h.errors.WriteError(w, r, FromStoreError(err, "set barcode prompt for "+user.ID.String()))
		return
	}
	writeJSON(w, http.StatusOK, barcodePromptResponse{Enabled: state.Enabled, FirstTime: state.FirstTime})
}

// Shown serves POST /api/auth/barcode-prompt/shown — the client announcing
// that it is about to render the offer for a newly created product.
//
// The response decides both questions at once: whether the offer applies at
// all (enabled), and whether this occurrence gets the first-time copy. The
// store answers them from a single conditional UPDATE, so two qualifying
// products arriving together cannot both be told they are the first.
//
// A disabled offer answers {"enabled": false} and writes nothing at all —
// including no seen_at, since nothing was shown.
func (h *BarcodePromptHandler) Shown(w http.ResponseWriter, r *http.Request) {
	user, ok := UserFrom(r.Context())
	if !ok {
		h.errors.WriteError(w, r, Unauthorized(ReasonSessionMissing))
		return
	}

	state, err := h.store.MarkBarcodePromptShown(r.Context(), user.ID)
	if err != nil {
		h.errors.WriteError(w, r, FromStoreError(err, "mark barcode prompt shown for "+user.ID.String()))
		return
	}
	writeJSON(w, http.StatusOK, barcodePromptResponse{Enabled: state.Enabled, FirstTime: state.FirstTime})
}
