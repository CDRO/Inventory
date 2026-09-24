package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/CDRO/Inventory/internal/auth"
	"github.com/CDRO/Inventory/internal/store"
)

// Session lifetimes (docs/specs/03-auth-and-multi-tenancy.md).
const (
	// BrowserSessionTTL is how long a login lasts. Long enough that a
	// household is not retyping a password every week, short enough that an
	// abandoned session on a shared machine does not live forever.
	BrowserSessionTTL = 30 * 24 * time.Hour

	// DeviceSessionTTL is how long a paired native client lasts. Longer than a
	// browser session because re-pairing means physically fetching the phone
	// and scanning a QR, and because the device list gives the user a way to
	// revoke one immediately.
	DeviceSessionTTL = 365 * 24 * time.Hour
)

// AuthStoreFull is the slice of the store the auth routes need, beyond what
// the middleware already requires.
type AuthStoreFull interface {
	AuthStore

	CreateSession(ctx context.Context, userID uuid.UUID, kind store.SessionKind, label *string, ttl time.Duration) (*store.Session, error)
	DeleteSession(ctx context.Context, id string) error
	UserSessions(ctx context.Context, userID uuid.UUID) ([]store.Session, error)
	StoragesForUser(ctx context.Context, userID uuid.UUID) ([]store.Storage, error)

	// StorageMembershipsForUser is what writeMe below renders: the same
	// storages as StoragesForUser, plus the caller's own start page for each
	// (docs/specs/34-navigation-and-start-page.md). Filtered by the caller's
	// user id, so no other member's preference is reachable through it.
	StorageMembershipsForUser(ctx context.Context, userID uuid.UUID) ([]store.StorageMembership, error)
	UserByUsername(ctx context.Context, username string) (*store.User, error)
	CreatePairingCode(ctx context.Context, userID uuid.UUID) (string, error)
	RedeemPairingCode(ctx context.Context, code string) (uuid.UUID, error)

	// The credential lifecycle of docs/specs/14-account-self-service.md.
	// ChangePassword replaces the hash and revokes every session but
	// keepSessionID, in one transaction; "" keeps none, which is the admin
	// reset.
	ChangePassword(ctx context.Context, userID uuid.UUID, passwordHash, keepSessionID string) error
	SetDisplayName(ctx context.Context, userID uuid.UUID, displayName string) (*store.User, error)
}

// AuthHandler serves the session lifecycle
// (docs/specs/03-auth-and-multi-tenancy.md).
//
// # The cookie carries an opaque id and nothing else
//
// No user id, no username, no role, no flags, no signed claims. Everything
// about the caller is looked up server-side on every request, so there is
// nothing in the client's possession that could be edited to change who they
// are or what they may do. That is why there is no "refresh" or "claims"
// concept here to get wrong.
type AuthHandler struct {
	store  AuthStoreFull
	errors *ErrorWriter
	// secureCookies mirrors whether this deployment serves over HTTPS. It is
	// false only in dev, where Secure would stop the cookie working over plain
	// http://localhost entirely.
	secureCookies bool
	// credentials is the process-wide credential limiter, shared with
	// POST /api/auth/pair and POST /api/auth/password
	// (docs/specs/14-account-self-service.md).
	credentials *rateLimiter
}

// NewAuthHandler wires the auth routes. secureCookies must be false only for
// a dev deployment served over plain HTTP. credentials is the shared
// credential limiter built in NewRouter; it must be the same instance the
// device and account handlers hold.
func NewAuthHandler(s AuthStoreFull, errs *ErrorWriter, secureCookies bool, credentials *rateLimiter) *AuthHandler {
	return &AuthHandler{store: s, errors: errs, secureCookies: secureCookies, credentials: credentials}
}

// meResponse is what GET /api/auth/me returns.
//
// **There is no is_admin field, and adding one would be a security bug.**
// docs/specs/03-auth-and-multi-tenancy.md makes admin status a server-side
// fact checked per request; a client that could read it would invite a
// client-side branch on it, and the whole design is that no such branch
// exists anywhere in the shipped frontend.
//
// This is a DTO rather than the context user on purpose. store.User carries
// `json:"-"` on IsAdmin and PasswordHash, but relying on a struct tag means
// the guarantee lives somewhere a future edit could quietly remove. Building
// the response field by field means a leak has to be typed out deliberately.
type meResponse struct {
	ID          uuid.UUID    `json:"id"`
	Username    string       `json:"username"`
	DisplayName string       `json:"display_name"`
	Storages    []storageRef `json:"storages"`
}

// storageRef is one of the caller's storages.
//
// StartPage is an additive field (docs/specs/12-client-api-contract.md) and is
// the caller's own value for that storage — never another member's. It is a
// display preference: it decides where a browser lands and grants nothing
// (docs/specs/34-navigation-and-start-page.md).
type storageRef struct {
	ID        uuid.UUID `json:"id"`
	Name      string    `json:"name"`
	StartPage string    `json:"start_page"`
}

// Login serves POST /api/auth/login.
//
// A wrong username and a wrong password produce the identical response.
// Distinguishing them would turn this endpoint into a way to enumerate who
// has an account on a household's server.
//
// Rate-limited per address and per submitted username, on the counter shared
// with pairing and password change (docs/specs/14-account-self-service.md).
// The limit is checked before the password is verified, so a caller already
// over it costs this server a map lookup rather than an argon2id hash.
func (h *AuthHandler) Login(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if failure := decodeJSON(w, r, &body); failure != nil {
		h.errors.WriteError(w, r, failure)
		return
	}

	ip := clientIP(r)
	username := strings.TrimSpace(body.Username)
	keys := credentialKeys(ip, username)
	if retryAfter, blocked := h.credentials.blocked(keys...); blocked {
		h.errors.WriteError(w, r, rateLimited(retryAfter, "login rate limit for "+ip))
		return
	}

	if username == "" || body.Password == "" {
		// Charged to the address only: there is no username here to charge,
		// and counting a blank one would let anybody prime a shared "user:"
		// counter and lock out whoever submits an empty field next.
		h.credentials.record(credentialKeys(ip, "")...)
		// Still the generic refusal rather than a validation error naming the
		// empty field: an empty username is a failed login like any other, and
		// a different shape here is one more bit of signal.
		h.errors.WriteError(w, r, invalidCredentials("empty username or password"))
		return
	}

	user, err := auth.Authenticate(r.Context(), h.store, username, body.Password)
	if err != nil {
		if errors.Is(err, auth.ErrBadCredentials) {
			h.credentials.record(keys...)
			h.errors.WriteError(w, r, invalidCredentials("bad username or password"))
			return
		}
		h.errors.WriteError(w, r, Internal(err))
		return
	}

	session, err := h.store.CreateSession(r.Context(), user.ID, store.SessionBrowser, nil, BrowserSessionTTL)
	if err != nil {
		h.errors.WriteError(w, r, Internal(err))
		return
	}
	h.credentials.reset(keys...)

	http.SetCookie(w, h.sessionCookie(session.ID, session.ExpiresAt))
	writeMe(w, r, h.store, h.errors, user)
}

// Logout serves POST /api/auth/logout.
//
// The row is deleted rather than marked expired, so revocation is immediate
// and server-side: a stolen session id stops working the moment its owner
// logs out, not whenever a cached copy notices.
func (h *AuthHandler) Logout(w http.ResponseWriter, r *http.Request) {
	session, ok := SessionFrom(r.Context())
	if !ok {
		h.errors.WriteError(w, r, Unauthorized(ReasonSessionMissing))
		return
	}

	if err := h.store.DeleteSession(r.Context(), session.ID); err != nil {
		h.errors.WriteError(w, r, Internal(err))
		return
	}

	// Cleared unconditionally, even for a Bearer caller that never had one:
	// an expired cookie header is harmless to a client that ignores cookies,
	// and forgetting it for a browser would leave a dead id in the jar.
	http.SetCookie(w, h.clearedCookie())
	writeJSON(w, http.StatusNoContent, nil)
}

// Me serves GET /api/auth/me.
func (h *AuthHandler) Me(w http.ResponseWriter, r *http.Request) {
	user, ok := UserFrom(r.Context())
	if !ok {
		h.errors.WriteError(w, r, Unauthorized(ReasonSessionMissing))
		return
	}
	writeMe(w, r, h.store, h.errors, user)
}

// writeMe renders the caller and their storages.
//
// Package-level rather than a method because two handlers answer with this
// shape: GET /api/auth/me and login here, and PATCH /api/auth/me in
// account.go (docs/specs/14-account-self-service.md). One renderer means the
// no-is_admin guarantee on meResponse cannot be true of one of them and not
// the other.
//
// The storages come from StorageMembershipsForUser rather than
// StoragesForUser, so each entry carries the caller's own start page
// (docs/specs/34-navigation-and-start-page.md). The query is filtered by the
// caller's user id; there is no join here that could pick up a second
// member's row.
func writeMe(w http.ResponseWriter, r *http.Request, s AuthStoreFull, errs *ErrorWriter, user *store.User) {
	storages, err := s.StorageMembershipsForUser(r.Context(), user.ID)
	if err != nil {
		errs.WriteError(w, r, Internal(err))
		return
	}

	refs := make([]storageRef, 0, len(storages))
	for _, s := range storages {
		refs = append(refs, storageRef{ID: s.ID, Name: s.Name, StartPage: s.StartPage})
	}

	writeJSON(w, http.StatusOK, meResponse{
		ID:          user.ID,
		Username:    user.Username,
		DisplayName: user.DisplayName,
		Storages:    refs,
	})
}

// sessionCookie builds the browser session cookie.
//
// HttpOnly so no script can read it, SameSite=Lax so a cross-site form post
// cannot ride on it, Secure everywhere but a plain-HTTP dev deployment, where
// setting it would stop the cookie working at all.
func (h *AuthHandler) sessionCookie(id string, expires time.Time) *http.Cookie {
	return &http.Cookie{
		Name:     SessionCookie,
		Value:    id,
		Path:     "/",
		HttpOnly: true,
		Secure:   h.secureCookies,
		SameSite: http.SameSiteLaxMode,
		Expires:  expires,
	}
}

// clearedCookie expires the session cookie.
//
// The attributes must match the ones it was set with, or the browser treats it
// as a different cookie and leaves the original in place.
func (h *AuthHandler) clearedCookie() *http.Cookie {
	return &http.Cookie{
		Name:     SessionCookie,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   h.secureCookies,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	}
}

// invalidCredentials is the one answer to every failed login.
//
// 401 with a fixed message, whatever actually went wrong. The reason is
// recorded for the log and for dev, where the serializer may disclose it; in
// production the caller learns only that it did not work.
func invalidCredentials(reason string) *Failure {
	return &Failure{
		Status:  http.StatusUnauthorized,
		Code:    CodeUnauthorized,
		Message: "Incorrect username or password.",
		Reason:  reason,
	}
}
