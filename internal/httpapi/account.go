package httpapi

import (
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/CDRO/Inventory/internal/auth"
)

// New-password bounds (docs/specs/14-account-self-service.md).
const (
	// minNewPassword is ten characters, with no composition rules at all.
	// Mandatory digits and symbols breed "Password1!", which is a worse
	// secret than a longer phrase somebody can actually remember.
	//
	// Deliberately stricter than minPassword, the eight-character floor spec
	// 03 puts on the password an admin types when creating an account: that
	// one is typed once, out loud, across a kitchen, and is expected to be
	// replaced through this very route. The password somebody chooses to keep
	// is held to the higher bar.
	minNewPassword = 10

	// maxNewPassword exists only so an unbounded string does not become an
	// unbounded argon2id input. docs/specs/14-account-self-service.md forbids
	// a maximum *below* 128; this is that floor, not a shorter one.
	maxNewPassword = 128
)

// AccountHandler serves the account self-service routes of
// docs/specs/14-account-self-service.md: the caller's own password and
// display name.
//
// # Nothing here is an admin surface
//
// The admin password reset lives in admin.go, behind
// RequireSession → RequireAdmin like every other admin route. These two are
// behind RequireSession alone and act only on the caller's own row, which is
// read from the session context and never from the request body — there is no
// user id to pass, so there is nothing to tamper with.
type AccountHandler struct {
	store  AuthStoreFull
	errors *ErrorWriter
	// credentials is the process-wide credential limiter, shared with
	// POST /api/auth/login and POST /api/auth/pair. Password change belongs on
	// it because it accepts password guesses from a borrowed unlocked phone:
	// a session alone is not proof of the password, which is exactly why
	// current_password is asked for.
	credentials *rateLimiter
}

// NewAccountHandler wires the self-service routes. credentials is the shared
// credential limiter built in NewRouter; it must be the same instance the
// auth and device handlers hold.
func NewAccountHandler(s AuthStoreFull, errs *ErrorWriter, credentials *rateLimiter) *AccountHandler {
	return &AccountHandler{store: s, errors: errs, credentials: credentials}
}

// ChangePassword serves POST /api/auth/password.
//
// # A wrong current_password is 422, not 401
//
// The caller *is* authenticated — their session is valid and stays valid.
// What failed is a field they submitted, so it is reported the way every
// other bad field is, with the message under `fields.current_password`. A 401
// here would tell a browser's fetch wrapper to redirect to the login page
// (web/static/js/api.js does exactly that), throwing away a perfectly good
// session because someone mistyped one box of a three-box form.
//
// The field exists so that a borrowed unlocked phone cannot silently take
// over the account, which also makes this endpoint a password oracle — hence
// the shared rate limiter.
func (h *AccountHandler) ChangePassword(w http.ResponseWriter, r *http.Request) {
	user, ok := UserFrom(r.Context())
	if !ok {
		h.errors.WriteError(w, r, Unauthorized(ReasonSessionMissing))
		return
	}
	session, ok := SessionFrom(r.Context())
	if !ok {
		h.errors.WriteError(w, r, Unauthorized(ReasonSessionMissing))
		return
	}

	// Keyed by address only, not by username.
	//
	// docs/specs/14-account-self-service.md scopes the username dimension to
	// login — "counted separately per client IP and — for login — per
	// submitted username" — and the reason shows up here. This route is
	// reached with a session, so charging the caller's username would let
	// somebody holding a borrowed phone submit ten wrong current_passwords
	// and lock the account's owner out of /api/auth/login from every address
	// for fifteen minutes, while the borrowed session went on working. That
	// turns a guard against guessing into a way to keep the real owner from
	// intervening.
	//
	// The address key is still the shared one, so these failures count
	// against the same budget login and pairing draw from.
	keys := credentialKeys(clientIP(r), "")
	if retryAfter, blocked := h.credentials.blocked(keys...); blocked {
		h.errors.WriteError(w, r, rateLimited(retryAfter, "password-change rate limit for "+user.ID.String()))
		return
	}

	var body struct {
		CurrentPassword string `json:"current_password"`
		NewPassword     string `json:"new_password"`
	}
	if failure := decodeJSON(w, r, &body); failure != nil {
		h.errors.WriteError(w, r, failure)
		return
	}

	// The new password is checked first, and deliberately does not touch the
	// limiter. A too-short new_password is a typo in a form, not a guess at a
	// secret; burning a guess counter on it would let an honest user lock
	// themselves out by fumbling the field that is not the credential.
	if fields := validateNewPassword(body.NewPassword); fields != nil {
		h.errors.WriteError(w, r, ValidationFailed(fields, nil))
		return
	}

	correct, err := auth.VerifyPassword(user.PasswordHash, body.CurrentPassword)
	if err != nil {
		// A stored hash this package did not write is corruption, not a wrong
		// password. Reporting it as a failed attempt would bury it.
		h.errors.WriteError(w, r, Internal(err))
		return
	}
	if !correct {
		h.credentials.record(keys...)
		h.errors.WriteError(w, r, ValidationFailed(map[string][]string{
			"current_password": {"That is not your current password."},
		}, nil))
		return
	}

	hash, err := auth.HashPassword(body.NewPassword)
	if err != nil {
		h.errors.WriteError(w, r, Internal(err))
		return
	}

	// One transaction: the new hash and the revocation of every other session
	// land together or not at all (store.ChangePassword). The session making
	// the request survives — signing somebody out of the tab they are looking
	// at, as a reward for improving their security, is the kind of detail
	// that stops people changing passwords at all.
	if err := h.store.ChangePassword(r.Context(), user.ID, hash, session.ID); err != nil {
		h.errors.WriteError(w, r, FromStoreError(err, "change password for "+user.ID.String()))
		return
	}
	h.credentials.reset(keys...)

	// 204, with nothing to say. There is no state a client could want back
	// from this that it does not already have, and a body would be one more
	// place for a password or a hash to end up.
	writeJSON(w, http.StatusNoContent, nil)
}

// UpdateMe serves PATCH /api/auth/me.
//
// display_name is the only field, and an unknown one is refused rather than
// ignored (docs/specs/14-account-self-service.md). Usernames are immutable,
// and `is_admin` is not something any route accepts from a non-admin caller —
// a body carrying either gets a 422 naming the field, so a client that
// believed it was setting something finds out.
func (h *AccountHandler) UpdateMe(w http.ResponseWriter, r *http.Request) {
	user, ok := UserFrom(r.Context())
	if !ok {
		h.errors.WriteError(w, r, Unauthorized(ReasonSessionMissing))
		return
	}

	// A pointer, so "field absent" and "field set to empty" are different
	// requests: the first is a PATCH that asks for nothing, the second is an
	// attempt to blank the name.
	var body struct {
		DisplayName *string `json:"display_name"`
	}
	if failure := decodeJSONStrict(w, r, &body); failure != nil {
		h.errors.WriteError(w, r, failure)
		return
	}

	if body.DisplayName == nil {
		h.errors.WriteError(w, r, ValidationFailed(map[string][]string{
			"display_name": {"A display name is required."},
		}, nil))
		return
	}

	displayName := strings.TrimSpace(*body.DisplayName)
	fields := map[string][]string{}
	switch {
	case displayName == "":
		fields["display_name"] = append(fields["display_name"], "A display name is required.")
	case utf8.RuneCountInString(displayName) > maxDisplayName:
		fields["display_name"] = append(fields["display_name"], "Must be at most 255 characters.")
	}
	if len(fields) > 0 {
		h.errors.WriteError(w, r, ValidationFailed(fields, nil))
		return
	}

	updated, err := h.store.SetDisplayName(r.Context(), user.ID, displayName)
	if err != nil {
		h.errors.WriteError(w, r, FromStoreError(err, "set display name for "+user.ID.String()))
		return
	}

	// The same renderer GET /api/auth/me uses, so this response cannot grow a
	// field that one does not have.
	writeMe(w, r, h.store, h.errors, updated)
}

// validateNewPassword checks a proposed password, returning the field map for
// a 422 or nil when it is acceptable.
//
// Shared by the self-service change here and the admin reset in admin.go, so
// the two cannot drift into disagreeing about what a valid password is.
// Counted in runes, not bytes: a passphrase in a non-Latin script is not
// shorter for being encoded in more bytes.
func validateNewPassword(password string) map[string][]string {
	switch n := utf8.RuneCountInString(password); {
	case n < minNewPassword:
		return map[string][]string{"new_password": {"Must be at least 10 characters."}}
	case n > maxNewPassword:
		return map[string][]string{"new_password": {"Must be at most 128 characters."}}
	}
	return nil
}
