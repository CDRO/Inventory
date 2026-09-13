package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/CDRO/Inventory/internal/auth"
	"github.com/CDRO/Inventory/internal/store"
)

// Admin input bounds, matching the column widths in migration 00002 so an
// over-long value is a 422 naming the field rather than a driver error.
const (
	maxUsername    = 64
	maxDisplayName = 255
	maxStorageName = 255
	// minPassword is deliberately modest. This is a household server behind
	// Tailscale, and a rule that pushes people toward writing a password on a
	// sticky note on the NAS is worse than a short one they remember.
	minPassword = 8
)

// AdminStore is the slice of the store the admin JSON routes use.
type AdminStore interface {
	ListUsers(ctx context.Context) ([]store.User, error)
	CreateUser(ctx context.Context, in store.NewUser) (*store.User, error)
	DeleteUser(ctx context.Context, id uuid.UUID) error
	ListStorages(ctx context.Context) ([]store.Storage, error)
	CreateStorage(ctx context.Context, name string) (*store.Storage, error)
	DeleteStorage(ctx context.Context, id uuid.UUID) error
	ListMembers(ctx context.Context, storageID uuid.UUID) ([]store.Member, error)
	AddMember(ctx context.Context, storageID, userID uuid.UUID) error
	RemoveMember(ctx context.Context, storageID, userID uuid.UUID) error
}

// AdminHandler serves the admin JSON routes of
// docs/specs/03-auth-and-multi-tenancy.md.
//
// # These responses never carry is_admin
//
// Spec 03 is explicit that admin status is "never returned by any JSON API" —
// and that includes this one, even though it is admin-only. That is not an
// oversight in the admin user list; it is the reason the admin area is
// server-rendered at all. The HTML page at /admin can show who is an admin,
// because the server decided to render it for a caller it just re-verified.
// A JSON endpoint carrying the flag would be one more place a client could
// read it and branch on it, and the design is that no such branch exists.
//
// is_admin is accepted as *input* when creating a user. Input from an admin is
// not the leak; output to any client is.
type AdminHandler struct {
	store  AdminStore
	errors *ErrorWriter
}

// NewAdminHandler wires the admin JSON routes.
func NewAdminHandler(s AdminStore, errs *ErrorWriter) *AdminHandler {
	return &AdminHandler{store: s, errors: errs}
}

// adminUser is a user as the admin JSON API describes one. No is_admin, no
// password hash — see the type comment on AdminHandler.
type adminUser struct {
	ID          uuid.UUID `json:"id"`
	Username    string    `json:"username"`
	DisplayName string    `json:"display_name"`
}

type adminStorage struct {
	ID        uuid.UUID `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

type adminMember struct {
	UserID      uuid.UUID `json:"user_id"`
	Username    string    `json:"username"`
	DisplayName string    `json:"display_name"`
	AddedAt     time.Time `json:"added_at"`
}

// ListUsers serves GET /api/admin/users.
func (h *AdminHandler) ListUsers(w http.ResponseWriter, r *http.Request) {
	users, err := h.store.ListUsers(r.Context())
	if err != nil {
		h.errors.WriteError(w, r, Internal(err))
		return
	}
	out := make([]adminUser, 0, len(users))
	for _, u := range users {
		out = append(out, adminUser{ID: u.ID, Username: u.Username, DisplayName: u.DisplayName})
	}
	writeJSON(w, http.StatusOK, collection[adminUser]{Items: out})
}

// CreateUser serves POST /api/admin/users.
//
// This and the initial-admin bootstrap are the only two ways an account comes
// into existence. There is no public registration, by design.
func (h *AdminHandler) CreateUser(w http.ResponseWriter, r *http.Request) {
	var body createUserInput
	if failure := decodeJSON(w, r, &body); failure != nil {
		h.errors.WriteError(w, r, failure)
		return
	}

	fields := map[string][]string{}
	username := strings.TrimSpace(body.Username)
	switch {
	case username == "":
		fields["username"] = append(fields["username"], "A username is required.")
	case utf8.RuneCountInString(username) > maxUsername:
		fields["username"] = append(fields["username"], "Must be at most 64 characters.")
	}

	if utf8.RuneCountInString(body.Password) < minPassword {
		fields["password"] = append(fields["password"], "Must be at least 8 characters.")
	}

	displayName := strings.TrimSpace(body.DisplayName)
	if displayName == "" {
		displayName = username
	}
	if utf8.RuneCountInString(displayName) > maxDisplayName {
		fields["display_name"] = append(fields["display_name"], "Must be at most 255 characters.")
	}

	if len(fields) > 0 {
		h.errors.WriteError(w, r, ValidationFailed(fields, nil))
		return
	}

	hash, err := auth.HashPassword(body.Password)
	if err != nil {
		h.errors.WriteError(w, r, Internal(err))
		return
	}

	user, err := h.store.CreateUser(r.Context(), store.NewUser{
		Username: username, PasswordHash: hash, DisplayName: displayName, IsAdmin: body.IsAdmin,
	})
	if errors.Is(err, store.ErrDuplicate) {
		h.errors.WriteError(w, r, &Failure{
			Status:  http.StatusConflict,
			Code:    CodeConflict,
			Message: "That username is already taken.",
			Reason:  err.Error(),
		})
		return
	}
	if err != nil {
		h.errors.WriteError(w, r, Internal(err))
		return
	}

	writeJSON(w, http.StatusCreated, adminUser{ID: user.ID, Username: user.Username, DisplayName: user.DisplayName})
}

// DeleteUser serves DELETE /api/admin/users/{id}.
//
// Their sessions go with them in the same statement —
// sessions.user_id is ON DELETE CASCADE — which is what makes the spec's
// "revoking access immediately" true rather than eventually true: there is no
// window in which a deleted user's cookie still authenticates.
//
// An admin cannot delete themselves. Doing so from the admin page would end
// the request's own session mid-flight, and on an install with a single admin
// it would lock the household out of its own server with no way back short of
// emptying the users table.
func (h *AdminHandler) DeleteUser(w http.ResponseWriter, r *http.Request) {
	id, failure := idFromPath(r, "id", "malformed user id")
	if failure != nil {
		h.errors.WriteError(w, r, failure)
		return
	}

	if caller, ok := UserFrom(r.Context()); ok && caller.ID == id {
		h.errors.WriteError(w, r, Conflict("You cannot delete your own account.", nil))
		return
	}

	if err := h.store.DeleteUser(r.Context(), id); err != nil {
		h.errors.WriteError(w, r, FromStoreError(err, "user not found"))
		return
	}
	writeJSON(w, http.StatusNoContent, nil)
}

// ListStorages serves GET /api/admin/storages.
//
// The one place a caller may see every storage. The non-enumeration rules in
// spec 03 apply to non-admins; this route sits behind RequireAdmin, which is
// what makes a global list acceptable here and nowhere else.
func (h *AdminHandler) ListStorages(w http.ResponseWriter, r *http.Request) {
	storages, err := h.store.ListStorages(r.Context())
	if err != nil {
		h.errors.WriteError(w, r, Internal(err))
		return
	}
	out := make([]adminStorage, 0, len(storages))
	for _, s := range storages {
		out = append(out, adminStorage{ID: s.ID, Name: s.Name, CreatedAt: s.CreatedAt})
	}
	writeJSON(w, http.StatusOK, collection[adminStorage]{Items: out})
}

// CreateStorage serves POST /api/admin/storages.
//
// The new storage arrives with its starter category tree, seeded in the same
// transaction (docs/specs/08-expiration-and-classification.md).
func (h *AdminHandler) CreateStorage(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	if failure := decodeJSON(w, r, &body); failure != nil {
		h.errors.WriteError(w, r, failure)
		return
	}

	name := strings.TrimSpace(body.Name)
	fields := map[string][]string{}
	switch {
	case name == "":
		fields["name"] = append(fields["name"], "A name is required.")
	case utf8.RuneCountInString(name) > maxStorageName:
		fields["name"] = append(fields["name"], "Must be at most 255 characters.")
	}
	if len(fields) > 0 {
		h.errors.WriteError(w, r, ValidationFailed(fields, nil))
		return
	}

	storage, err := h.store.CreateStorage(r.Context(), name)
	if err != nil {
		h.errors.WriteError(w, r, Internal(err))
		return
	}
	writeJSON(w, http.StatusCreated, adminStorage{ID: storage.ID, Name: storage.Name, CreatedAt: storage.CreatedAt})
}

// DeleteStorage serves DELETE /api/admin/storages/{id}.
func (h *AdminHandler) DeleteStorage(w http.ResponseWriter, r *http.Request) {
	id, failure := idFromPath(r, "id", "malformed storage id")
	if failure != nil {
		h.errors.WriteError(w, r, failure)
		return
	}
	if err := h.store.DeleteStorage(r.Context(), id); err != nil {
		h.errors.WriteError(w, r, FromStoreError(err, "storage not found"))
		return
	}
	writeJSON(w, http.StatusNoContent, nil)
}

// ListMembers serves GET /api/admin/storages/{id}/members.
func (h *AdminHandler) ListMembers(w http.ResponseWriter, r *http.Request) {
	id, failure := idFromPath(r, "id", "malformed storage id")
	if failure != nil {
		h.errors.WriteError(w, r, failure)
		return
	}
	members, err := h.store.ListMembers(r.Context(), id)
	if err != nil {
		h.errors.WriteError(w, r, FromStoreError(err, "storage not found"))
		return
	}
	out := make([]adminMember, 0, len(members))
	for _, m := range members {
		out = append(out, adminMember{UserID: m.UserID, Username: m.Username, DisplayName: m.DisplayName, AddedAt: m.AddedAt})
	}
	writeJSON(w, http.StatusOK, collection[adminMember]{Items: out})
}

// AddMember serves POST /api/admin/storages/{id}/members.
func (h *AdminHandler) AddMember(w http.ResponseWriter, r *http.Request) {
	storageID, failure := idFromPath(r, "id", "malformed storage id")
	if failure != nil {
		h.errors.WriteError(w, r, failure)
		return
	}

	var body struct {
		UserID string `json:"user_id"`
	}
	if failure := decodeJSON(w, r, &body); failure != nil {
		h.errors.WriteError(w, r, failure)
		return
	}
	userID, err := uuid.Parse(body.UserID)
	if err != nil {
		h.errors.WriteError(w, r, ValidationFailed(map[string][]string{"user_id": {"Must be a UUID."}}, err))
		return
	}

	// Idempotent: re-adding an existing member is a 204, not a 409.
	if err := h.store.AddMember(r.Context(), storageID, userID); err != nil {
		h.errors.WriteError(w, r, FromStoreError(err, "storage or user not found"))
		return
	}
	writeJSON(w, http.StatusNoContent, nil)
}

// RemoveMember serves DELETE /api/admin/storages/{id}/members/{user_id}.
//
// Revocation is immediate: RequireStorageMember re-checks membership on every
// request, so the very next call from that user to this storage is a 404.
func (h *AdminHandler) RemoveMember(w http.ResponseWriter, r *http.Request) {
	storageID, failure := idFromPath(r, "id", "malformed storage id")
	if failure != nil {
		h.errors.WriteError(w, r, failure)
		return
	}
	userID, failure := idFromPath(r, "user_id", "malformed user id")
	if failure != nil {
		h.errors.WriteError(w, r, failure)
		return
	}
	if err := h.store.RemoveMember(r.Context(), storageID, userID); err != nil {
		h.errors.WriteError(w, r, FromStoreError(err, "membership not found"))
		return
	}
	writeJSON(w, http.StatusNoContent, nil)
}
