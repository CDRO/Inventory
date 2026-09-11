package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CDRO/Inventory/internal/imagesearch"
	"github.com/CDRO/Inventory/internal/store"
)

// fakeSuggester records the URL builder it was handed, so a test can check
// what shape of URL the handler would publish.
type fakeSuggester struct {
	suggestions []imagesearch.Suggestion
	lastQuery   string
	builtURLs   []string
}

func (f *fakeSuggester) Suggest(_ context.Context, query string, urlFor func(string) string) []imagesearch.Suggestion {
	f.lastQuery = query
	if f.suggestions != nil {
		return f.suggestions
	}
	// Exercise the handler's own URL builder with a plausible hash.
	built := urlFor(strings.Repeat("a", 64))
	f.builtURLs = append(f.builtURLs, built)
	return []imagesearch.Suggestion{
		{Type: imagesearch.TypeIcon, URL: built, Source: "iconify"},
	}
}

// fakeImageCache serves canned bytes.
type fakeImageCache struct {
	data        []byte
	contentType string
	err         error

	touched []string
}

func (f *fakeImageCache) Open(_ context.Context, _ string) ([]byte, string, error) {
	if f.err != nil {
		return nil, "", f.err
	}
	if f.data == nil {
		return []byte("bytes"), "image/png", nil
	}
	return f.data, f.contentType, nil
}

func (f *fakeImageCache) Touch(hash string) { f.touched = append(f.touched, hash) }

// TestSuggestionURLsAreAlwaysOnOurOwnOrigin is the invariant at the HTTP
// boundary: whatever the providers returned, what the browser receives points
// back here.
func TestSuggestionURLsAreAlwaysOnOurOwnOrigin(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)

	rec := f.do(http.MethodGet, f.base()+"/image-suggestions?query=tomatoes", "")
	require.Equal(t, http.StatusOK, rec.Code)

	var body struct {
		Suggestions []struct {
			URL string `json:"url"`
		} `json:"suggestions"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.NotEmpty(t, body.Suggestions)

	for _, s := range body.Suggestions {
		assert.True(t, strings.HasPrefix(s.URL, "/api/storages/"+f.storageID.String()+"/images/"),
			"got %q — a suggestion URL must be a path on this origin, behind this storage's gate", s.URL)
		assert.NotContains(t, s.URL, "://", "never an absolute URL to somebody else's host")
	}
}

func TestSuggestQueryIsRequired(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)

	rec := f.do(http.MethodGet, f.base()+"/image-suggestions", "")

	require.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	assert.NotEmpty(t, errorFields(t, rec)["query"])
}

// TestServedImagesCarryTheHardeningHeaders — an SVG served from our own origin
// is same-origin script if a browser can be talked into running it. The bytes
// were sanitized before storage; these headers are the second layer.
func TestServedImagesCarryTheHardeningHeaders(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	f.imageData.data = []byte(`<svg xmlns="http://www.w3.org/2000/svg"><rect/></svg>`)
	f.imageData.contentType = "image/svg+xml"

	hash := strings.Repeat("b", 64)
	rec := f.do(http.MethodGet, f.base()+"/images/"+hash, "")

	require.Equal(t, http.StatusOK, rec.Code)
	csp := rec.Header().Get("Content-Security-Policy")
	assert.Contains(t, csp, "default-src 'none'")
	// `sandbox` is the directive the sanitizer's own comment leans on as the
	// layer that stops a directly-opened SVG executing anything a regex pass
	// missed — SMIL attribute hijacking, for one. Asserting only on
	// default-src would let it be dropped silently.
	assert.Contains(t, csp, "sandbox")
	assert.Equal(t, "nosniff", rec.Header().Get("X-Content-Type-Options"))
	assert.Equal(t, "image/svg+xml", rec.Header().Get("Content-Type"))
	assert.Contains(t, rec.Header().Get("Cache-Control"), "private",
		"the route is behind a membership check; a shared proxy must not hand it to anyone else")
}

// TestServingRefreshesRecencyAfterTheResponse — bookkeeping must not add
// latency to an image request, and the throttle lives in the store.
func TestServingRefreshesRecencyAfterTheResponse(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	hash := strings.Repeat("c", 64)

	rec := f.do(http.MethodGet, f.base()+"/images/"+hash, "")

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, []string{hash}, f.imageData.touched)
}

// TestAMalformedHashIs404NotATraversal — the parameter is used to build a
// filename, so it is validated against the exact shape of a SHA-256 digest
// rather than sanitized.
func TestAMalformedHashIs404NotATraversal(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)

	for _, hash := range []string{
		"..",
		"../../etc/passwd",
		"short",
		strings.Repeat("a", 63),
		strings.Repeat("a", 65),
		strings.Repeat("A", 64), // uppercase is not the hex we emit
		strings.Repeat("z", 64), // not hex at all
	} {
		t.Run(hash, func(t *testing.T) {
			rec := f.do(http.MethodGet, f.base()+"/images/"+hash, "")

			require.Equal(t, http.StatusNotFound, rec.Code)
			assert.Equal(t, "not_found", errorCode(t, rec))
			assert.Empty(t, f.imageData.touched, "a rejected hash must not reach the cache at all")
		})
	}
}

func TestAMissingImageIs404(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	f.imageData.err = store.ErrNotFound

	rec := f.do(http.MethodGet, f.base()+"/images/"+strings.Repeat("d", 64), "")

	require.Equal(t, http.StatusNotFound, rec.Code)
}

// TestImageRoutesAreBehindTheGateChain — the suggestion endpoint spends money
// and the serving endpoint reads shared cache bytes; neither may be reachable
// without a session and a membership.
func TestImageRoutesAreBehindTheGateChain(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	hash := strings.Repeat("e", 64)

	for _, path := range []string{
		f.base() + "/image-suggestions?query=milk",
		f.base() + "/images/" + hash,
	} {
		t.Run(path, func(t *testing.T) {
			anon := f.anonymous(http.MethodGet, path, "")
			require.Equal(t, http.StatusUnauthorized, anon.Code)
			assert.Equal(t, "unauthorized", errorCode(t, anon))
		})
	}

	// And a valid session that is not a member of this storage gets the same
	// 404 as an unknown storage.
	f.auth.removeMember(f.storageID, f.user.ID)
	rec := f.do(http.MethodGet, f.base()+"/image-suggestions?query=milk", "")
	require.Equal(t, http.StatusNotFound, rec.Code)
	assert.Zero(t, f.images.lastQuery, "a refused caller must not cause a provider call")
}

// TestShoppingListRoutesAreBehindTheGateChain covers the other new group the
// same way.
func TestShoppingListRoutesAreBehindTheGateChain(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	listID, itemID := uuid.New().String(), uuid.New().String()

	routes := []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodPost, f.base() + "/shopping-lists", `{"raw_text":"milk"}`},
		{http.MethodGet, f.base() + "/shopping-lists/" + listID, ""},
		{http.MethodPost, f.base() + "/shopping-lists/" + listID + "/items/" + itemID + "/rematch", `{"raw_text":"milk"}`},
		{http.MethodPost, f.base() + "/shopping-lists/" + listID + "/items/" + itemID + "/resolve", `{}`},
	}

	for _, route := range routes {
		t.Run(route.method+" "+route.path, func(t *testing.T) {
			rec := f.anonymous(route.method, route.path, route.body)

			require.Equal(t, http.StatusUnauthorized, rec.Code)
			assert.Equal(t, "unauthorized", errorCode(t, rec))
		})
	}
}
