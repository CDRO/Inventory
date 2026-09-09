// Package httpapi owns the HTTP surface: the chi router, the JSON handlers,
// and the middleware that decides authorization
// (docs/specs/04-backend-api-conventions.md).
//
// At this stage it carries the deployment skeleton only — the readiness
// endpoint and the static-asset mount. Sessions, storage scoping, and the
// single error-envelope serializer arrive with specs 03 and 04.
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
}

// NewRouter builds the application's HTTP handler.
//
// Route groups follow docs/specs/04-backend-api-conventions.md: /healthz is
// unauthenticated because it is consumed by the compose healthcheck and by
// Traefik, and "/" serves the static frontend.
func NewRouter(d Deps) http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)

	r.Get("/healthz", HealthHandler(d.DB, d.Vision))

	if d.StaticFS != nil {
		r.Handle("/*", http.FileServer(http.FS(d.StaticFS)))
	}
	return r
}
