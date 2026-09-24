package httpapi

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strings"

	"github.com/google/uuid"

	"github.com/CDRO/Inventory/internal/store"
)

// The caller's own membership of one storage
// (docs/specs/34-navigation-and-start-page.md).
//
// The resource is `membership`, not `preferences`, deliberately.
// /api/me/preferences already exists for the gamification opt-in
// (docs/specs/51-gamification-scoring.md) and is per *user*; giving a
// per-storage setting the same word would invite somebody to merge the two
// into one endpoint that cannot express either correctly.
//
// **There is no user id anywhere in this route or its body.** The path is
// /api/storages/{storage_id}/membership and the body carries exactly one
// field, so there is no shape of request that names whose row to write — the
// user comes from the session and nowhere else. The route rides the
// storage-scoped sub-router in router.go, so a non-member gets the same 404
// as an unknown storage, byte for byte, from RequireStorageMember
// (docs/specs/03-auth-and-multi-tenancy.md).
//
// start_page carries no rights and is not a role. It changes where a browser
// lands and nothing else; no authorization decision anywhere reads it.

// startPages is the closed list of docs/specs/34-navigation-and-start-page.md.
//
// It is written out here as well as in the column's CHECK on purpose. Letting
// the constraint do the validating would surface an unknown value as a pgx
// error from the store, which has no field information in it and would have
// to be served as a 500 — where the spec requires 422 with a
// `fields.start_page` message. The CHECK stays as the backstop for anything
// that reaches the column by another road.
//
// Pages needing a parameter to mean anything (review.html?job=,
// stocktake.html?location=) are absent because a start page with no
// parameter would open on an error, and settings.html is absent because it
// is not a place anyone starts their day.
//
// web/static/js/nav.js carries the same list as a value→path map; the two are
// kept in step by hand, and TestStartPagesMatchTheShippedNavigationMap pins
// them together so a page added to one and not the other fails the build.
var startPages = map[string]struct{}{
	"dashboard":     {},
	"inventory":     {},
	"products":      {},
	"locations":     {},
	"shopping_list": {},
	"ingest":        {},
	"inbox":         {},
}

// DefaultStartPage is the column default, repeated for the one caller that
// never reads the column: a test or a response built without a row.
const DefaultStartPage = "dashboard"

// StartPages returns the closed list, sorted. Exported for the test that pins
// it against web/static/js/nav.js's map — the two lists are the same list, and
// a page added to one and not the other is a select box that 422s or a stored
// value the frontend cannot route.
func StartPages() []string {
	names := make([]string, 0, len(startPages))
	for name := range startPages {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// startPageList renders the closed list for a validation message, in a fixed
// order so the message is the same on every run.
func startPageList() string {
	return strings.Join(StartPages(), ", ")
}

// MembershipStore is the slice of the store this handler writes through.
type MembershipStore interface {
	SetStartPage(ctx context.Context, storageID, userID uuid.UUID, startPage string) error
}

// MembershipHandler serves the caller's own membership row.
type MembershipHandler struct {
	store  MembershipStore
	errors *ErrorWriter
}

// NewMembershipHandler wires the membership route to a store and the one
// error writer.
func NewMembershipHandler(s MembershipStore, errs *ErrorWriter) *MembershipHandler {
	return &MembershipHandler{store: s, errors: errs}
}

// membershipResponse is what a successful write answers with: the value now
// stored, and nothing else. No user id and no storage name — the caller knows
// both, and echoing an id back is how a client starts believing it may send
// one.
type membershipResponse struct {
	StartPage string `json:"start_page"`
}

// Patch serves PATCH /api/storages/{storage_id}/membership.
//
// The body is decoded strictly, so a client that invents a `user_id` field is
// told its body was refused rather than left believing the extra half did
// something. It would not have been honoured either way — the id below comes
// from the session — but a silent ignore is the version that teaches a client
// the wrong contract.
func (h *MembershipHandler) Patch(w http.ResponseWriter, r *http.Request) {
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
		StartPage string `json:"start_page"`
	}
	if failure := decodeJSONStrict(w, r, &body); failure != nil {
		h.errors.WriteError(w, r, failure)
		return
	}

	startPage := strings.TrimSpace(body.StartPage)
	if _, valid := startPages[startPage]; !valid {
		h.errors.WriteError(w, r, ValidationFailed(
			map[string][]string{"start_page": {"Choose one of: " + startPageList() + "."}},
			errors.New("httpapi: start_page not in the closed list")))
		return
	}

	err := h.store.SetStartPage(r.Context(), storageID, user.ID, startPage)
	if errors.Is(err, store.ErrNotFound) {
		// The membership gate let this request in, so the row existed when it
		// arrived and has been revoked since. The answer is the one a
		// non-member gets, which is the one an unknown storage gets.
		h.errors.WriteError(w, r, NotFound("membership removed between the gate and the write"))
		return
	}
	if err != nil {
		h.errors.WriteError(w, r, Internal(err))
		return
	}

	writeJSON(w, http.StatusOK, membershipResponse{StartPage: startPage})
}
