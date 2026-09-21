package httpapi

import (
	"context"
	"errors"
	"net/http"

	"github.com/google/uuid"

	"github.com/CDRO/Inventory/internal/store"
)

// The three places a navigation route can send a browser, and the only place
// any of them is written down.
//
// LoginPage is what web/static/js/api.js calls LOGIN_PAGE: the file server
// canonicalises it to "/" with a 301, so a redirected browser ends on the
// login page's real location either way, and a caller that already knows the
// SPA's behaviour sees the target it expects.
const (
	LoginPage         = "/index.html"
	StoragesPage      = "/storages.html"
	StoragesPageEmpty = "/storages.html?empty=1"
	adminPage         = "/admin"
)

// NavigationStore is what the navigation routes read: the caller's admin flag
// and the storages they belong to. Both are looked up per request; neither is
// taken from anything the client sent.
type NavigationStore interface {
	IsAdmin(ctx context.Context, userID uuid.UUID) (bool, error)
	StoragesForUser(ctx context.Context, userID uuid.UUID) ([]store.Storage, error)
}

// NavigationHandler serves the browser navigation routes — routes with no body
// that exist only to decide where a browser goes next
// (docs/specs/29-first-run-admin-guidance.md).
type NavigationHandler struct {
	store  NavigationStore
	errors *ErrorWriter
}

// NewNavigationHandler wires the navigation routes to a store.
func NewNavigationHandler(s NavigationStore, errs *ErrorWriter) *NavigationHandler {
	return &NavigationHandler{store: s, errors: errs}
}

// NoStorages answers GET /no-storages: where does a user with no storage go?
//
// It exists because the answer differs for an admin and the client may not be
// the one to work that out. On a fresh deployment the bootstrap admin belongs
// to no storage — is_admin grants the admin view, membership is still a
// storage_members row like anyone else's — so the first thing the operator
// sees is the empty state telling them to ask an admin for access. The obvious
// fix, a link rendered "if the user is an admin", needs three things
// docs/specs/03-auth-and-multi-tenancy.md forbids outright: is_admin in a JSON
// response, a client-side branch on it, and navigation to /admin present in
// the shipped JavaScript.
//
// So the decision moves to the only place allowed to make it. The client
// navigates to one neutral URL and is never told what was decided: it observes
// only its own outcome, and the Location naming the admin area is sent to
// nobody but an admin.
//
// The route needs the session gate and no other. Behind RequireAdmin it would
// answer 404 to every non-admin — which is every single user this route exists
// to route somewhere useful.
func (h *NavigationHandler) NoStorages(w http.ResponseWriter, r *http.Request) {
	user, ok := UserFrom(r.Context())
	if !ok {
		// Mounted without a session gate in front: a wiring mistake, not a
		// caller error. Refuse rather than guess who this is.
		h.errors.WriteError(w, r, Internal(errors.New("httpapi: /no-storages mounted without a session gate")))
		return
	}

	// is_admin is re-read from the database here, on every call, before
	// anything else — the same rule and the same ordering as RequireAdmin in
	// middleware.go. Never from the session row, never from the user struct,
	// never from anything the client sent. Reading it first, even on the calls
	// whose answer cannot depend on it, is what makes "re-read on every
	// request" true without a qualifying clause; revoking someone's admin
	// rights changes the very next response, rather than whenever their
	// session happens to expire.
	isAdmin, err := h.store.IsAdmin(r.Context(), user.ID)
	if err != nil {
		h.errors.WriteError(w, r, Internal(err))
		return
	}

	storages, err := h.store.StoragesForUser(r.Context(), user.ID)
	if err != nil {
		h.errors.WriteError(w, r, Internal(err))
		return
	}

	if len(storages) > 0 {
		// Admin or not: this user has somewhere to be. Redirecting an admin
		// away from the storages page is right only while they have nothing
		// there, and an admin who has since added themselves to a storage uses
		// the app like anyone else.
		http.Redirect(w, r, StoragesPage, http.StatusFound)
		return
	}

	// A paired device session is never an admin session
	// (docs/specs/12-client-api-contract.md, and RequireAdmin enforces the
	// same rule one gate over), so it must not be sent to the admin area:
	// following that Location would get it the same 404 every non-admin gets,
	// and issuing it at all would turn this route into an is_admin oracle over
	// exactly the transport the admin area excludes. It sees what a non-admin
	// sees. The spec's table has no column for session kind because it
	// describes a browser navigating; this is that sentence made true of the
	// other kind of caller too.
	session, hasSession := SessionFrom(r.Context())
	if isAdmin && hasSession && session.Kind == store.SessionBrowser {
		http.Redirect(w, r, adminPage, http.StatusFound)
		return
	}

	// ?empty=1 is set by the server for non-admins only, and says nothing
	// about admin status: it means "you have already been through
	// /no-storages, render the empty state rather than coming back". An admin
	// who types it by hand simply sees the empty state.
	http.Redirect(w, r, StoragesPageEmpty, http.StatusFound)
}

// noStore stamps Cache-Control: no-store on whatever the chain below answers.
//
// It wraps the session gate rather than living inside the handler because the
// logged-out redirect is issued by the gate, and "every response from this
// route carries no-store" (docs/specs/29-first-run-admin-guidance.md) has no
// exception for the visitor who was not logged in. A navigation answer held in
// a browser's cache would keep sending a user where they belonged one
// membership change ago. WriteError stamps the same header itself, so a
// refusal carries it whichever of the two got there first.
//
// There is deliberately no method check here. The spec's "GET only, any other
// method gets the standard 404 envelope of every unrouted request" is satisfied
// by registering the route for GET alone: a wrong verb then never matches, and
// falls through to staticHandler, which *is* the envelope of every unrouted
// request. Writing a second 404 here instead would be a copy of that answer
// kept in step with the original by hand, and the two drifting apart is how a
// route that should be invisible starts announcing itself.
func noStore(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}
