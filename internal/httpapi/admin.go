package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/CDRO/Inventory/internal/auth"
	"github.com/CDRO/Inventory/internal/config"
	"github.com/CDRO/Inventory/internal/store"
	"github.com/CDRO/Inventory/internal/vision"
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
	AdminCreateUser(ctx context.Context, actor uuid.UUID, in store.NewUser) (*store.User, error)
	DeleteUser(ctx context.Context, actor, id uuid.UUID) error

	// AdminResetPassword is the admin reset of
	// docs/specs/14-account-self-service.md: the target's hash is replaced,
	// *all* of their sessions are revoked rather than all but one, and the
	// admin_audit_log row is written in the same transaction.
	//
	// The plain ChangePassword the self-service route uses is deliberately
	// **not** in this interface. It writes no audit row — someone changing
	// their own password is not an admin action — so an admin handler able to
	// reach it would be a mutating admin route with no trail, which is the one
	// failure this interface's shape exists to make impossible.
	AdminResetPassword(ctx context.Context, actor, userID uuid.UUID, passwordHash string) error
	ListStorages(ctx context.Context) ([]store.Storage, error)
	CreateStorage(ctx context.Context, actor uuid.UUID, name string) (*store.Storage, error)
	DeleteStorage(ctx context.Context, actor, id uuid.UUID) error
	ListMembers(ctx context.Context, storageID uuid.UUID) ([]store.Member, error)
	AddMember(ctx context.Context, actor, storageID, userID uuid.UUID) error
	RemoveMember(ctx context.Context, actor, storageID, userID uuid.UUID) error

	// Setting/SetSetting back the app-settings routes (docs/specs/01-architecture-and-deployment.md's
	// AI model resilience) — currently just the gemini_model override, read
	// by vision.Checker.EffectiveModel and written here.
	Setting(ctx context.Context, key string) (string, bool, error)
	SetSetting(ctx context.Context, key, value string, updatedBy uuid.UUID) error

	// SearchCatalog, CorrectCatalogShelfLife and DeleteCatalogProduct back
	// catalog moderation: catalog_products is insert-only
	// (docs/specs/02-data-model.md), so an admin correcting or removing a bad
	// entry is the only remedy.
	SearchCatalog(ctx context.Context, q string) ([]store.CatalogProduct, error)
	CorrectCatalogShelfLife(ctx context.Context, actor, catalogID uuid.UUID, days *int) (int, error)
	DeleteCatalogProduct(ctx context.Context, actor, id uuid.UUID) error

	// ListAdminAudit backs GET /admin/audit
	// (docs/specs/18-operations-and-observability.md). It is the only read of
	// admin_audit_log in the system, and there is no write, update or delete
	// method for the table anywhere: the trail is append-only because the
	// application has no other verb for it.
	ListAdminAudit(ctx context.Context, cursor string, limit int) (*store.AuditPage, error)
}

// adminActor is the caller an audited admin mutation is attributed to.
//
// It re-reads the user from the request context rather than taking an id from
// anywhere in the request, so the actor recorded in admin_audit_log is the one
// RequireSession resolved and RequireAdmin just re-verified against the
// database. The second result is false only if this ran outside the gate
// chain, which is a wiring mistake rather than a caller error — the handlers
// answer it with the same 404 the gates would have.
func adminActor(r *http.Request) (uuid.UUID, bool) {
	user, ok := UserFrom(r.Context())
	if !ok {
		return uuid.Nil, false
	}
	return user.ID, true
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
	vision AdminVisionChecker
	cfg    *config.Config
	errors *ErrorWriter
}

// NewAdminHandler wires the admin JSON routes. visionChecker and cfg may be
// nil — GetSettings/PutSettings then report model_unavailable / an empty
// model list rather than panicking, and the env-file route is not registered
// at all when cfg is nil (see Deps.Config in router.go).
func NewAdminHandler(s AdminStore, visionChecker AdminVisionChecker, cfg *config.Config, errs *ErrorWriter) *AdminHandler {
	return &AdminHandler{store: s, vision: visionChecker, cfg: cfg, errors: errs}
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
	actor, ok := adminActor(r)
	if !ok {
		h.errors.WriteError(w, r, NotFound(ReasonAdminAreaHidden))
		return
	}

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

	user, err := h.store.AdminCreateUser(r.Context(), actor, store.NewUser{
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
	actor, ok := adminActor(r)
	if !ok {
		h.errors.WriteError(w, r, NotFound(ReasonAdminAreaHidden))
		return
	}

	id, failure := idFromPath(r, "id", "malformed user id")
	if failure != nil {
		h.errors.WriteError(w, r, failure)
		return
	}

	if actor == id {
		h.errors.WriteError(w, r, Conflict("You cannot delete your own account.", nil))
		return
	}

	if err := h.store.DeleteUser(r.Context(), actor, id); err != nil {
		h.errors.WriteError(w, r, FromStoreError(err, "user not found"))
		return
	}
	writeJSON(w, http.StatusNoContent, nil)
}

// ResetPassword serves POST /api/admin/users/{id}/password.
//
// The "I forgot my password" remedy in a system with no email and therefore
// no reset-link flow: the admin is a person in the same household, and
// handing over a new temporary password in the kitchen is the intended UX
// (docs/specs/14-account-self-service.md).
//
// # Nothing special about this route's authorization
//
// It rides the RequireSession → RequireAdmin chain the whole admin group is
// registered on, so is_admin is re-queried from the database for this request
// like every other (middleware.go), and a non-admin gets the same 404 as a
// path that does not exist. There is no per-route check here to get wrong,
// and a user id that names nobody is the same 404 as one the caller may not
// touch — ChangePassword's ErrNotFound, mapped by the one store-error mapper.
//
// **All** of the target's sessions go, not all but one: the resetter cannot
// know which of them are legitimate. There is deliberately no
// must_change_password flag — at household scale the social contract
// ("change it after you log in") is enough, and the flag would add a second
// login state machine to every client for something that happens twice a
// year.
func (h *AdminHandler) ResetPassword(w http.ResponseWriter, r *http.Request) {
	actor, ok := adminActor(r)
	if !ok {
		h.errors.WriteError(w, r, NotFound(ReasonAdminAreaHidden))
		return
	}

	id, failure := idFromPath(r, "id", "malformed user id")
	if failure != nil {
		h.errors.WriteError(w, r, failure)
		return
	}

	var body struct {
		NewPassword string `json:"new_password"`
	}
	if failure := decodeJSON(w, r, &body); failure != nil {
		h.errors.WriteError(w, r, failure)
		return
	}

	if fields := validateNewPassword(body.NewPassword); fields != nil {
		h.errors.WriteError(w, r, ValidationFailed(fields, nil))
		return
	}

	hash, err := auth.HashPassword(body.NewPassword)
	if err != nil {
		h.errors.WriteError(w, r, Internal(err))
		return
	}

	if err := h.store.AdminResetPassword(r.Context(), actor, id, hash); err != nil {
		h.errors.WriteError(w, r, FromStoreError(err, "user not found"))
		return
	}

	// 204: the response echoes success and nothing else. The admin typed the
	// new password, so there is nothing to tell them about it, and a body
	// carrying it would put a live credential into a log, a proxy cache, or
	// whatever the browser does with a JSON response next.
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
	actor, ok := adminActor(r)
	if !ok {
		h.errors.WriteError(w, r, NotFound(ReasonAdminAreaHidden))
		return
	}

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

	storage, err := h.store.CreateStorage(r.Context(), actor, name)
	if err != nil {
		h.errors.WriteError(w, r, Internal(err))
		return
	}
	writeJSON(w, http.StatusCreated, adminStorage{ID: storage.ID, Name: storage.Name, CreatedAt: storage.CreatedAt})
}

// DeleteStorage serves DELETE /api/admin/storages/{id}.
func (h *AdminHandler) DeleteStorage(w http.ResponseWriter, r *http.Request) {
	actor, ok := adminActor(r)
	if !ok {
		h.errors.WriteError(w, r, NotFound(ReasonAdminAreaHidden))
		return
	}

	id, failure := idFromPath(r, "id", "malformed storage id")
	if failure != nil {
		h.errors.WriteError(w, r, failure)
		return
	}
	if err := h.store.DeleteStorage(r.Context(), actor, id); err != nil {
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
	actor, ok := adminActor(r)
	if !ok {
		h.errors.WriteError(w, r, NotFound(ReasonAdminAreaHidden))
		return
	}

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
	if err := h.store.AddMember(r.Context(), actor, storageID, userID); err != nil {
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
	actor, ok := adminActor(r)
	if !ok {
		h.errors.WriteError(w, r, NotFound(ReasonAdminAreaHidden))
		return
	}

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
	if err := h.store.RemoveMember(r.Context(), actor, storageID, userID); err != nil {
		h.errors.WriteError(w, r, FromStoreError(err, "membership not found"))
		return
	}
	writeJSON(w, http.StatusNoContent, nil)
}

// adminSettings is the app-settings state for GET /api/admin/settings.
type adminSettings struct {
	GeminiModel     string   `json:"gemini_model"`
	AvailableModels []string `json:"available_models"`
	Status          string   `json:"status"`
}

// GetSettings serves GET /api/admin/settings.
func (h *AdminHandler) GetSettings(w http.ResponseWriter, r *http.Request) {
	out := adminSettings{AvailableModels: []string{}, Status: vision.StatusModelUnavailable}
	if h.vision == nil {
		writeJSON(w, http.StatusOK, out)
		return
	}

	model, err := h.vision.EffectiveModel(r.Context())
	if err != nil {
		h.errors.WriteError(w, r, Internal(err))
		return
	}
	out.GeminiModel = model
	out.Status = h.vision.Status(r.Context())

	// A provider that cannot be reached is model_unavailable (Status above),
	// not a 500 (see internal/admin/admin.go's load(), which degrades the
	// same way for the same reason). Logged, unlike Status/EffectiveModel,
	// because this is the one path whose error is otherwise never seen
	// anywhere — an operator would see an empty picker with no clue why.
	if models, err := h.vision.Models(r.Context()); err == nil {
		out.AvailableModels = models
	} else {
		slog.WarnContext(r.Context(), "admin settings: could not list vision models", slog.Any("err", err))
	}

	writeJSON(w, http.StatusOK, out)
}

// PutSettings serves PUT /api/admin/settings. Body: {gemini_model}.
//
// Writing the settings-table override is what makes this apply immediately,
// no restart: vision.Checker.EffectiveModel reads the same row on every
// check (docs/specs/01-architecture-and-deployment.md).
func (h *AdminHandler) PutSettings(w http.ResponseWriter, r *http.Request) {
	user, ok := UserFrom(r.Context())
	if !ok {
		h.errors.WriteError(w, r, Unauthorized(ReasonSessionMissing))
		return
	}

	var body struct {
		GeminiModel string `json:"gemini_model"`
	}
	if failure := decodeJSON(w, r, &body); failure != nil {
		h.errors.WriteError(w, r, failure)
		return
	}

	model := strings.TrimSpace(body.GeminiModel)
	if model == "" {
		h.errors.WriteError(w, r, ValidationFailed(
			map[string][]string{"gemini_model": {"A model id is required."}}, nil))
		return
	}
	// Rejected here, not just trimmed: this value is later written verbatim
	// into a generated .env file (config.RenderEnv, EnvFile below) via
	// fmt.Sprintf. A newline in it would let an admin session inject an
	// extra line into that file — e.g. a second ADMIN_INITIAL_USERNAME= —
	// that an operator could redeploy from without noticing.
	if strings.ContainsAny(model, "\n\r") {
		h.errors.WriteError(w, r, ValidationFailed(
			map[string][]string{"gemini_model": {"Cannot contain line breaks."}}, nil))
		return
	}

	if err := h.store.SetSetting(r.Context(), vision.SettingsModelKey, model, user.ID); err != nil {
		h.errors.WriteError(w, r, Internal(err))
		return
	}
	if h.vision != nil {
		// Drop the cached model list so the very next Status/EffectiveModel
		// check reflects this write — "applied immediately, no restart" is
		// the whole point of the settings-table override.
		h.vision.Invalidate()
	}
	writeJSON(w, http.StatusOK, map[string]string{"gemini_model": model})
}

// EnvFile serves GET /api/admin/settings/env-file: a regenerated .env with
// the corrected GEMINI_MODEL line, for an operator who prefers redeploying
// from config over the in-app override
// (docs/specs/01-architecture-and-deployment.md's "Admin remediation").
// Not registered at all when the router was built with no Config (see
// Deps.Config), so h.cfg is guaranteed non-nil whenever this runs.
func (h *AdminHandler) EnvFile(w http.ResponseWriter, r *http.Request) {
	model := h.cfg.GeminiModel
	if h.vision != nil {
		if effective, err := h.vision.EffectiveModel(r.Context()); err == nil && effective != "" {
			model = effective
		}
	}
	body := config.RenderEnv(h.cfg, model)

	header := w.Header()
	header.Set("Content-Type", "text/plain; charset=utf-8")
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("Content-Disposition", `attachment; filename=".env"`)
	header.Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(body))
}

// adminCatalogEntry is one catalog row as the admin moderation view
// describes it. Unlike every other surface built on catalog_products
// (docs/specs/02-data-model.md), this one carries id: PATCH and DELETE need
// something to target it by, and this route is reachable only behind
// RequireAdmin — the same "one place a caller may see" exception
// ListStorages already relies on for the same reason.
type adminCatalogEntry struct {
	ID                   uuid.UUID `json:"id"`
	DisplayName          string    `json:"display_name"`
	CategoryPath         *string   `json:"category_path"`
	ItemType             string    `json:"item_type"`
	DefaultShelfLifeDays *int      `json:"default_shelf_life_days"`
}

// SearchCatalog serves GET /api/admin/catalog?q=. An empty q returns the
// first page alphabetically, so the admin page can render a browsable table
// rather than requiring a search term before showing anything.
func (h *AdminHandler) SearchCatalog(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	rows, err := h.store.SearchCatalog(r.Context(), q)
	if err != nil {
		h.errors.WriteError(w, r, Internal(err))
		return
	}
	out := make([]adminCatalogEntry, 0, len(rows))
	for _, c := range rows {
		out = append(out, adminCatalogEntry{
			ID: c.ID, DisplayName: c.DisplayName, CategoryPath: c.CategoryPath,
			ItemType: string(c.ItemType), DefaultShelfLifeDays: c.DefaultShelfLifeDays,
		})
	}
	writeJSON(w, http.StatusOK, collection[adminCatalogEntry]{Items: out})
}

// PatchCatalog serves PATCH /api/admin/catalog/{id}. Body:
// {default_shelf_life_days} only — the one field catalog_products'
// insert-only rule (docs/specs/02-data-model.md) still permits an admin to
// change. The value is a JSON number or null, decoded by the same
// parseShelfLifeDays helper PatchCategoryShelfLife uses
// (internal/httpapi/expiry.go), so the two admin-facing shelf-life routes
// share one request shape and one set of error messages.
//
// Changing this value is a correction, not just a setting — the old number
// was wrong, so docs/specs/08-expiration-and-classification.md requires it to
// reach every batch already in the database, not only future ones. Unlike
// PatchCategoryShelfLife's storage-scoped cascade, this one crosses every
// storage: catalog_products has no storage_id, so an admin's correction to it
// is a correction for every household that picked this catalog entry.
//
// The spec calls this a "background job" because it is unbounded across
// storages in principle. In practice it is the same shape of work as the
// storage-scoped cascade — a bounded set of local UPDATEs, no external call,
// no vision API — and this project's jobs table/runner
// (internal/jobs/runner.go) exists specifically for slow *external* calls
// that must not block a request; recomputing rows from the household's own
// database is not that. So it runs inline, like the storage-scoped sibling,
// and the response carries the real count rather than a job id to poll.
//
// Running inline under the request's context means an admin who cancels, or
// whose connection drops, can stop the cascade part-way. That is safe because
// the write and the cascade are one transaction
// (store.CorrectCatalogShelfLife): a correction that does not finish is
// rolled back whole, so the entry is never left showing a value its batches
// do not follow.
func (h *AdminHandler) PatchCatalog(w http.ResponseWriter, r *http.Request) {
	actor, ok := adminActor(r)
	if !ok {
		h.errors.WriteError(w, r, NotFound(ReasonAdminAreaHidden))
		return
	}

	id, failure := idFromPath(r, "id", "malformed catalog id")
	if failure != nil {
		h.errors.WriteError(w, r, failure)
		return
	}

	var body struct {
		DefaultShelfLifeDays json.RawMessage `json:"default_shelf_life_days"`
	}
	if failure := decodeJSON(w, r, &body); failure != nil {
		h.errors.WriteError(w, r, failure)
		return
	}
	if body.DefaultShelfLifeDays == nil {
		h.errors.WriteError(w, r, ValidationFailed(map[string][]string{
			"default_shelf_life_days": {"Provide a number of days, or null to clear the override."},
		}, nil))
		return
	}

	days, failure := parseShelfLifeDays(body.DefaultShelfLifeDays)
	if failure != nil {
		h.errors.WriteError(w, r, failure)
		return
	}

	affected, err := h.store.CorrectCatalogShelfLife(r.Context(), actor, id, days)
	if err != nil {
		h.errors.WriteError(w, r, FromStoreError(err, "catalog entry not found"))
		return
	}

	writeJSON(w, http.StatusOK, struct {
		DefaultShelfLifeDays *int `json:"default_shelf_life_days"`
		RecomputedBatches    int  `json:"recomputed_batches"`
	}{DefaultShelfLifeDays: days, RecomputedBatches: affected})
}

// DeleteCatalog serves DELETE /api/admin/catalog/{id} — moderation. Never
// touches any storage's own products: products.catalog_id is
// ON DELETE SET NULL (docs/specs/02-data-model.md).
func (h *AdminHandler) DeleteCatalog(w http.ResponseWriter, r *http.Request) {
	actor, ok := adminActor(r)
	if !ok {
		h.errors.WriteError(w, r, NotFound(ReasonAdminAreaHidden))
		return
	}

	id, failure := idFromPath(r, "id", "malformed catalog id")
	if failure != nil {
		h.errors.WriteError(w, r, failure)
		return
	}
	if err := h.store.DeleteCatalogProduct(r.Context(), actor, id); err != nil {
		h.errors.WriteError(w, r, FromStoreError(err, "catalog entry not found"))
		return
	}
	writeJSON(w, http.StatusNoContent, nil)
}
