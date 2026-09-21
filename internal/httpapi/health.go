package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/CDRO/Inventory/internal/vision"
)

// healthTimeout bounds the readiness checks so a hung database cannot hold the
// probe open until Traefik's own timeout fires.
const healthTimeout = 2 * time.Second

// HealthResponse is the body of GET /healthz.
//
// Status is "ok" only when the database is reachable. Vision is reported
// separately and never affects Status: an unavailable model degrades the
// vision features alone, and marking the whole deployment unhealthy for it
// would take a working inventory system out of service
// (docs/specs/01-architecture-and-deployment.md).
// Version is the build stamped into the binary
// (docs/specs/18-operations-and-observability.md). It is here rather than only
// in the admin UI so that "what is the NAS actually running" is answerable
// from a curl, without a session and without SSH.
type HealthResponse struct {
	Status  string `json:"status"`
	Vision  string `json:"vision"`
	Version string `json:"version"`
}

// Health status values.
const (
	healthOK          = "ok"
	healthUnavailable = "unavailable"
)

// DevVersion is what an unstamped build reports. A binary built without
// -ldflags "-X main.version=…" — every `go build` and `go run` outside the
// Dockerfile — is a development build, and saying so is more useful than an
// empty string a reader has to interpret.
const DevVersion = "dev"

// HealthHandler serves GET /healthz.
//
// It answers 200 once the database can be reached and 503 until then, which is
// what the compose healthcheck and Traefik consume. The vision field carries
// the model-resilience status so a deployment running on a deprecated model id
// is visible without opening the admin UI.
//
// Both collaborators may be nil: a nil DBPinger reports the database as
// reachable, and a nil VisionReporter reports vision as unavailable, since a
// deployment with no vision client configured cannot do vision work. An empty
// version is reported as DevVersion, so a router built without one still
// answers the question rather than answering it with "".
func HealthHandler(db DBPinger, v VisionReporter, version string) http.HandlerFunc {
	if version == "" {
		version = DevVersion
	}
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), healthTimeout)
		defer cancel()

		body := HealthResponse{
			Status:  healthOK,
			Vision:  vision.StatusModelUnavailable,
			Version: version,
		}
		if v != nil {
			body.Vision = v.Status(ctx)
		}

		code := http.StatusOK
		if db != nil {
			if err := db.Ping(ctx); err != nil {
				body.Status = healthUnavailable
				code = http.StatusServiceUnavailable
			}
		}

		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(code)
		// The response is two short fixed-shape strings; an encode failure here
		// means the client hung up, which is not actionable server-side.
		_ = json.NewEncoder(w).Encode(body)
	}
}
