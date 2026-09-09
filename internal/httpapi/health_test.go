package httpapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CDRO/Inventory/internal/httpapi"
	"github.com/CDRO/Inventory/internal/vision"
)

type stubPinger struct{ err error }

func (s stubPinger) Ping(context.Context) error { return s.err }

type stubVision struct{ status string }

func (s stubVision) Status(context.Context) string { return s.status }

// TestHealthHandler pins the two independent axes of GET /healthz: the status
// code follows database reachability, and the vision field follows model
// availability without ever influencing the code. Collapsing them — answering
// 503 because a model id is stale — would take a working inventory system out
// of service, so it is asserted explicitly.
func TestHealthHandler(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		db         httpapi.DBPinger
		vision     httpapi.VisionReporter
		wantCode   int
		wantStatus string
		wantVision string
	}{
		{
			name:       "database up and model available",
			db:         stubPinger{},
			vision:     stubVision{status: vision.StatusOK},
			wantCode:   http.StatusOK,
			wantStatus: "ok",
			wantVision: vision.StatusOK,
		},
		{
			name:       "database up but model gone still serves 200",
			db:         stubPinger{},
			vision:     stubVision{status: vision.StatusModelUnavailable},
			wantCode:   http.StatusOK,
			wantStatus: "ok",
			wantVision: vision.StatusModelUnavailable,
		},
		{
			name:       "database unreachable is 503 even when vision is fine",
			db:         stubPinger{err: errors.New("dial tcp: connection refused")},
			vision:     stubVision{status: vision.StatusOK},
			wantCode:   http.StatusServiceUnavailable,
			wantStatus: "unavailable",
			wantVision: vision.StatusOK,
		},
		{
			name:       "no vision client configured reports model_unavailable",
			db:         stubPinger{},
			vision:     nil,
			wantCode:   http.StatusOK,
			wantStatus: "ok",
			wantVision: vision.StatusModelUnavailable,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/healthz", nil)

			httpapi.HealthHandler(tc.db, tc.vision).ServeHTTP(rec, req)

			require.Equal(t, tc.wantCode, rec.Code)
			assert.Equal(t, "application/json; charset=utf-8", rec.Header().Get("Content-Type"))

			var got httpapi.HealthResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
			assert.Equal(t, tc.wantStatus, got.Status)
			assert.Equal(t, tc.wantVision, got.Vision)
		})
	}
}

// TestRouterServesHealthz is the deployment smoke test: it exercises the route
// as the compose healthcheck and Traefik reach it, so a handler that works in
// isolation but was never wired to /healthz still fails the suite.
func TestRouterServesHealthz(t *testing.T) {
	t.Parallel()

	router := httpapi.NewRouter(httpapi.Deps{
		DB:     stubPinger{},
		Vision: stubVision{status: vision.StatusOK},
	})

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	require.Equal(t, http.StatusOK, rec.Code)

	var got httpapi.HealthResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, "ok", got.Status)
	assert.Equal(t, vision.StatusOK, got.Vision)
}

// TestHealthzRejectsNonGET guards the method set: chi answers 405 for a verb
// the route does not register, and a probe that silently accepted POST would
// hide a routing mistake.
func TestHealthzRejectsNonGET(t *testing.T) {
	t.Parallel()

	router := httpapi.NewRouter(httpapi.Deps{
		DB:     stubPinger{},
		Vision: stubVision{status: vision.StatusOK},
	})

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/healthz", nil))

	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}
