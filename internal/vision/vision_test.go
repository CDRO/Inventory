package vision

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type stubSettings struct {
	value string
	ok    bool
	err   error
}

func (s stubSettings) Setting(context.Context, string) (string, bool, error) {
	return s.value, s.ok, s.err
}

type stubLister struct {
	models []string
	err    error
	calls  int
}

func (s *stubLister) ListModels(context.Context) ([]string, error) {
	s.calls++
	return s.models, s.err
}

// TestEffectiveModelPrefersSettings is the whole point of the settings
// override: an operator whose pinned model id was deprecated corrects it in
// the admin UI and it applies immediately. If the environment won, the fix
// would require editing .env and recreating the container.
func TestEffectiveModelPrefersSettings(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		settings SettingsReader
		envModel string
		want     string
	}{
		{
			name:     "settings row wins over the environment",
			settings: stubSettings{value: "gemini-2.5-flash", ok: true},
			envModel: "gemini-2.0-flash",
			want:     "gemini-2.5-flash",
		},
		{
			name:     "no settings row falls back to the environment",
			settings: stubSettings{ok: false},
			envModel: "gemini-2.0-flash",
			want:     "gemini-2.0-flash",
		},
		{
			name:     "a blank settings row is not an override",
			settings: stubSettings{value: "   ", ok: true},
			envModel: "gemini-2.0-flash",
			want:     "gemini-2.0-flash",
		},
		{
			name:     "no settings reader at all uses the environment",
			settings: nil,
			envModel: "gemini-2.0-flash",
			want:     "gemini-2.0-flash",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			c := NewChecker(tc.settings, &stubLister{}, tc.envModel)
			got, err := c.EffectiveModel(context.Background())

			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestEffectiveModelSurfacesSettingsFailure guards against a broken database
// being masked by a plausible-looking environment default.
func TestEffectiveModelSurfacesSettingsFailure(t *testing.T) {
	t.Parallel()

	c := NewChecker(stubSettings{err: errors.New("connection refused")}, &stubLister{}, "gemini-2.0-flash")

	_, err := c.EffectiveModel(context.Background())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "connection refused", "the cause must survive wrapping")
}

// TestStatus covers the resilience rule: a configured model the provider no
// longer offers must report model_unavailable rather than being assumed fine.
func TestStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		envModel string
		lister   *stubLister
		want     string
	}{
		{
			name:     "configured model is offered",
			envModel: "gemini-2.0-flash",
			lister:   &stubLister{models: []string{"models/gemini-2.0-flash", "models/gemini-2.5-pro"}},
			want:     StatusOK,
		},
		{
			name:     "configured model was deprecated away",
			envModel: "gemini-1.0-pro",
			lister:   &stubLister{models: []string{"models/gemini-2.0-flash"}},
			want:     StatusModelUnavailable,
		},
		{
			name:     "provider unreachable is reported, not assumed healthy",
			envModel: "gemini-2.0-flash",
			lister:   &stubLister{err: errors.New("dial tcp: timeout")},
			want:     StatusModelUnavailable,
		},
		{
			name:     "empty model id is unusable",
			envModel: "",
			lister:   &stubLister{models: []string{"models/gemini-2.0-flash"}},
			want:     StatusModelUnavailable,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			c := NewChecker(nil, tc.lister, tc.envModel)
			assert.Equal(t, tc.want, c.Status(context.Background()))
		})
	}
}

// TestStatusMatchesUnprefixedModelID pins the normalization that makes the
// comparison work at all: the API answers "models/gemini-2.0-flash" while
// GEMINI_MODEL is written "gemini-2.0-flash". Without it every deployment
// would report model_unavailable.
func TestStatusMatchesUnprefixedModelID(t *testing.T) {
	t.Parallel()

	c := NewChecker(nil, &stubLister{models: []string{"models/gemini-2.0-flash"}}, "gemini-2.0-flash")

	assert.Equal(t, StatusOK, c.Status(context.Background()))
}

// TestStatusCachesModelList protects the readiness probe: /healthz calls
// Status on every hit, and an uncached implementation would turn a container
// healthcheck into a per-second outbound API call.
func TestStatusCachesModelList(t *testing.T) {
	t.Parallel()

	lister := &stubLister{models: []string{"models/gemini-2.0-flash"}}
	c := NewChecker(nil, lister, "gemini-2.0-flash")

	for i := 0; i < 5; i++ {
		require.Equal(t, StatusOK, c.Status(context.Background()))
	}
	assert.Equal(t, 1, lister.calls, "the model list must be fetched once, not once per probe")
}

// TestInvalidateForcesRefetch covers the recovery path: after a
// model-not-found error the cache is dropped, and the next check sees the
// provider's current list.
func TestInvalidateForcesRefetch(t *testing.T) {
	t.Parallel()

	lister := &stubLister{models: []string{"models/gemini-1.0-pro"}}
	c := NewChecker(nil, lister, "gemini-2.0-flash")

	require.Equal(t, StatusModelUnavailable, c.Status(context.Background()))
	require.Equal(t, 1, lister.calls)

	lister.models = []string{"models/gemini-2.0-flash"}
	c.Invalidate()

	assert.Equal(t, StatusOK, c.Status(context.Background()))
	assert.Equal(t, 2, lister.calls)
}

// TestCacheExpiresAfterTTL drives the clock rather than sleeping, so a stale
// cache is caught without making the suite slow.
func TestCacheExpiresAfterTTL(t *testing.T) {
	t.Parallel()

	lister := &stubLister{models: []string{"models/gemini-2.0-flash"}}
	c := NewChecker(nil, lister, "gemini-2.0-flash")

	clock := time.Now()
	c.now = func() time.Time { return clock }

	require.Equal(t, StatusOK, c.Status(context.Background()))
	require.Equal(t, 1, lister.calls)

	clock = clock.Add(c.ttl + time.Second)

	require.Equal(t, StatusOK, c.Status(context.Background()))
	assert.Equal(t, 2, lister.calls, "an expired cache must be refetched")
}

// TestModelsListsAvailableIDs covers the data behind the admin banner that
// offers a replacement when the configured model is gone.
func TestModelsListsAvailableIDs(t *testing.T) {
	t.Parallel()

	c := NewChecker(nil, &stubLister{models: []string{"models/gemini-2.0-flash", "gemini-2.5-pro"}}, "gemini-2.0-flash")

	got, err := c.Models(context.Background())

	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"gemini-2.0-flash", "gemini-2.5-pro"}, got)
}
