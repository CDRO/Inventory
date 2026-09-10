package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/CDRO/Inventory/internal/store"
)

// SessionCookie is the name of the browser session cookie.
const SessionCookie = "inventory_session"

// AuthStore is the slice of the store the middleware needs. Declared as an
// interface so the authorization rules can be tested without a database.
type AuthStore interface {
	LookupSession(ctx context.Context, id string) (*store.Session, error)
	UserByID(ctx context.Context, id uuid.UUID) (*store.User, error)
	IsAdmin(ctx context.Context, userID uuid.UUID) (bool, error)
	IsStorageMember(ctx context.Context, storageID, userID uuid.UUID) (bool, error)
	TouchSession(ctx context.Context, id string) error
}

type contextKey int

const (
	ctxUser contextKey = iota
	ctxSession
	ctxStorageID
)

// UserFrom returns the authenticated user. It is only populated behind
// RequireSession; the second result is false otherwise.
func UserFrom(ctx context.Context) (*store.User, bool) {
	user, ok := ctx.Value(ctxUser).(*store.User)
	return user, ok
}

// SessionFrom returns the caller's session, behind RequireSession.
func SessionFrom(ctx context.Context) (*store.Session, bool) {
	session, ok := ctx.Value(ctxSession).(*store.Session)
	return session, ok
}

// StorageIDFrom returns the storage this request is scoped to.
//
// Handlers must read the id from here and never re-parse it from the URL.
// The value in the context is the one RequireStorageMember validated; a
// handler that parsed the path itself could act on an id nothing checked.
func StorageIDFrom(ctx context.Context) (uuid.UUID, bool) {
	id, ok := ctx.Value(ctxStorageID).(uuid.UUID)
	return id, ok
}

// Middleware bundles the three authorization gates with the one error writer,
// so every refusal they produce goes through the same serializer.
type Middleware struct {
	store  AuthStore
	errors *ErrorWriter
}

// NewMiddleware wires the gates to a store and the error serializer.
func NewMiddleware(s AuthStore, errs *ErrorWriter) *Middleware {
	return &Middleware{store: s, errors: errs}
}

// sessionIDFrom extracts the session id from the request.
//
// Both transports resolve to the same sessions lookup — there is one session
// concept here, not a second auth mechanism with its own rules
// (docs/specs/12-client-api-contract.md).
//
// **The cookie wins when both are present.** Preferring the header would let
// anyone able to set a request header — a compromised extension, a proxy, a
// crafted cross-origin form — override the session of a logged-in browser and
// act as themselves inside that browser's tab, or worse, get the browser to
// act as them.
func sessionIDFrom(r *http.Request) string {
	if cookie, err := r.Cookie(SessionCookie); err == nil && cookie.Value != "" {
		return cookie.Value
	}
	header := r.Header.Get("Authorization")
	if value, found := strings.CutPrefix(header, "Bearer "); found {
		return strings.TrimSpace(value)
	}
	return ""
}

// RequireSession resolves the caller and puts them in the request context.
//
// Nothing about the caller is taken from the request beyond the opaque session
// id: the user, their name and their admin status are all looked up
// server-side, so there is nothing a client could edit to change who they are
// (docs/specs/03-auth-and-multi-tenancy.md).
func (m *Middleware) RequireSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := sessionIDFrom(r)
		if id == "" {
			m.errors.WriteError(w, r, Unauthorized(ReasonSessionMissing))
			return
		}

		session, err := m.store.LookupSession(r.Context(), id)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				// LookupSession deletes an expired row as it finds it, so an
				// unknown id and a lapsed one arrive here identically.
				m.errors.WriteError(w, r, Unauthorized(ReasonSessionExpired))
				return
			}
			m.errors.WriteError(w, r, Internal(err))
			return
		}

		user, err := m.store.UserByID(r.Context(), session.UserID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				// The account was deleted while the session lived. Sessions
				// cascade with the user, so this is a narrow race rather than
				// a normal state — refuse it as an expired session.
				m.errors.WriteError(w, r, Unauthorized(ReasonSessionExpired))
				return
			}
			m.errors.WriteError(w, r, Internal(err))
			return
		}

		// Best-effort activity tracking; throttled to at most hourly in the
		// store. A failure here must not fail the request — but it must not be
		// invisible either: a persistently failing touch (a bad migration on
		// sessions, say) would otherwise be the one store error in this file
		// nobody ever hears about.
		if err := m.store.TouchSession(r.Context(), session.ID); err != nil {
			m.errors.Log(r.Context(), "touch session failed", err)
		}

		ctx := context.WithValue(r.Context(), ctxUser, user)
		ctx = context.WithValue(ctx, ctxSession, session)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// RequireAdmin gates the admin area. Always mounted after RequireSession.
//
// **is_admin is re-queried from the database on every single request.** Not
// read from the session row, not cached on the user struct across requests,
// not taken from a header or any other client-supplied value — by design there
// is no client-supplied value to read. Anything else means revoking someone's
// admin rights does not take effect until their session expires.
//
// A non-admin receives the same `404` as an unknown resource, byte for byte.
// The admin area does not announce its own existence: a `403` would tell an
// ordinary user that `/admin` is a real thing worth attacking.
func (m *Middleware) RequireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, ok := UserFrom(r.Context())
		if !ok {
			// Reaching here without RequireSession is a wiring mistake, not a
			// caller error. Refuse identically rather than trusting anything.
			m.errors.WriteError(w, r, NotFound(ReasonAdminAreaHidden))
			return
		}

		isAdmin, err := m.store.IsAdmin(r.Context(), user.ID)
		if err != nil {
			m.errors.WriteError(w, r, Internal(err))
			return
		}
		if !isAdmin {
			m.errors.WriteError(w, r, NotFound(ReasonNotAdmin))
			return
		}

		next.ServeHTTP(w, r)
	})
}

// RequireStorageMember scopes a request to one storage. Always mounted after
// RequireSession.
//
// All three refusals — a malformed id, a storage that does not exist, and a
// storage the caller is not a member of — produce the identical `404`. Only
// the debug_reason differs, and only in dev. A response that separated them
// would confirm which ids name real storages and let a user map out
// households they cannot see (docs/specs/03-auth-and-multi-tenancy.md).
//
// The validated id goes into the request context; handlers read it from there
// rather than re-parsing the path.
func (m *Middleware) RequireStorageMember(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, ok := UserFrom(r.Context())
		if !ok {
			m.errors.WriteError(w, r, NotFound(ReasonNotStorageMember))
			return
		}

		storageID, err := uuid.Parse(chi.URLParam(r, "storage_id"))
		if err != nil {
			// A malformed id cannot name a storage, so it is a not-found like
			// any other. Answering 400 here would separate "not a uuid" from
			// "a uuid you may not see", which is a distinction worth nothing
			// to an honest client and something to a prober.
			m.errors.WriteError(w, r, NotFound(ReasonStorageNotFound))
			return
		}

		member, err := m.store.IsStorageMember(r.Context(), storageID, user.ID)
		if err != nil {
			m.errors.WriteError(w, r, Internal(err))
			return
		}
		if !member {
			// IsStorageMember cannot tell a missing storage from an
			// inaccessible one, so neither can this. The reason below is the
			// honest one: all we know is that there is no membership.
			m.errors.WriteError(w, r, NotFound(ReasonNotStorageMember))
			return
		}

		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxStorageID, storageID)))
	})
}
