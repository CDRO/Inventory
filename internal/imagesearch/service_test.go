package imagesearch_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CDRO/Inventory/internal/imagesearch"
	"github.com/CDRO/Inventory/internal/store"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// memStore is an in-memory CacheStore, so the cache's own logic is exercised
// without a database.
type memStore struct {
	mu   sync.Mutex
	rows map[string]store.CachedImage

	touches int
}

func newMemStore() *memStore {
	return &memStore{rows: map[string]store.CachedImage{}}
}

func (m *memStore) CachedImageByHash(_ context.Context, hash string) (*store.CachedImage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	row, ok := m.rows[hash]
	if !ok {
		return nil, store.ErrNotFound
	}
	return &row, nil
}

func (m *memStore) PutCachedImage(_ context.Context, in store.CachedImage) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if in.LastAccessedAt.IsZero() {
		in.LastAccessedAt = time.Now()
	}
	m.rows[in.Hash] = in
	return nil
}

func (m *memStore) TouchCachedImage(_ context.Context, hash string, _ time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.touches++
	return nil
}

func (m *memStore) CachedImageBytes(_ context.Context) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var total int64
	for _, row := range m.rows {
		if row.Status == store.CachedOK {
			total += row.ByteSize
		}
	}
	return total, nil
}

func (m *memStore) LeastRecentlyUsedImages(_ context.Context, limit int) ([]store.CachedImage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var out []store.CachedImage
	for _, row := range m.rows {
		if row.Status == store.CachedOK {
			out = append(out, row)
		}
	}
	// Oldest access first.
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j].LastAccessedAt.Before(out[i].LastAccessedAt) {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (m *memStore) DeleteCachedImage(_ context.Context, hash string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.rows, hash)
	return nil
}

func (m *memStore) CachedImageHashes(_ context.Context) (map[string]struct{}, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]struct{}{}
	for hash := range m.rows {
		out[hash] = struct{}{}
	}
	return out, nil
}

// pngBytes builds a small valid PNG.
func pngBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, w, h))))
	return buf.Bytes()
}

// stubProvider returns fixed candidates.
type stubProvider struct {
	candidates []imagesearch.Candidate
	err        error
	calls      int
}

func (s *stubProvider) Candidates(_ context.Context, _ string, _ int) ([]imagesearch.Candidate, error) {
	s.calls++
	return s.candidates, s.err
}

// TestSuggestionsOnlyEverCarryOurOwnURLs is the invariant the whole package
// exists for. A provider URL in a response would leak every viewer's IP and
// user-agent to that host, break on a LAN-only NAS, and let the remote server
// swap the picture afterwards.
func TestSuggestionsOnlyEverCarryOurOwnURLs(t *testing.T) {
	t.Parallel()

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(pngBytes(t, 40, 40))
	}))
	defer origin.Close()

	dir := t.TempDir()
	cache := imagesearch.NewCache(dir, newMemStore(), origin.Client(), discardLogger())

	icons := &stubProvider{candidates: []imagesearch.Candidate{
		{SourceURL: origin.URL + "/icon.svg", Type: imagesearch.TypeIcon, Source: "iconify"},
	}}
	photos := &stubProvider{candidates: []imagesearch.Candidate{
		{SourceURL: origin.URL + "/a.jpg", Type: imagesearch.TypePhoto, Source: "serpapi"},
		{SourceURL: origin.URL + "/b.jpg", Type: imagesearch.TypePhoto, Source: "serpapi"},
	}}

	svc := imagesearch.NewService(icons, photos, cache, discardLogger())
	got := svc.Suggest(context.Background(), "tomatoes",
		func(hash string) string { return "/api/storages/s1/images/" + hash })

	require.Len(t, got, imagesearch.WantTotal)
	for _, s := range got {
		assert.True(t, strings.HasPrefix(s.URL, "/api/storages/"),
			"every suggestion URL must be on our own origin, got %q", s.URL)
		assert.NotContains(t, s.URL, origin.URL, "the provider's URL must not survive into the response")
		assert.NotContains(t, s.URL, "http://")
		assert.NotContains(t, s.URL, "https://")
	}
}

// TestSerpAPIKeyNeverReachesAResponse — a key in a URL handed to a browser is
// a key in history, in proxy logs, and in the referrer of the next click.
func TestSerpAPIKeyNeverReachesAResponse(t *testing.T) {
	t.Parallel()

	const secret = "super-secret-serpapi-key"

	// The image origin doubles as the "provider result" host, so a leaked key
	// would have to show up in a suggestion URL to be visible at all.
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(pngBytes(t, 20, 20))
	}))
	defer origin.Close()

	var sawKey bool
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawKey = r.URL.Query().Get("api_key") == secret
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"images_results":[{"original":%q}]}`, origin.URL+"/p.jpg")
	}))
	defer provider.Close()

	serp := imagesearch.NewSerpAPIWithEndpoint(secret, provider.Client(), provider.URL)
	cache := imagesearch.NewCache(t.TempDir(), newMemStore(), origin.Client(), discardLogger())

	svc := imagesearch.NewService(nil, serp, cache, discardLogger())
	got := svc.Suggest(context.Background(), "milk", func(h string) string { return "/img/" + h })

	assert.True(t, sawKey, "the key must reach the provider — that is its only legitimate destination")
	require.NotEmpty(t, got)
	for _, s := range got {
		assert.NotContains(t, s.URL, secret, "and must appear nowhere in what the browser receives")
		assert.NotContains(t, s.Source, secret)
	}
}

// TestAProviderOutageDegradesRatherThanFails — a New Item flow that failed
// outright because Google was busy would be a worse system than one showing
// two pictures instead of three.
func TestAProviderOutageDegradesRatherThanFails(t *testing.T) {
	t.Parallel()

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(pngBytes(t, 20, 20))
	}))
	defer origin.Close()

	cache := imagesearch.NewCache(t.TempDir(), newMemStore(), origin.Client(), discardLogger())

	icons := &stubProvider{candidates: []imagesearch.Candidate{
		{SourceURL: origin.URL + "/i.svg", Type: imagesearch.TypeIcon, Source: "iconify"},
	}}
	photos := &stubProvider{err: errors.New("rate limited")}

	svc := imagesearch.NewService(icons, photos, cache, discardLogger())
	got := svc.Suggest(context.Background(), "milk", func(h string) string { return "/img/" + h })

	assert.Len(t, got, 1, "the icon still arrives; the photo slots are simply empty")
	assert.Equal(t, imagesearch.TypeIcon, got[0].Type)
}

// TestAnUnusableCandidateIsRememberedAndNeverRefetched is the point of negative
// caching: without it the same broken URL is downloaded on every run of the
// same query, forever.
func TestAnUnusableCandidateIsRememberedAndNeverRefetched(t *testing.T) {
	t.Parallel()

	var downloads int
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		downloads++
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write([]byte("not an image at all"))
	}))
	defer origin.Close()

	mem := newMemStore()
	cache := imagesearch.NewCache(t.TempDir(), mem, origin.Client(), discardLogger())

	_, err := cache.Fetch(context.Background(), origin.URL+"/broken.jpg")
	require.ErrorIs(t, err, imagesearch.ErrUnusableCached)
	assert.Equal(t, 1, downloads)

	// Second attempt must short-circuit before any network access.
	_, err = cache.Fetch(context.Background(), origin.URL+"/broken.jpg")
	require.ErrorIs(t, err, imagesearch.ErrUnusableCached)
	assert.Equal(t, 1, downloads, "a candidate known to be unusable must never be downloaded again")

	row, err := mem.CachedImageByHash(context.Background(), imagesearch.HashURL(origin.URL+"/broken.jpg"))
	require.NoError(t, err)
	assert.Equal(t, store.CachedUnusable, row.Status)
	assert.Zero(t, row.ByteSize, "an unusable row is metadata only, with no file")
}

// TestATransientFailureIsNotRememberedAsUnusable — recording a network outage
// permanently would make a perfectly good candidate unusable forever.
func TestATransientFailureIsNotRememberedAsUnusable(t *testing.T) {
	t.Parallel()

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer origin.Close()

	mem := newMemStore()
	cache := imagesearch.NewCache(t.TempDir(), mem, origin.Client(), discardLogger())

	_, err := cache.Fetch(context.Background(), origin.URL+"/flaky.jpg")
	require.Error(t, err)
	assert.NotErrorIs(t, err, imagesearch.ErrUnusableCached)

	_, err = mem.CachedImageByHash(context.Background(), imagesearch.HashURL(origin.URL+"/flaky.jpg"))
	assert.ErrorIs(t, err, store.ErrNotFound, "a 500 from the provider must leave no permanent verdict")
}

// TestDownloadIsCappedBySize bounds the work a hostile or broken URL can cause.
func TestDownloadIsCappedBySize(t *testing.T) {
	t.Parallel()

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		// Stream past the cap without ever declaring a Content-Length, since a
		// header is a claim and the body is what costs memory.
		chunk := make([]byte, 1<<20)
		for range 25 {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	}))
	defer origin.Close()

	cache := imagesearch.NewCache(t.TempDir(), newMemStore(), origin.Client(), discardLogger())

	_, err := cache.Fetch(context.Background(), origin.URL+"/huge.jpg")
	require.Error(t, err)
}

// TestEvictionFreesToTheLowWaterMark — evicting to exactly the cap would
// re-trigger a sweep on the very next write.
func TestEvictionFreesToTheLowWaterMark(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	mem := newMemStore()
	cache := imagesearch.NewCache(dir, mem, nil, discardLogger())

	// Fill past the cap, oldest first.
	const each = int64(100 << 20) // 100MB
	base := time.Now().Add(-24 * time.Hour)
	for i := range 12 {
		hash := fmt.Sprintf("hash%02d", i)
		require.NoError(t, os.WriteFile(filepath.Join(dir, hash), []byte("x"), 0o644))
		require.NoError(t, mem.PutCachedImage(context.Background(), store.CachedImage{
			Hash: hash, SourceURL: "u" + hash, ContentType: "image/jpeg",
			ByteSize: each, Status: store.CachedOK,
			LastAccessedAt: base.Add(time.Duration(i) * time.Minute),
		}))
	}

	require.NoError(t, cache.Evict(context.Background()))

	total, err := mem.CachedImageBytes(context.Background())
	require.NoError(t, err)
	// Via a variable: as a constant expression this truncates and will not
	// compile, since the product is not a whole number.
	capBytes := imagesearch.MaxCacheBytes
	lowWaterMark := int64(float64(capBytes) * imagesearch.EvictTargetRatio)
	assert.LessOrEqual(t, total, lowWaterMark)

	// The oldest entries went first, and their files went with them.
	_, err = mem.CachedImageByHash(context.Background(), "hash00")
	assert.ErrorIs(t, err, store.ErrNotFound, "the least recently used entry is evicted first")
	_, statErr := os.Stat(filepath.Join(dir, "hash00"))
	assert.True(t, os.IsNotExist(statErr), "the file is unlinked, not just the row")
}

// TestSweepOrphansRemovesFilesNoRowPointsAt is the other half of "row first,
// file second": eviction and a crashed write both leave files behind.
func TestSweepOrphansRemovesFilesNoRowPointsAt(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	mem := newMemStore()
	cache := imagesearch.NewCache(dir, mem, nil, discardLogger())

	require.NoError(t, os.WriteFile(filepath.Join(dir, "orphan"), []byte("x"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "kept"), []byte("x"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".tmp-inflight"), []byte("x"), 0o644))
	require.NoError(t, mem.PutCachedImage(context.Background(), store.CachedImage{
		Hash: "kept", SourceURL: "u", ContentType: "image/png", ByteSize: 1, Status: store.CachedOK,
	}))

	require.NoError(t, cache.SweepOrphans(context.Background()))

	_, err := os.Stat(filepath.Join(dir, "orphan"))
	assert.True(t, os.IsNotExist(err), "a file no row points at is an orphan")

	_, err = os.Stat(filepath.Join(dir, "kept"))
	assert.NoError(t, err, "a file with a row must survive")

	_, err = os.Stat(filepath.Join(dir, ".tmp-inflight"))
	assert.NoError(t, err, "a concurrent write's temp file is not an orphan yet")
}

// TestFetchRefetchesWhenTheRowSurvivedButTheFileDidNot — otherwise the cache
// serves a 404 for something it believes it has.
func TestFetchRefetchesWhenTheRowSurvivedButTheFileDidNot(t *testing.T) {
	t.Parallel()

	var downloads int
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		downloads++
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(pngBytes(t, 20, 20))
	}))
	defer origin.Close()

	dir := t.TempDir()
	mem := newMemStore()
	cache := imagesearch.NewCache(dir, mem, origin.Client(), discardLogger())

	url := origin.URL + "/p.png"
	hash, err := cache.Fetch(context.Background(), url)
	require.NoError(t, err)
	require.Equal(t, 1, downloads)

	require.NoError(t, os.Remove(filepath.Join(dir, hash)))

	again, err := cache.Fetch(context.Background(), url)
	require.NoError(t, err)
	assert.Equal(t, hash, again)
	assert.Equal(t, 2, downloads, "a row without its file must trigger a refetch")
}

// TestIconifyIdentifiersAreValidatedBeforeBecomingURLs — the identifier comes
// from a remote service, and this server fetches whatever it names from inside
// the deployment's network.
func TestIconifyIdentifiersAreValidatedBeforeBecomingURLs(t *testing.T) {
	t.Parallel()

	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"icons":[
			"../../etc/passwd",
			"http://169.254.169.254/latest/meta-data",
			"mdi:milk",
			"bad name:with spaces"
		]}`)
	}))
	defer provider.Close()

	icons := imagesearch.NewIconifyWithEndpoints(provider.Client(), provider.URL, "https://api.iconify.design")

	got, err := icons.Candidates(context.Background(), "milk", 5)
	require.NoError(t, err)

	require.Len(t, got, 1, "only the well-formed identifier may become a URL")
	assert.Equal(t, "https://api.iconify.design/mdi/milk.svg", got[0].SourceURL)
}

func TestFetchableURLRejectsNonHTTPSchemes(t *testing.T) {
	t.Parallel()

	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"images_results":[
			{"original":"file:///etc/passwd"},
			{"original":"gopher://internal:70/x"},
			{"original":"/relative/path.jpg"},
			{"original":"https://cdn.example/ok.jpg"}
		]}`)
	}))
	defer provider.Close()

	serp := imagesearch.NewSerpAPIWithEndpoint("key", provider.Client(), provider.URL)

	got, err := serp.Candidates(context.Background(), "milk", 5)
	require.NoError(t, err)

	require.Len(t, got, 1, "this server fetches whatever a provider names; only absolute http(s) is allowed")
	assert.Equal(t, "https://cdn.example/ok.jpg", got[0].SourceURL)
}
