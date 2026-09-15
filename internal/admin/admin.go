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

// Handler serves the admin pages.
type Handler struct {
	store  Store
	vision VisionChecker
	page   *template.Template
	// currentUser names the caller, so the page can withhold the delete action
	// from their own row. It is a callback rather than an import because the
	// session lives in the httpapi package, which mounts this one.
	currentUser func(*http.Request) (uuid.UUID, bool)
	// fail reports an unexpected error. It is the httpapi error serializer, so
	// this package cannot produce an error body of its own — the single
	// serializer rule holds here too.
	fail func(http.ResponseWriter, *http.Request, error)
}

// New parses the admin template. It fails at startup, not on the first
// request, if the template is missing or malformed.
func New(
	s Store,
	visionChecker VisionChecker,
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
	return &Handler{store: s, vision: visionChecker, page: page, currentUser: currentUser, fail: fail}, nil
}

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
	Users    []userRow
	Storages []storageRow

	// AI model resilience (docs/specs/01-architecture-and-deployment.md).
	GeminiModel      string
	AvailableModels  []string
	ModelUnavailable bool

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

	nonce, err := newNonce()
	if err != nil {
		h.fail(w, r, err)
		return
	}
	data.Nonce = nonce

	// Rendered into a buffer first, so a template error is still a clean 500
	// rather than half a page followed by nothing.
	var buf bytes.Buffer
	if err := h.page.Execute(&buf, data); err != nil {
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

	if h.vision != nil {
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
			slog.Warn("admin page: could not list vision models", slog.Any("err", err))
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
