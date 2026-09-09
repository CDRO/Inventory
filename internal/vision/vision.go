// Package vision owns the Gemini client and, at this stage, the model
// resilience rules from docs/specs/01-architecture-and-deployment.md.
//
// Pinned model ids get deprecated and then simply stop working. The rule this
// package enforces is that such a deployment degrades visibly rather than
// opaquely: the application still starts, every non-vision feature keeps
// working, and the vision surface reports model_unavailable naming the model
// it was asked for.
package vision

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Status values reported by Checker.Status and by GET /healthz.
const (
	// StatusOK means the effective model appears in the provider's model list.
	StatusOK = "ok"
	// StatusModelUnavailable means the configured model is not one the
	// provider currently offers. Vision endpoints answer 503 model_unavailable
	// while this holds; everything else is unaffected.
	StatusModelUnavailable = "model_unavailable"
)

// settingsModelKey is the settings-table row that outranks GEMINI_MODEL.
const settingsModelKey = "gemini_model"

// defaultListEndpoint is Gemini's model catalogue.
const defaultListEndpoint = "https://generativelanguage.googleapis.com/v1beta/models"

// SettingsReader reads the database-backed configuration overrides. It is an
// interface so the checker can be exercised without a database.
type SettingsReader interface {
	// Setting returns the value for key and whether one exists.
	Setting(ctx context.Context, key string) (string, bool, error)
}

// ModelLister returns the model ids the provider currently offers.
type ModelLister interface {
	ListModels(ctx context.Context) ([]string, error)
}

// Checker resolves the effective model and reports whether it is available.
//
// The available-model list is cached: Status is called on every /healthz hit
// and must not turn a readiness probe into an outbound API call. The cache is
// refreshed on first use, when it expires, and whenever Invalidate is called —
// which is what a model-not-found error from a vision call should do.
type Checker struct {
	settings SettingsReader
	lister   ModelLister
	envModel string
	ttl      time.Duration
	now      func() time.Time

	mu        sync.Mutex
	models    map[string]struct{}
	fetchedAt time.Time
	fetchErr  error
	// inflight is non-nil while one goroutine is fetching the model list, and
	// is closed when that fetch completes. Concurrent callers wait on it
	// instead of queueing behind the mutex, so a burst of readiness probes
	// costs one outbound request rather than one convoy.
	inflight chan struct{}
}

// NewChecker builds a Checker. envModel is the GEMINI_MODEL fallback; settings
// may be nil, in which case only envModel is consulted.
func NewChecker(settings SettingsReader, lister ModelLister, envModel string) *Checker {
	return &Checker{
		settings: settings,
		lister:   lister,
		envModel: envModel,
		ttl:      5 * time.Minute,
		now:      time.Now,
	}
}

// EffectiveModel returns the model id the application should use: the
// settings-table value when present, otherwise the environment's. The database
// always wins, so an operator can correct a dead model id in the admin UI and
// have it apply immediately, with no restart.
//
// A settings read that fails is reported; it is not silently treated as
// "no override", because that would mask a broken database behind an id that
// merely looks plausible.
func (c *Checker) EffectiveModel(ctx context.Context) (string, error) {
	if c.settings == nil {
		return c.envModel, nil
	}
	value, ok, err := c.settings.Setting(ctx, settingsModelKey)
	if err != nil {
		return "", fmt.Errorf("vision: resolve effective model: %w", err)
	}
	if ok && strings.TrimSpace(value) != "" {
		return strings.TrimSpace(value), nil
	}
	return c.envModel, nil
}

// Status reports StatusOK when the effective model is offered by the provider,
// and StatusModelUnavailable otherwise.
//
// A provider or database that cannot be reached also yields
// StatusModelUnavailable: the honest answer to "can this deployment do vision
// work right now" is no, and reporting ok would hide the outage behind a green
// health check.
func (c *Checker) Status(ctx context.Context) string {
	model, err := c.EffectiveModel(ctx)
	if err != nil || model == "" {
		return StatusModelUnavailable
	}
	available, err := c.available(ctx)
	if err != nil {
		return StatusModelUnavailable
	}
	if _, ok := available[model]; ok {
		return StatusOK
	}
	return StatusModelUnavailable
}

// Models returns the cached list of available model ids, for the admin banner
// that offers a replacement when the configured one has gone away.
func (c *Checker) Models(ctx context.Context) ([]string, error) {
	available, err := c.available(ctx)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(available))
	for id := range available {
		ids = append(ids, id)
	}
	return ids, nil
}

// Invalidate drops the cached model list so the next call refetches it. Call it
// when the provider reports that a model was not found.
func (c *Checker) Invalidate() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.models = nil
	c.fetchedAt = time.Time{}
	c.fetchErr = nil
}

// available returns the cached model set, fetching it when the cache is cold or
// stale. The outbound call is made without holding c.mu: a readiness probe runs
// on every /healthz hit, and blocking every concurrent probe on one in-flight
// HTTP request would turn a health check into a queue.
func (c *Checker) available(ctx context.Context) (map[string]struct{}, error) {
	// c.lister is set once at construction and never reassigned.
	if c.lister == nil {
		return nil, fmt.Errorf("vision: no model lister configured")
	}

	for {
		c.mu.Lock()
		if c.models != nil && c.now().Sub(c.fetchedAt) < c.ttl {
			models, err := c.models, c.fetchErr
			c.mu.Unlock()
			return models, err
		}
		if wait := c.inflight; wait != nil {
			c.mu.Unlock()
			select {
			case <-wait:
				continue // re-read the cache the winner just populated
			case <-ctx.Done():
				return nil, fmt.Errorf("vision: await model list: %w", ctx.Err())
			}
		}
		done := make(chan struct{})
		c.inflight = done
		c.mu.Unlock()

		models, err := c.fetch(ctx)
		close(done)
		return models, err
	}
}

// fetch performs the outbound call and stores the result. It must only be
// called by the goroutine that installed c.inflight.
func (c *Checker) fetch(ctx context.Context) (map[string]struct{}, error) {
	ids, listErr := c.lister.ListModels(ctx)

	c.mu.Lock()
	defer c.mu.Unlock()

	c.fetchedAt = c.now()
	c.inflight = nil

	if listErr != nil {
		// Cache the failure too, so a provider outage does not produce one
		// outbound attempt per probe for the whole of the TTL.
		c.models = map[string]struct{}{}
		c.fetchErr = fmt.Errorf("vision: list models: %w", listErr)
		return c.models, c.fetchErr
	}

	set := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		set[normalizeModelID(id)] = struct{}{}
	}
	c.models, c.fetchErr = set, nil
	return c.models, nil
}

// normalizeModelID strips the "models/" prefix the API returns, so a
// GEMINI_MODEL of "gemini-2.0-flash" matches a listed "models/gemini-2.0-flash".
func normalizeModelID(id string) string {
	return strings.TrimPrefix(strings.TrimSpace(id), "models/")
}

// APILister calls Gemini's models.list endpoint.
type APILister struct {
	APIKey   string
	Endpoint string
	HTTP     *http.Client
}

// NewAPILister returns a lister for the public Gemini endpoint.
func NewAPILister(apiKey string) *APILister {
	return &APILister{
		APIKey:   apiKey,
		Endpoint: defaultListEndpoint,
		HTTP:     &http.Client{Timeout: 10 * time.Second},
	}
}

// ListModels returns the model ids the API reports, following pagination.
func (l *APILister) ListModels(ctx context.Context) ([]string, error) {
	endpoint := l.Endpoint
	if endpoint == "" {
		endpoint = defaultListEndpoint
	}
	client := l.HTTP
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}

	var ids []string
	pageToken := ""
	for page := 0; page < 10; page++ { // bounded: never loop on a stuck token
		// Built with url.Values rather than concatenated: a page token
		// containing '+', '=' or '&' would otherwise truncate the query and
		// silently end pagination early.
		query := url.Values{}
		query.Set("key", l.APIKey)
		query.Set("pageSize", "200")
		if pageToken != "" {
			query.Set("pageToken", pageToken)
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"?"+query.Encode(), nil)
		if err != nil {
			return nil, fmt.Errorf("vision: build models.list request: %w", err)
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("vision: call models.list: %w", err)
		}

		body, err := decodeModelList(resp)
		if err != nil {
			return nil, err
		}
		for _, m := range body.Models {
			ids = append(ids, m.Name)
		}
		if body.NextPageToken == "" {
			return ids, nil
		}
		pageToken = body.NextPageToken
	}
	return ids, nil
}

type modelListResponse struct {
	Models []struct {
		Name string `json:"name"`
	} `json:"models"`
	NextPageToken string `json:"nextPageToken"`
}

// decodeModelList closes resp.Body on every path, including the error ones.
func decodeModelList(resp *http.Response) (*modelListResponse, error) {
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("vision: models.list returned %s", resp.Status)
	}
	var body modelListResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("vision: decode models.list: %w", err)
	}
	return &body, nil
}
