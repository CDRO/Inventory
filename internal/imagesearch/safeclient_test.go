package imagesearch_test

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CDRO/Inventory/internal/imagesearch"
	"github.com/CDRO/Inventory/internal/store"
)

// This server fetches URLs that came from a third-party API, from inside the
// deployment's own network. On a home NAS that network contains the router's
// admin page and every other container; on a cloud host it contains the
// instance metadata endpoint. These are the addresses that must never be
// reachable through an image suggestion.
func TestIsBlockedIP(t *testing.T) {
	t.Parallel()

	blocked := []struct {
		name string
		ip   string
	}{
		{"cloud instance metadata", "169.254.169.254"},
		{"link-local generally", "169.254.1.1"},
		{"loopback", "127.0.0.1"},
		{"loopback, not just .1", "127.9.9.9"},
		{"private 10/8", "10.0.0.1"},
		{"private 172.16/12", "172.16.5.4"},
		{"private 192.168/16", "192.168.1.1"},
		{"carrier-grade NAT", "100.64.0.1"},
		{"unspecified", "0.0.0.0"},
		{"multicast", "224.0.0.1"},
		{"IPv6 loopback", "::1"},
		{"IPv6 unique local", "fd00::1"},
		{"IPv6 link-local", "fe80::1"},
		{"IPv6 unspecified", "::"},
		// The mapped forms matter: a resolver can hand back ::ffff:127.0.0.1,
		// and judging it as an IPv6 address rather than the IPv4 one it
		// actually is would let loopback straight through.
		{"IPv4-mapped loopback", "::ffff:127.0.0.1"},
		{"IPv4-mapped metadata", "::ffff:169.254.169.254"},
	}

	for _, tc := range blocked {
		t.Run("blocked/"+tc.name, func(t *testing.T) {
			t.Parallel()

			ip := net.ParseIP(tc.ip)
			require.NotNil(t, ip, "test address must parse")
			assert.True(t, imagesearch.IsBlockedIP(ip), "%s must be refused", tc.ip)
		})
	}

	allowed := []struct {
		name string
		ip   string
	}{
		{"a public host", "93.184.216.34"},
		{"a public DNS resolver", "8.8.8.8"},
		{"public IPv6", "2606:2800:220:1:248:1893:25c8:1946"},
		// 100.128.0.0 is just outside the carrier-grade NAT range; the bound
		// has to be a range check, not a first-octet check.
		{"just past the CGNAT range", "100.128.0.1"},
	}

	for _, tc := range allowed {
		t.Run("allowed/"+tc.name, func(t *testing.T) {
			t.Parallel()

			ip := net.ParseIP(tc.ip)
			require.NotNil(t, ip)
			assert.False(t, imagesearch.IsBlockedIP(ip), "%s is on the public internet", tc.ip)
		})
	}

	t.Run("blocked/nil", func(t *testing.T) {
		t.Parallel()
		assert.True(t, imagesearch.IsBlockedIP(nil), "an unparseable address is not something to connect to")
	})
}

// TestSafeClientRefusesToReachAPrivateAddress is the end-to-end version: the
// guard is at dial time, so it holds however the address was arrived at.
func TestSafeClientRefusesToReachAPrivateAddress(t *testing.T) {
	t.Parallel()

	// A loopback server stands in for anything on the internal network. It is
	// running and would answer — the point is that the client never connects.
	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("secrets"))
	}))
	defer internal.Close()

	client := imagesearch.SafeHTTPClient(5 * time.Second)

	resp, err := client.Get(internal.URL)
	if resp != nil {
		_ = resp.Body.Close()
	}

	require.Error(t, err, "a private address must not be reachable")
	assert.Contains(t, err.Error(), "refusing to connect")
}

// TestSafeClientRefusesARedirectIntoTheInternalNetwork is the bug review-go
// found with a proof of concept: URL validation runs once on the string a
// provider supplied, and Go's default client then follows a 302 anywhere it
// is pointed. A perfectly ordinary CDN URL can answer
// `302 Location: http://169.254.169.254/…`.
//
// Both hops here are on loopback, which is the point: the dial guard refuses
// them for the same reason it would refuse the metadata endpoint, so the test
// does not need a real internal address to prove the behaviour.
func TestSafeClientRefusesARedirectIntoTheInternalNetwork(t *testing.T) {
	t.Parallel()

	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("secrets"))
	}))
	defer internal.Close()

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, internal.URL, http.StatusFound)
	}))
	defer redirector.Close()

	client := imagesearch.SafeHTTPClient(5 * time.Second)

	resp, err := client.Get(redirector.URL)
	if resp != nil {
		_ = resp.Body.Close()
	}

	require.Error(t, err)
	assert.NotContains(t, err.Error(), "secrets", "the internal body must never be read")
}

// TestTheDefaultCacheClientIsGuarded closes the gap review-go named: every
// other test in this package injects its own plain client via httptest, so
// until this one nothing proved that a Cache built the way production builds
// it — with no client supplied — actually gets the guarded transport.
//
// A wiring mistake in NewCache would leave the whole SSRF guard inert while
// safeclient_test.go stayed green, because that file tests SafeHTTPClient
// directly rather than the path Fetch takes.
func TestTheDefaultCacheClientIsGuarded(t *testing.T) {
	t.Parallel()

	// Stands in for anything on the internal network. It is running and would
	// answer; the point is that Fetch never reaches it.
	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("secrets"))
	}))
	defer internal.Close()

	mem := newMemStore()
	// nil client: exactly how cmd/inventory builds it.
	cache := imagesearch.NewCache(t.TempDir(), mem, nil, discardLogger())

	_, err := cache.Fetch(context.Background(), internal.URL+"/photo.png")

	require.Error(t, err, "the default client must refuse a private address")
	assert.Contains(t, err.Error(), "refusing to connect")

	// And the refusal is not recorded as a permanent verdict, since the
	// candidate itself was never shown to be unusable.
	_, lookupErr := mem.CachedImageByHash(context.Background(),
		imagesearch.HashURL(internal.URL+"/photo.png"))
	assert.ErrorIs(t, lookupErr, store.ErrNotFound)
}

// TestSafeClientStopsRedirectLoops keeps a hostile or broken CDN from tying up
// a request indefinitely.
func TestSafeClientStopsRedirectLoops(t *testing.T) {
	t.Parallel()

	client := imagesearch.SafeHTTPClient(5 * time.Second)
	require.NotNil(t, client.CheckRedirect)

	req, err := http.NewRequest(http.MethodGet, "https://cdn.example/final.jpg", nil)
	require.NoError(t, err)

	// Under the cap: allowed.
	assert.NoError(t, client.CheckRedirect(req, make([]*http.Request, 3)))

	// At the cap: refused.
	assert.Error(t, client.CheckRedirect(req, make([]*http.Request, 10)))
}

// TestSafeClientRefusesANonHTTPRedirect — a redirect is a fresh URL from the
// same untrusted source, so it gets the same scheme rule as the first one.
func TestSafeClientRefusesANonHTTPRedirect(t *testing.T) {
	t.Parallel()

	client := imagesearch.SafeHTTPClient(5 * time.Second)

	req, err := http.NewRequest(http.MethodGet, "file:///etc/passwd", nil)
	require.NoError(t, err)

	assert.Error(t, client.CheckRedirect(req, nil))
}
