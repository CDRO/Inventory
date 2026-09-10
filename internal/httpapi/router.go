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
//     chi's own 404 and 405 are routed through it too, so the system has one
//     error format rather than two.
//   - **Three authorization gates, and nowhere else.** RequireSession,
//     RequireAdmin and RequireStorageMember in middleware.go decide access. No
//     handler performs its own check. RequireAdmin re-reads is_admin from the
//     database on every request, and every refusal — unknown storage,
//     inaccessible storage, malformed id, the whole admin area — is the same
//     404, byte for byte.
//   - **One upload path.** ReadImageUpload in upload.go strips metadata from
//     every image entering the system and generates the filename itself.
//
// The auth, admin, pairing and device routes that mount behind these gates are
// spec 03's surface and land separately.
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

	if d.StaticFS != nil {
		r.Handle("/*", http.FileServer(http.FS(d.StaticFS)))
	}
	return r
}
