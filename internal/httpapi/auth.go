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
	UserByUsername(ctx context.Context, username string) (*store.User, error)
	CreatePairingCode(ctx context.Context, userID uuid.UUID) (string, error)
	RedeemPairingCode(ctx context.Context, code string) (uuid.UUID, error)
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
}

// NewAuthHandler wires the auth routes. secureCookies must be false only for
// a dev deployment served over plain HTTP.
func NewAuthHandler(s AuthStoreFull, errs *ErrorWriter, secureCookies bool) *AuthHandler {
	return &AuthHandler{store: s, errors: errs, secureCookies: secureCookies}
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

type storageRef struct {
	ID   uuid.UUID `json:"id"`
	Name string    `json:"name"`
}

// Login serves POST /api/auth/login.
//
// A wrong username and a wrong password produce the identical response.
// Distinguishing them would turn this endpoint into a way to enumerate who
// has an account on a household's server.
func (h *AuthHandler) Login(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if failure := decodeJSON(w, r, &body); failure != nil {
		h.errors.WriteError(w, r, failure)
		return
	}

	username := strings.TrimSpace(body.Username)
	if username == "" || body.Password == "" {
		// Still the generic refusal rather than a validation error naming the
		// empty field: an empty username is a failed login like any other, and
		// a different shape here is one more bit of signal.
		h.errors.WriteError(w, r, invalidCredentials("empty username or password"))
		return
	}

	user, err := auth.Authenticate(r.Context(), h.store, username, body.Password)
	if err != nil {
		if errors.Is(err, auth.ErrBadCredentials) {
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

	http.SetCookie(w, h.sessionCookie(session.ID, session.ExpiresAt))
	h.writeMe(w, r, user)
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
	h.writeMe(w, r, user)
}

// writeMe renders the caller and their storages.
func (h *AuthHandler) writeMe(w http.ResponseWriter, r *http.Request, user *store.User) {
	storages, err := h.store.StoragesForUser(r.Context(), user.ID)
	if err != nil {
		h.errors.WriteError(w, r, Internal(err))
		return
	}

	refs := make([]storageRef, 0, len(storages))
	for _, s := range storages {
		refs = append(refs, storageRef{ID: s.ID, Name: s.Name})
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
