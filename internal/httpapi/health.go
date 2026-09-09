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
type HealthResponse struct {
	Status string `json:"status"`
	Vision string `json:"vision"`
}

// Health status values.
const (
	healthOK          = "ok"
	healthUnavailable = "unavailable"
)

// HealthHandler serves GET /healthz.
//
// It answers 200 once the database can be reached and 503 until then, which is
// what the compose healthcheck and Traefik consume. The vision field carries
// the model-resilience status so a deployment running on a deprecated model id
// is visible without opening the admin UI.
//
// Both collaborators may be nil: a nil DBPinger reports the database as
// reachable, and a nil VisionReporter reports vision as unavailable, since a
// deployment with no vision client configured cannot do vision work.
func HealthHandler(db DBPinger, v VisionReporter) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), healthTimeout)
		defer cancel()

		body := HealthResponse{
			Status: healthOK,
			Vision: vision.StatusModelUnavailable,
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
