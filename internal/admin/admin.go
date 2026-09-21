// Package admin renders the server-side admin pages
// (docs/specs/03-auth-and-multi-tenancy.md).
//
// # Why this is not part of the JavaScript application
//
// Admin status is a server-side fact. The spec's answer to "how does a client
// know whether to show the admin UI" is that it never does: the frontend ships
// no admin routes, views or branches, and the admin area exists only as HTML
// the server chose to render for a caller it had just re-verified against the
// database. There is nothing in the shipped app to unlock.
//
// This package renders; it does not authorize. It is mounted behind the same
// RequireSession → RequireAdmin chain as /api/admin/*, and it has no check of
// its own to get out of step with that chain
// (docs/specs/04-backend-api-conventions.md).
//
// # Where the writes go
//
// The page's forms submit to the /api/admin/* JSON routes, through a short
// script inlined in the template. There is deliberately no second, form-encoded
// write path here: one set of handlers means one set of validation rules, and
// an HTML route that forgot a check the JSON route performs is exactly the
// divergence the shared gate chain exists to prevent. The script decides
// nothing — the page is composed server-side and every write is re-authorized
// by the API — so it stays within the spec's "form validation and visual
// helpers" allowance.
package admin

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/CDRO/Inventory/internal/store"
	"github.com/CDRO/Inventory/internal/vision"
	"github.com/CDRO/Inventory/web"
)

// Store is the slice of the store the admin pages read.
type Store interface {
	ListUsers(ctx context.Context) ([]store.User, error)
	ListStorages(ctx context.Context) ([]store.Storage, error)
	ListMembers(ctx context.Context, storageID uuid.UUID) ([]store.Member, error)
	SearchCatalog(ctx context.Context, q string) ([]store.CatalogProduct, error)
	// ListAdminAudit backs /admin/audit
	// (docs/specs/18-operations-and-observability.md). Read-only, like
	// everything else this interface names: the trail is written by the admin
	// mutations themselves, inside their own transactions, and this package
	// only renders it.
	ListAdminAudit(ctx context.Context, cursor string, limit int) (*store.AuditPage, error)
}

// VisionChecker backs the AI-model banner
// (docs/specs/01-architecture-and-deployment.md's AI model resilience). A
// nil VisionChecker (no vision provider configured at all) renders the
// banner as unavailable with an empty model list, rather than panicking.
type VisionChecker interface {
	EffectiveModel(ctx context.Context) (string, error)
	Models(ctx context.Context) ([]string, error)
	Status(ctx context.Context) string
}

// ImageModelChecker reports whether the optional GEMINI_IMAGE_MODEL is
// available, naming it (docs/specs/01-architecture-and-deployment.md). Nil is a
// deployment that never configured one, and is never warned about.
type ImageModelChecker interface {
	Available(ctx context.Context) (model string, ok bool)
}

// Handler serves the admin pages.
type Handler struct {
	store      Store
	vision     VisionChecker
	imageModel ImageModelChecker
	page       *template.Template
	// audit is the trail page (docs/specs/18-operations-and-observability.md).
	//
	// Its own *template.Template rather than a second file parsed into the
	// same set: ParseFS into one set leaves Execute picking whichever template
	// was parsed last, so two pages in one set is a way to serve the wrong
	// page and never hear about it.
	audit *template.Template
	// version is the build string in the footer of both pages, so an operator
	// can see what is running without leaving the UI they are already in.
	version string
	// currentUser names the caller, so the page can withhold the delete action
	// from their own row. It is a callback rather than an import because the
	// session lives in the httpapi package, which mounts this one.
	currentUser func(*http.Request) (uuid.UUID, bool)
	// fail reports an unexpected error. It is the httpapi error serializer, so
	// this package cannot produce an error body of its own — the single
	// serializer rule holds here too.
	fail func(http.ResponseWriter, *http.Request, error)
}

// New parses the admin templates. It fails at startup, not on the first
// request, if one is missing or malformed.
func New(
	s Store,
	visionChecker VisionChecker,
	imageModel ImageModelChecker,
	version string,
	currentUser func(*http.Request) (uuid.UUID, bool),
	fail func(http.ResponseWriter, *http.Request, error),
) (*Handler, error) {
	tree, err := web.Templates()
	if err != nil {
		return nil, fmt.Errorf("admin: load templates: %w", err)
	}
	page, err := template.ParseFS(tree, "admin.html")
	if err != nil {
		return nil, fmt.Errorf("admin: parse template: %w", err)
	}
	audit, err := template.ParseFS(tree, "audit.html")
	if err != nil {
		return nil, fmt.Errorf("admin: parse template: %w", err)
	}
	if version == "" {
		version = devVersion
	}
	return &Handler{
		store: s, vision: visionChecker, imageModel: imageModel,
		page: page, audit: audit, version: version,
		currentUser: currentUser, fail: fail,
	}, nil
}

// devVersion mirrors httpapi.DevVersion. It is duplicated rather than imported
// because httpapi mounts this package and importing it back would be a cycle;
// the constant is one word and both call sites name the spec.
const devVersion = "dev"

type userRow struct {
	ID          uuid.UUID
	Username    string
	DisplayName string
	// IsAdmin is shown here and only here. An HTML page the server rendered for
	// a verified admin is not the "JSON API" the spec forbids it in.
	IsAdmin  bool
	IsSelf   bool
	Storages []string
}

type storageRow struct {
	ID      uuid.UUID
	Name    string
	Members []store.Member
	// Candidates are the users not yet in this storage, for the grant form.
	Candidates []store.User
}

type catalogRow struct {
	ID           uuid.UUID
	DisplayName  string
	CategoryPath string // empty when unset — the template shows a dash
	ItemType     string
	// ShelfLifeDays is pre-formatted (empty when unset) because html/template
	// cannot dereference a *int for an <input value> attribute without a
	// helper, and one string field is simpler than adding one.
	ShelfLifeDays string
}

type pageData struct {
	Nonce    string
	Version  string
	Users    []userRow
	Storages []storageRow

	// AI model resilience (docs/specs/01-architecture-and-deployment.md).
	GeminiModel      string
	AvailableModels  []string
	ModelUnavailable bool
	// ImageModelUnavailable is set only when GEMINI_IMAGE_MODEL is configured
	// and not offered: a deployment that never wanted background removal is
	// never told anything is wrong.
	ImageModel            string
	ImageModelUnavailable bool

	// Catalog moderation: CatalogQuery echoes the search box so a reload
	// (after a PATCH/DELETE, or just re-visiting the page) does not lose it.
	CatalogQuery string
	Catalog      []catalogRow
}

// Page serves GET /admin: users with their storages, storages with their
// members, and the forms that change either.
func (h *Handler) Page(w http.ResponseWriter, r *http.Request) {
	data, err := h.load(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.render(w, r, h.page, func(nonce string) any {
		data.Nonce, data.Version = nonce, h.version
		return data
	})
}

// auditRow is one audit entry as the template renders it.
//
// Details arrives as pre-formatted key=value text rather than a map, because
// html/template cannot range a map[string]any in a stable order and an audit
// page whose columns rearrange between reloads is one nobody trusts.
type auditRow struct {
	When    string
	Actor   string
	Action  string
	Target  string
	Details string
}

type auditData struct {
	Nonce   string
	Version string
	Rows    []auditRow
	// NextCursor is empty on the last page; the template shows the "older"
	// link only when it is set.
	NextCursor string
}

// Audit serves GET /admin/audit: the admin audit trail, newest first
// (docs/specs/18-operations-and-observability.md).
//
// It is mounted on the same RequireSession → RequireAdmin group as every other
// admin route, so a non-admin gets the same 404 as for a path that does not
// exist, and there is no check here of its own to fall out of step with that.
//
// There is deliberately no JSON counterpart. The trail records who did what to
// whom across tenancy boundaries — exactly the data the non-enumeration rules
// of docs/specs/03-auth-and-multi-tenancy.md keep out of the client API — and
// a read-only server-rendered table is all an operator needs.
func (h *Handler) Audit(w http.ResponseWriter, r *http.Request) {
	page, err := h.store.ListAdminAudit(r.Context(), r.URL.Query().Get("cursor"), store.AuditPageSize)
	if err != nil {
		// Includes a mangled cursor, which the store reports as
		// store.ErrValidation. It reaches the same serializer as everything
		// else; this package renders no error of its own.
		h.fail(w, r, err)
		return
	}

	data := &auditData{NextCursor: page.Next}
	for _, entry := range page.Entries {
		data.Rows = append(data.Rows, auditRow{
			When: entry.CreatedAt.Format(time.RFC3339),
			// Empty means the actor was store.SystemActor or has since been
			// deleted; the two are indistinguishable in the data and are not
			// guessed apart here.
			Actor:   orSystem(entry.ActorUsername),
			Action:  string(entry.Action),
			Target:  entry.Target,
			Details: formatDetails(entry.Details),
		})
	}

	h.render(w, r, h.audit, func(nonce string) any {
		data.Nonce, data.Version = nonce, h.version
		return data
	})
}

func orSystem(username string) string {
	if username == "" {
		return "system"
	}
	return username
}

// formatDetails renders the JSONB payload as stable "key=value" text, keys in
// alphabetical order so two reloads of the same row look identical.
func formatDetails(details map[string]any) string {
	if len(details) == 0 {
		return ""
	}
	keys := make([]string, 0, len(details))
	for k := range details {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%v", k, details[k]))
	}
	// Escaped by html/template on the way out like any other string; nothing
	// here builds markup.
	return strings.Join(parts, " ")
}

// render is the one response path both admin pages take: a fresh nonce, the
// template into a buffer, and the identical security headers.
//
// Shared rather than copied because the headers are the page's whole defence.
// A second page that rendered without the CSP, or with a stale nonce, would
// look correct in a browser and be a hole — the kind of divergence that only
// shows up when somebody goes looking. data is a callback so the nonce reaches
// the template's own struct without this helper knowing either page's shape.
func (h *Handler) render(w http.ResponseWriter, r *http.Request, tmpl *template.Template, data func(nonce string) any) {
	nonce, err := newNonce()
	if err != nil {
		h.fail(w, r, err)
		return
	}

	// Rendered into a buffer first, so a template error is still a clean 500
	// rather than half a page followed by nothing.
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data(nonce)); err != nil {
		h.fail(w, r, fmt.Errorf("admin: render: %w", err))
		return
	}

	header := w.Header()
	header.Set("Content-Type", "text/html; charset=utf-8")
	header.Set("Cache-Control", "no-store")
	// The page's one script and one style block run by nonce; nothing else
	// runs at all. frame-ancestors keeps the admin page out of anyone's iframe,
	// where its delete buttons could be clickjacked.
	//
	// form-action is 'self', not 'none': every write on this page goes through
	// the script's fetch (which preventDefault stops from ever reaching a
	// native submission, so this never mattered for those), but the catalog
	// search box is a plain native GET to /admin?q=... — a real search UX
	// that a client-side fetch would break, since a page a fetch replaced in
	// place could not be reloaded or bookmarked with the query still in the
	// URL. 'self' still refuses a form aimed at any other origin.
	header.Set("Content-Security-Policy", fmt.Sprintf(
		"default-src 'none'; script-src 'nonce-%[1]s'; style-src 'nonce-%[1]s'; "+
			"connect-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'",
		nonce))
	header.Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(http.StatusOK)
	_, _ = buf.WriteTo(w)
}

// load assembles both tables from one pass over the memberships.
//
// One ListMembers per storage yields both the members column and, inverted,
// each user's storages column — a household has a handful of each, so this is
// a few queries rather than a join worth writing.
func (h *Handler) load(r *http.Request) (*pageData, error) {
	ctx := r.Context()

	users, err := h.store.ListUsers(ctx)
	if err != nil {
		return nil, err
	}
	storages, err := h.store.ListStorages(ctx)
	if err != nil {
		return nil, err
	}

	self, _ := h.currentUser(r)
	storagesOf := map[uuid.UUID][]string{}
	data := &pageData{}

	for _, s := range storages {
		members, err := h.store.ListMembers(ctx, s.ID)
		if err != nil {
			return nil, err
		}
		inStorage := make(map[uuid.UUID]bool, len(members))
		for _, m := range members {
			inStorage[m.UserID] = true
			storagesOf[m.UserID] = append(storagesOf[m.UserID], s.Name)
		}
		row := storageRow{ID: s.ID, Name: s.Name, Members: members}
		for _, u := range users {
			if !inStorage[u.ID] {
				row.Candidates = append(row.Candidates, u)
			}
		}
		data.Storages = append(data.Storages, row)
	}

	for _, u := range users {
		names := storagesOf[u.ID]
		sort.Strings(names)
		data.Users = append(data.Users, userRow{
			ID:          u.ID,
			Username:    u.Username,
			DisplayName: u.DisplayName,
			IsAdmin:     u.IsAdmin,
			IsSelf:      u.ID == self,
			Storages:    names,
		})
	}

	if h.vision == nil {
		// No vision provider at all is the most unavailable a model can be.
		// The banner says so, matching GetSettings' model_unavailable for the
		// same configuration (internal/httpapi/admin.go), rather than an
		// admin page that looks healthy while every vision feature is off.
		data.ModelUnavailable = true
	} else {
		model, err := h.vision.EffectiveModel(ctx)
		if err != nil {
			return nil, err
		}
		data.GeminiModel = model
		data.ModelUnavailable = h.vision.Status(ctx) != vision.StatusOK
		// A provider that cannot be reached is ModelUnavailable above, not a
		// reason to fail the whole page (see internal/httpapi/admin.go's
		// GetSettings, which degrades the same way for the same reason);
		// the template's picker falls back to a plain <input> when
		// AvailableModels is empty. Logged for the same observability
		// reason GetSettings logs it.
		if models, err := h.vision.Models(ctx); err == nil {
			data.AvailableModels = models
		} else {
			slog.WarnContext(ctx, "admin page: could not list vision models", slog.Any("err", err))
		}
	}
	if h.imageModel != nil {
		if model, ok := h.imageModel.Available(ctx); !ok {
			data.ImageModel, data.ImageModelUnavailable = model, true
		}
	}

	data.CatalogQuery = r.URL.Query().Get("q")
	catalog, err := h.store.SearchCatalog(ctx, data.CatalogQuery)
	if err != nil {
		return nil, err
	}
	for _, c := range catalog {
		row := catalogRow{ID: c.ID, DisplayName: c.DisplayName, ItemType: string(c.ItemType)}
		if c.CategoryPath != nil {
			row.CategoryPath = *c.CategoryPath
		}
		if c.DefaultShelfLifeDays != nil {
			row.ShelfLifeDays = strconv.Itoa(*c.DefaultShelfLifeDays)
		}
		data.Catalog = append(data.Catalog, row)
	}

	return data, nil
}

// newNonce returns a fresh CSP nonce. 128 bits, per request.
//
// URL-safe and unpadded, so html/template has nothing to escape: standard
// base64's "+" is rendered as "&#43;" in the attribute. A browser decodes that
// back and the nonce still matches, but the header and the markup should not
// need an HTML parser to agree.
func newNonce() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("admin: nonce: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
