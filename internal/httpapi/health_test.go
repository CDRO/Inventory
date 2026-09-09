package httpapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"

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

// TestRouterServesStaticAssets covers the mount the other router tests skip by
// passing StaticFS: nil. Without it, a frontend that is embedded correctly but
// never wired to "/" ships with every test green and every page 404.
func TestRouterServesStaticAssets(t *testing.T) {
	t.Parallel()

	assets := fstest.MapFS{
		"index.html":   &fstest.MapFile{Data: []byte("<h1>index</h1>")},
		"css/base.css": &fstest.MapFile{Data: []byte("body{}")},
	}
	router := httpapi.NewRouter(httpapi.Deps{
		DB:       stubPinger{},
		Vision:   stubVision{status: vision.StatusOK},
		StaticFS: assets,
	})

	tests := []struct {
		path string
		want string
	}{
		{path: "/", want: "<h1>index</h1>"},
		{path: "/css/base.css", want: "body{}"},
	}

	for _, tc := range tests {
		t.Run(tc.path, func(t *testing.T) {
			t.Parallel()

			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))

			require.Equal(t, http.StatusOK, rec.Code)
			assert.Contains(t, rec.Body.String(), tc.want)
		})
	}

	// http.FileServer canonicalises /index.html to /. Pinned because it is
	// stdlib behaviour the frontend's own links depend on, not a bug.
	t.Run("/index.html redirects to /", func(t *testing.T) {
		t.Parallel()

		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/index.html", nil))

		require.Equal(t, http.StatusMovedPermanently, rec.Code)
		assert.Equal(t, "./", rec.Header().Get("Location"))
	})
}

// TestStaticMountDoesNotShadowHealthz pins the route precedence: the catch-all
// file server is registered on "/*", and if it took priority the readiness
// probe would start answering 404 to Traefik and the compose healthcheck.
func TestStaticMountDoesNotShadowHealthz(t *testing.T) {
	t.Parallel()

	router := httpapi.NewRouter(httpapi.Deps{
		DB:     stubPinger{},
		Vision: stubVision{status: vision.StatusOK},
		StaticFS: fstest.MapFS{
			"healthz": &fstest.MapFile{Data: []byte("this file must never be served")},
		},
	})

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	require.Equal(t, http.StatusOK, rec.Code)
	var got httpapi.HealthResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, "ok", got.Status)
}
