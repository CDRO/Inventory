package vision

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newListerFor points an APILister at a test server.
func newListerFor(t *testing.T, handler http.HandlerFunc) *APILister {
	t.Helper()

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	lister := NewAPILister("test-key")
	lister.Endpoint = srv.URL
	return lister
}

// TestListModelsFollowsPagination is the regression this whole file exists
// for: Gemini returns models a page at a time, and a broken page-token loop
// drops everything past page one. The failure is silent and total — a
// deployment whose configured model sits on page two reports
// model_unavailable forever while every stubbed test stays green.
func TestListModelsFollowsPagination(t *testing.T) {
	t.Parallel()

	var seenTokens []string
	lister := newListerFor(t, func(w http.ResponseWriter, r *http.Request) {
		seenTokens = append(seenTokens, r.URL.Query().Get("pageToken"))

		assert.Equal(t, "test-key", r.URL.Query().Get("key"))
		assert.Equal(t, "200", r.URL.Query().Get("pageSize"))

		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Query().Get("pageToken") {
		case "":
			fmt.Fprint(w, `{"models":[{"name":"models/gemini-2.0-flash"}],"nextPageToken":"page-2"}`)
		case "page-2":
			fmt.Fprint(w, `{"models":[{"name":"models/gemini-2.5-pro"}]}`)
		default:
			t.Errorf("unexpected page token %q", r.URL.Query().Get("pageToken"))
		}
	})

	got, err := lister.ListModels(context.Background())

	require.NoError(t, err)
	assert.Equal(t, []string{"models/gemini-2.0-flash", "models/gemini-2.5-pro"}, got)
	assert.Equal(t, []string{"", "page-2"}, seenTokens, "the second request must carry the token from the first")
}

// TestListModelsEscapesPageToken pins the query building. Real page tokens are
// base64 and routinely contain '+' and '='; concatenating them into the URL
// truncates the query and ends pagination early, which looks exactly like a
// provider that only has one page.
func TestListModelsEscapesPageToken(t *testing.T) {
	t.Parallel()

	const awkward = "a+b/c=d&e=f"

	var secondToken string
	calls := 0
	lister := newListerFor(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		if calls == 1 {
			fmt.Fprintf(w, `{"models":[{"name":"models/a"}],"nextPageToken":%q}`, awkward)
			return
		}
		secondToken = r.URL.Query().Get("pageToken")
		fmt.Fprint(w, `{"models":[{"name":"models/b"}]}`)
	})

	got, err := lister.ListModels(context.Background())

	require.NoError(t, err)
	assert.Equal(t, awkward, secondToken, "the token must survive the round trip intact")
	assert.Equal(t, []string{"models/a", "models/b"}, got)
}

// TestListModelsStopsAtPageBound proves the loop is bounded. A provider stuck
// returning the same token would otherwise spin forever inside a /healthz
// request.
func TestListModelsStopsAtPageBound(t *testing.T) {
	t.Parallel()

	calls := 0
	lister := newListerFor(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"models":[{"name":"models/loop"}],"nextPageToken":"always-more"}`)
	})

	got, err := lister.ListModels(context.Background())

	require.NoError(t, err)
	assert.Equal(t, 10, calls, "the page loop must stop at its bound")
	assert.Len(t, got, 10)
}

// TestListModelsReportsFailures covers the paths where the provider answers
// but the answer is unusable. Each must surface as an error so Status reports
// model_unavailable rather than silently concluding no models exist.
func TestListModelsReportsFailures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		handler http.HandlerFunc
		wantErr string
	}{
		{
			name: "unauthorized key",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusUnauthorized)
			},
			wantErr: "models.list returned",
		},
		{
			name: "server error",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
			},
			wantErr: "models.list returned",
		},
		{
			name: "malformed body",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"models":[`)
			},
			wantErr: "decode models.list",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			lister := newListerFor(t, tc.handler)

			_, err := lister.ListModels(context.Background())

			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

// TestListModelsHonoursContextCancellation keeps a hung provider from
// outliving the readiness probe that triggered it.
func TestListModelsHonoursContextCancellation(t *testing.T) {
	t.Parallel()

	lister := newListerFor(t, func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := lister.ListModels(ctx)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "call models.list")
}

// TestCheckerAgainstRealHTTP wires the checker to an actual server rather than
// a stub, so the normalization and the pagination join up end to end: a model
// that only appears on page two must still report ok.
func TestCheckerAgainstRealHTTP(t *testing.T) {
	t.Parallel()

	lister := newListerFor(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("pageToken") == "" {
			fmt.Fprint(w, `{"models":[{"name":"models/gemini-1.0-pro"}],"nextPageToken":"p2"}`)
			return
		}
		fmt.Fprint(w, `{"models":[{"name":"models/gemini-2.0-flash"}]}`)
	})

	c := NewChecker(nil, lister, "gemini-2.0-flash")

	assert.Equal(t, StatusOK, c.Status(context.Background()))
}
