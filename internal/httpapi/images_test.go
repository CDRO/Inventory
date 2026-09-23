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

// fakeImageCache serves canned bytes. Fetch hands out the real hash of any
// provider URL; SourceURL knows only the hashes a test put in sources.
type fakeImageCache struct {
	data        []byte
	contentType string
	err         error
	fetchErr    error
	sources     map[string]string

	touched []string
	fetched []string
}

func (f *fakeImageCache) Fetch(_ context.Context, sourceURL string) (string, error) {
	f.fetched = append(f.fetched, sourceURL)
	if f.fetchErr != nil {
		return "", f.fetchErr
	}
	return imagesearch.HashURL(sourceURL), nil
}

func (f *fakeImageCache) SourceURL(_ context.Context, hash string) (string, error) {
	if source, ok := f.sources[hash]; ok {
		return source, nil
	}
	return "", store.ErrNotFound
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

// --- the catalog-images route (docs/specs/24-barcode-hot-cache.md) ---------
//
// /api/catalog-images/{hash} is Serve's non-storage-scoped twin: the picture
// behind a hot-cache card, on the same origin, gated by session alone
// because the list it illustrates carries no storage reference either.

// TestServeCatalogImageCarriesTheHardeningHeaders mirrors
// TestServedImagesCarryTheHardeningHeaders for the non-storage-scoped route:
// the two share their serving logic, and the headers must too.
func TestServeCatalogImageCarriesTheHardeningHeaders(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	f.imageData.data = []byte(`<svg xmlns="http://www.w3.org/2000/svg"><rect/></svg>`)
	f.imageData.contentType = "image/svg+xml"

	hash := strings.Repeat("b", 64)
	rec := f.do(http.MethodGet, "/api/catalog-images/"+hash, "")

	require.Equal(t, http.StatusOK, rec.Code)
	csp := rec.Header().Get("Content-Security-Policy")
	assert.Contains(t, csp, "default-src 'none'")
	assert.Contains(t, csp, "sandbox")
	assert.Equal(t, "nosniff", rec.Header().Get("X-Content-Type-Options"))
	assert.Equal(t, "image/svg+xml", rec.Header().Get("Content-Type"))
}

// TestServeCatalogImageMalformedHashIs404 mirrors
// TestAMalformedHashIs404NotATraversal for the new route.
func TestServeCatalogImageMalformedHashIs404(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	rec := f.do(http.MethodGet, "/api/catalog-images/../../etc/passwd", "")

	require.Equal(t, http.StatusNotFound, rec.Code)
	assert.Empty(t, f.imageData.touched)
}

// TestServeCatalogImageNeedsNoStorageMembership is the point of the route:
// unlike /api/storages/{id}/images/{hash}, a caller with no storage
// membership at all still gets the picture — the same "not storage-scoped"
// property GET /api/barcodes/hot has, checked here for its image mechanism.
func TestServeCatalogImageNeedsNoStorageMembership(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	f.auth.removeMember(f.storageID, f.user.ID)
	hash := strings.Repeat("f", 64)

	rec := f.do(http.MethodGet, "/api/catalog-images/"+hash, "")

	require.Equal(t, http.StatusOK, rec.Code)
}

// TestServeCatalogImageRequiresASession — session-scoped is still a gate, not
// an open route.
func TestServeCatalogImageRequiresASession(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	rec := f.anonymous(http.MethodGet, "/api/catalog-images/"+strings.Repeat("a", 64), "")

	require.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Equal(t, "unauthorized", errorCode(t, rec))
}

// TestHotBarcodesImageURLPointsAtTheCatalogImagesRoute — the hot-cache
// handler builds its picture address with the non-storage-scoped twin, never
// the storage-scoped one a caller might not be allowed to use for every
// storage they belong to.
func TestHotBarcodesImageURLPointsAtTheCatalogImagesRoute(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	source := "https://provider.example/tomatoes.jpg"
	f.barcodes.hot = []store.HotBarcode{{
		Barcode: "4006381333931", DisplayName: "Canned Tomatoes",
		ItemType: store.ItemLongShelfLife, ImageURL: &source,
	}}

	rec := f.do(http.MethodGet, "/api/barcodes/hot", "")
	require.Equal(t, http.StatusOK, rec.Code)

	var body struct {
		Items []struct {
			ImageURL *string `json:"image_url"`
		} `json:"items"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body.Items, 1)
	require.NotNil(t, body.Items[0].ImageURL)
	assert.True(t, strings.HasPrefix(*body.Items[0].ImageURL, "/api/catalog-images/"),
		"got %q", *body.Items[0].ImageURL)
	assert.NotContains(t, *body.Items[0].ImageURL, f.storageID.String(),
		"the address must not be scoped to whichever storage happened to ask")
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
