package notify_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/CDRO/Inventory/internal/notify"
	"github.com/CDRO/Inventory/internal/store"
)

// capture records what a target actually received, which is the only way to
// check the promises about the outbound request: no cookies, no credentials,
// and nothing beyond the digest and the configured token.
type capture struct {
	requests []recorded
	status   int
	location string
}

type recorded struct {
	method string
	path   string
	query  string
	header http.Header
	body   string
}

func (c *capture) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		c.requests = append(c.requests, recorded{
			method: r.Method, path: r.URL.Path, query: r.URL.RawQuery,
			header: r.Header.Clone(), body: string(body),
		})
		if c.location != "" {
			w.Header().Set("Location", c.location)
		}
		status := c.status
		if status == 0 {
			status = http.StatusOK
		}
		w.WriteHeader(status)
	}
}

func settingsFor(kind, url, token string) store.NotificationSettings {
	return store.NotificationSettings{
		StorageID: uuid.New(), Enabled: true, Kind: kind, URL: url, Token: token, SendHour: 8,
	}
}

func sampleDigest(t *testing.T) notify.Digest {
	t.Helper()
	today := day(2026, time.September, 21)
	return notify.Build([]store.DigestItem{
		item("Milk", 2, "Fridge > Door", day(2026, time.September, 19)),
	}, today, false)
}

func TestDeliverNtfyPostsThePlainTextDigest(t *testing.T) {
	target := &capture{}
	server := httptest.NewServer(target.handler())
	defer server.Close()

	service := notify.New(&fakeStore{}, discardLogger())
	require.NoError(t, service.Deliver(context.Background(),
		settingsFor(store.NotificationNtfy, server.URL+"/inventory", "tk_secret"), sampleDigest(t)))

	require.Len(t, target.requests, 1)
	got := target.requests[0]
	require.Equal(t, http.MethodPost, got.method)
	require.Equal(t, "/inventory", got.path)
	require.Contains(t, got.body, "Milk")
	require.Contains(t, got.header.Get("Title"), "1 expired")
	require.Equal(t, "Bearer tk_secret", got.header.Get("Authorization"))
}

func TestDeliverGotifyPostsToTheMessagePathWithTheToken(t *testing.T) {
	target := &capture{}
	server := httptest.NewServer(target.handler())
	defer server.Close()

	service := notify.New(&fakeStore{}, discardLogger())
	require.NoError(t, service.Deliver(context.Background(),
		settingsFor(store.NotificationGotify, server.URL, "tk_secret"), sampleDigest(t)))

	require.Len(t, target.requests, 1)
	got := target.requests[0]
	require.Equal(t, "/message", got.path)
	require.Equal(t, "token=tk_secret", got.query, "gotify takes its token in the query string, as its API expects")

	var payload struct {
		Title    string `json:"title"`
		Message  string `json:"message"`
		Priority int    `json:"priority"`
	}
	require.NoError(t, json.Unmarshal([]byte(got.body), &payload))
	require.Contains(t, payload.Message, "Milk")
	require.Equal(t, 4, payload.Priority)
}

func TestDeliverWebhookPostsTheStructuredPayload(t *testing.T) {
	target := &capture{}
	server := httptest.NewServer(target.handler())
	defer server.Close()

	service := notify.New(&fakeStore{}, discardLogger())
	require.NoError(t, service.Deliver(context.Background(),
		settingsFor(store.NotificationWebhook, server.URL+"/hook", "tk_secret"), sampleDigest(t)))

	require.Len(t, target.requests, 1)
	got := target.requests[0]

	var payload struct {
		Title   string `json:"title"`
		Message string `json:"message"`
		Items   []struct {
			Name           string `json:"name"`
			Quantity       int    `json:"quantity"`
			Location       string `json:"location"`
			ExpirationDate string `json:"expiration_date"`
			Urgency        string `json:"urgency"`
		} `json:"items"`
	}
	require.NoError(t, json.Unmarshal([]byte(got.body), &payload))
	require.Len(t, payload.Items, 1)
	require.Equal(t, "Milk", payload.Items[0].Name)
	require.Equal(t, 2, payload.Items[0].Quantity)
	require.Equal(t, "Fridge > Door", payload.Items[0].Location)
	require.Equal(t, "2026-09-19", payload.Items[0].ExpirationDate)
	require.Equal(t, "expired", payload.Items[0].Urgency)
}

// The request carries the digest and the configured token, and nothing else:
// no cookie is attached, and with no token configured there is no
// Authorization header to attach either.
func TestDeliverCarriesNoCredentialsOfItsOwn(t *testing.T) {
	target := &capture{}
	server := httptest.NewServer(target.handler())
	defer server.Close()

	service := notify.New(&fakeStore{}, discardLogger())
	require.NoError(t, service.Deliver(context.Background(),
		settingsFor(store.NotificationNtfy, server.URL+"/inventory", ""), sampleDigest(t)))

	got := target.requests[0]
	require.Empty(t, got.header.Values("Cookie"))
	require.Empty(t, got.header.Get("Authorization"))
	require.Empty(t, got.header.Get("X-Api-Key"))
}

// "Redirect responses from the target are treated as delivery failure, not
// followed." The second server exists so the test fails loudly if the client
// ever does follow one.
func TestDeliverTreatsARedirectAsFailureAndDoesNotFollowIt(t *testing.T) {
	elsewhere := &capture{}
	second := httptest.NewServer(elsewhere.handler())
	defer second.Close()

	target := &capture{status: http.StatusFound, location: second.URL + "/elsewhere"}
	server := httptest.NewServer(target.handler())
	defer server.Close()

	service := notify.New(&fakeStore{}, discardLogger())
	err := service.Deliver(context.Background(),
		settingsFor(store.NotificationNtfy, server.URL+"/inventory", "tk_secret"), sampleDigest(t))

	require.Error(t, err)
	var delivery *notify.DeliveryError
	require.ErrorAs(t, err, &delivery)
	require.Contains(t, delivery.Summary(), "redirect")
	require.Empty(t, elsewhere.requests, "the second hop was never made")
}

// The summary is stored in last_result and returned by every read of the
// settings row, so it must never contain the token. net/http's own error
// text would: it embeds the request URL, and a gotify URL carries the token
// in its query string.
func TestDeliveryFailureSummaryNeverCarriesTheToken(t *testing.T) {
	target := &capture{status: http.StatusUnauthorized}
	server := httptest.NewServer(target.handler())
	defer server.Close()

	service := notify.New(&fakeStore{}, discardLogger())
	err := service.Deliver(context.Background(),
		settingsFor(store.NotificationGotify, server.URL, "tk_super_secret"), sampleDigest(t))

	var delivery *notify.DeliveryError
	require.ErrorAs(t, err, &delivery)
	require.Contains(t, delivery.Summary(), "401")
	require.NotContains(t, delivery.Summary(), "tk_super_secret")
	require.NotContains(t, delivery.Summary(), server.URL)
}

// An unreachable target must also summarise without the URL — this is the
// path where the transport error, not a status code, supplies the reason.
func TestUnreachableTargetSummarisesWithoutTheURL(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	unreachable := server.URL
	server.Close()

	service := notify.New(&fakeStore{}, discardLogger())
	err := service.Deliver(context.Background(),
		settingsFor(store.NotificationGotify, unreachable, "tk_super_secret"), sampleDigest(t))

	var delivery *notify.DeliveryError
	require.ErrorAs(t, err, &delivery)
	require.NotContains(t, delivery.Summary(), "tk_super_secret")
	require.NotContains(t, delivery.Summary(), unreachable)
	require.Contains(t, delivery.Error(), "tk_super_secret",
		"the full error still carries it — that one is logged, never stored")
}

func TestParseTargetRestrictsSchemes(t *testing.T) {
	for _, raw := range []string{"file:///etc/passwd", "gopher://x/1", "ftp://host/x", "notaurl", "https://", ""} {
		_, err := notify.ParseTarget(raw)
		require.Error(t, err, raw)
	}
	for _, raw := range []string{"http://gotify.lan", "https://ntfy.example/inventory"} {
		_, err := notify.ParseTarget(raw)
		require.NoError(t, err, raw)
	}
}
