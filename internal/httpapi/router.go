// Package httpapi owns the HTTP surface: the chi router, the JSON handlers,
// and the middleware that decides authorization
// (docs/specs/04-backend-api-conventions.md).
//
// Three rules of the package are structural rather than conventional, because
// each of them fails silently when it is left to a handler to remember:
//
//   - **One error serializer.** errors.go is the only place an error envelope
//     is produced and the only place debug_reason can be attached. A handler
//     describes a Failure and hands it over; it has no way to write an
//     envelope itself, so it cannot leak an internal reason in production.
//     chi's own 404 and 405 are routed through it too, for any request chi
//     itself resolves as unmatched or wrong-method. The one exception is a GET
//     to an /api/... path that has no route registered yet: it still falls
//     through to the static file server's own plain-text 404, not this
//     serializer — see the comment beside the "/*" mount below for why, and
//     why that gap closes itself as each real route lands.
//   - **Three authorization gates, and nowhere else.** RequireSession,
//     RequireAdmin and RequireStorageMember in middleware.go decide access. No
//     handler performs its own check. RequireAdmin re-reads is_admin from the
//     database on every request, and every refusal — unknown storage,
//     inaccessible storage, malformed id, the whole admin area — is the same
//     404, byte for byte.
//   - **One upload path.** ReadImageUpload in upload.go strips metadata from
//     every image entering the system and generates the filename itself.
//
// The storage-scoped routes — the location tree and the batch operations of
// docs/specs/06-vision-shelf-ingestion.md — are registered on one sub-router
// carrying the gate chain, so a new route under /api/storages/{storage_id} is
// protected by the act of being added.
//
// The auth, admin, pairing and device routes that mount behind these gates are
// spec 03's surface and land separately. Until they do, nothing can create a
// session, so the storage-scoped API answers 401 to every caller — it is wired
// and guarded, not yet reachable.
package httpapi

import (
	"context"
	"io/fs"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// DBPinger reports whether the database is reachable. It is the narrow slice
// of *store.Store that the readiness endpoint needs, declared here so the
// handler can be tested without a database.
type DBPinger interface {
	Ping(ctx context.Context) error
}

// VisionReporter reports the vision subsystem's status, either
// vision.StatusOK or vision.StatusModelUnavailable.
type VisionReporter interface {
	Status(ctx context.Context) string
}

// APIStore is everything the storage-scoped API needs: the authorization
// lookups the gates perform, and the reads and writes the handlers behind them
// perform. *store.Store satisfies it.
//
// It is one interface rather than a field per resource so that the gates and
// the handlers they protect cannot be wired independently. A router that has
// the location handlers but not the session lookups would be a router serving
// a storage's tree to anyone who asked.
type APIStore interface {
	AuthStore
	LocationStore
	BatchStore
	ShoppingListStore
	ExpiryStore
}

// Deps are the collaborators the router needs. StaticFS may be nil, in which
// case no static assets are served — useful in tests.
type Deps struct {
	DB       DBPinger
	Vision   VisionReporter
	StaticFS fs.FS
	// Errors serializes every error the API returns. When nil a production
	// writer is used, so a router built without one still cannot emit
	// debug_reason.
	Errors *ErrorWriter
	// Store backs the storage-scoped API. When nil those routes are not
	// registered at all — the failure mode of forgetting to pass one is
	// "the API is absent", never "the API is unguarded".
	Store APIStore
	// Matcher resolves free text to a product
	// (docs/specs/07-shopping-list-reconciliation.md). Required for the
	// shopping-list routes; when nil they are not registered.
	Matcher Matcher
	// Images produces image suggestions, and ImageCache serves the bytes they
	// point at. Both nil means the image routes are absent, which is the
	// honest state for a deployment with no provider configured.
	Images     ImageSuggester
	ImageCache ImageCache
}

// NewRouter builds the application's HTTP handler.
//
// Route groups follow docs/specs/04-backend-api-conventions.md: /healthz is
// unauthenticated because it is consumed by the compose healthcheck and by
// Traefik, and "/" serves the static frontend.
func NewRouter(d Deps) http.Handler {
	errs := d.Errors
	if errs == nil {
		// Defaulting to a production writer rather than panicking keeps a
		// misconfigured router safe: the failure mode of forgetting to pass
		// one must be "no reasons disclosed", never "all of them".
		errs = NewErrorWriter(false, nil)
	}

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)

	// chi's defaults answer with plain text ("404 page not found"), which is
	// neither the API's error shape nor something a client can switch on. Both
	// go through the one serializer instead, so there is exactly one error
	// format in the system.
	r.NotFound(func(w http.ResponseWriter, req *http.Request) {
		errs.WriteError(w, req, NotFound("no route matches "+req.URL.Path))
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, req *http.Request) {
		errs.WriteError(w, req, &Failure{
			Status:  http.StatusMethodNotAllowed,
			Code:    "method_not_allowed",
			Message: "That method is not allowed here.",
			Reason:  req.Method + " on " + req.URL.Path,
		})
	})

	r.Get("/healthz", HealthHandler(d.DB, d.Vision))

	if d.Store != nil {
		// One sub-router carries the gate chain, and every storage-scoped route
		// is registered on it (docs/specs/04-backend-api-conventions.md). That
		// is what makes adding a route the same act as protecting it: there is
		// no way to hang a new /api/storages/... handler somewhere that skips
		// RequireStorageMember, because the pattern itself lives here.
		mw := NewMiddleware(d.Store, errs)
		locations := NewLocationHandler(d.Store, errs)
		batches := NewBatchHandler(d.Store, errs)

		r.Route("/api/storages/{storage_id}", func(sr chi.Router) {
			sr.Use(mw.RequireSession)
			sr.Use(mw.RequireStorageMember)

			sr.Get("/locations", locations.List)
			sr.Post("/locations", locations.Create)
			sr.Patch("/locations/{id}", locations.Update)
			sr.Delete("/locations/{id}", locations.Delete)

			sr.Patch("/inventory-batches/{id}", batches.Update)
			sr.Post("/inventory-batches/{id}/split", batches.Split)

			expiry := NewExpiryHandler(d.Store, errs)
			sr.Patch("/inventory-batches/{id}/expiry", expiry.PatchBatchExpiry)
			sr.Patch("/categories/{id}/shelf-life", expiry.PatchCategoryShelfLife)

			if d.Matcher != nil {
				lists := NewShoppingListHandler(d.Store, d.Matcher, errs)
				sr.Post("/shopping-lists", lists.Create)
				sr.Get("/shopping-lists/{id}", lists.Get)
				sr.Post("/shopping-lists/{id}/items/{item_id}/rematch", lists.Rematch)
				sr.Post("/shopping-lists/{id}/items/{item_id}/resolve", lists.Resolve)
			}

			if d.Images != nil && d.ImageCache != nil {
				images := NewImageHandler(d.Images, d.ImageCache, errs)
				sr.Get("/image-suggestions", images.Suggest)
				sr.Get("/images/{hash}", images.Serve)
			}
		})
	}

	if d.StaticFS != nil {
		// GET only, deliberately — not r.Handle, which would register the
		// file server for every method.
		//
		// chi's r.NotFound only fires when no registered pattern matches a
		// request at all, and "/*" matches every path. Registered for every
		// method, it would swallow POST/PUT/DELETE requests to routes that do
		// not exist yet — most immediately POST /api/auth/login, before spec
		// 03's HTTP surface (#27) registers it — handing them to
		// http.FileServer, which answers its own plain-text 404 before chi's
		// r.NotFound, and this package's single JSON serializer, ever see the
		// request. That silently contradicts the "one error format" rule
		// stated above; verified live (`curl -X POST .../api/auth/login`
		// returned "404 page not found" in text/plain) before this fix and
		// the JSON envelope after it.
		//
		// GET (and HEAD, below) closes it for every method that actually
		// mutates anything. A GET to a not-yet-registered /api/... path still
		// falls through to the file server today — there is no static file
		// there either, so it is still a 404, just not yet through the JSON
		// serializer — and that residual gap closes itself as each real
		// GET /api/... route is registered, since a registered route always
		// takes precedence over the "/*" catch-all.
		//
		// One side effect worth knowing about: because "/*" now answers only
		// GET and HEAD, chi's MethodNotAllowed (not NotFound) fires for every
		// other verb against any unregistered path — POST /nonexistent gets a
		// 405, not a 404, even though nothing by that name exists at all. 405
		// technically means "this resource exists but rejects this verb",
		// which isn't quite true here; it is an inherent limitation of
		// registering a catch-all on a single pattern rather than a defect,
		// and no client in this codebase branches on the distinction today.
		fileServer := http.FileServer(http.FS(d.StaticFS)).ServeHTTP
		r.Get("/*", fileServer)
		// chi does not imply HEAD from a GET registration the way stdlib's
		// http.ServeMux does — caught live: `curl -I` against a real asset
		// answered 405 until this line was added. http.FileServer's handler
		// already branches on r.Method internally (writing headers only for
		// HEAD), so the same handler value is correct for both routes; only
		// the registration was missing.
		r.Head("/*", fileServer)
	}
	return r
}
