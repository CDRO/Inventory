package imagesearch

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Provider endpoints. Declared here rather than inline so a test can point
// them at a local server and so an operator can see, in one place, every host
// this system talks to.
const (
	iconifySearchURL = "https://api.iconify.design/search"
	iconifySVGHost   = "https://api.iconify.design"
	serpAPIURL       = "https://serpapi.com/search"
)

// FallbackIcon is used when Iconify has nothing relevant.
//
// The spec asks for a generic box rather than an omitted suggestion: a New Item
// card with an obviously generic icon still lets the user proceed, while a
// missing third slot looks like the page failed.
const FallbackIcon = "mdi:package-variant-closed"

// SuggestionType distinguishes a vector icon from a photograph.
type SuggestionType string

const (
	TypeIcon  SuggestionType = "icon"
	TypePhoto SuggestionType = "photo"
)

// Candidate is a provider result before it has been fetched.
//
// SourceURL is a provider URL and never leaves the server: the whole point of
// this package is that the browser is handed a URL on our own origin instead.
type Candidate struct {
	SourceURL string
	Type      SuggestionType
	Source    string // "iconify" or "serpapi"
}

// Provider finds candidate image URLs for a query.
type Provider interface {
	// Candidates returns at most limit candidate URLs. An unreachable or
	// rate-limited provider returns an error; the caller degrades to fewer
	// suggestions rather than failing the New Item flow.
	Candidates(ctx context.Context, query string, limit int) ([]Candidate, error)
}

// Iconify searches api.iconify.design, which needs no API key.
type Iconify struct {
	client    *http.Client
	searchURL string
	svgHost   string
}

// NewIconify returns an Iconify provider pointed at the real service.
func NewIconify(client *http.Client) *Iconify {
	return NewIconifyWithEndpoints(client, iconifySearchURL, iconifySVGHost)
}

// NewIconifyWithEndpoints returns a provider pointed at the given endpoints.
//
// This exists so tests can stand up a local stub and exercise the real
// request-building and identifier-validation paths. Those are the parts worth
// testing — the identifier arrives from a remote service and is turned into a
// URL this server will fetch — and testing them against the live Iconify API
// would make the suite depend on the internet.
func NewIconifyWithEndpoints(client *http.Client, searchURL, svgHost string) *Iconify {
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	return &Iconify{client: client, searchURL: searchURL, svgHost: svgHost}
}

type iconifySearchResponse struct {
	Icons []string `json:"icons"`
}

// Candidates returns rendered-SVG URLs for the best matching icons.
//
// A miss is not an error: it returns the generic fallback icon, so the caller
// always has an icon slot to fill.
func (i *Iconify) Candidates(ctx context.Context, query string, limit int) ([]Candidate, error) {
	if limit <= 0 {
		return nil, nil
	}

	endpoint := i.searchURL + "?" + url.Values{
		"query": {query},
		"limit": {"10"},
	}.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("imagesearch: build iconify request: %w", err)
	}

	resp, err := i.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("imagesearch: iconify search: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("imagesearch: iconify search returned %d", resp.StatusCode)
	}

	var body iconifySearchResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("imagesearch: decode iconify search: %w", err)
	}

	names := body.Icons
	if len(names) == 0 {
		names = []string{FallbackIcon}
	}

	out := make([]Candidate, 0, limit)
	for _, name := range names {
		if len(out) == limit {
			break
		}
		svgURL, ok := i.svgURL(name)
		if !ok {
			continue
		}
		out = append(out, Candidate{SourceURL: svgURL, Type: TypeIcon, Source: "iconify"})
	}
	return out, nil
}

// svgURL turns an Iconify "prefix:name" identifier into its rendered-SVG URL.
//
// The identifier comes from a remote service, so it is validated rather than
// interpolated: without this, a crafted response containing "../" or a full URL
// would have this server fetching whatever it named.
func (i *Iconify) svgURL(identifier string) (string, bool) {
	prefix, name, found := strings.Cut(identifier, ":")
	if !found || prefix == "" || name == "" {
		return "", false
	}
	if !isIconifySegment(prefix) || !isIconifySegment(name) {
		return "", false
	}
	return fmt.Sprintf("%s/%s/%s.svg", i.svgHost, prefix, name), true
}

// isIconifySegment allows only the characters Iconify actually uses in a
// prefix or icon name.
func isIconifySegment(segment string) bool {
	if segment == "" || len(segment) > 64 {
		return false
	}
	for _, r := range segment {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}

// SerpAPI searches Google Images through serpapi.com.
type SerpAPI struct {
	client   *http.Client
	endpoint string
	apiKey   string
}

// NewSerpAPI returns a SerpAPI provider. An empty apiKey disables it: the
// system degrades to icon-only suggestions rather than failing, which is the
// documented behaviour when a provider is unavailable.
func NewSerpAPI(apiKey string, client *http.Client) *SerpAPI {
	return NewSerpAPIWithEndpoint(apiKey, client, serpAPIURL)
}

// NewSerpAPIWithEndpoint returns a provider pointed at the given endpoint, so
// tests can assert on the request this code actually builds — including that
// the API key goes to the provider and appears nowhere else.
func NewSerpAPIWithEndpoint(apiKey string, client *http.Client, endpoint string) *SerpAPI {
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	return &SerpAPI{client: client, endpoint: endpoint, apiKey: apiKey}
}

// Configured reports whether a key is present.
func (s *SerpAPI) Configured() bool { return s.apiKey != "" }

type serpAPIResponse struct {
	ImagesResults []struct {
		Original  string `json:"original"`
		Thumbnail string `json:"thumbnail"`
	} `json:"images_results"`
}

// Candidates returns the top image results for query.
//
// The API key is sent from here and appears in no response this package
// produces. That is not incidental: a key in a URL handed to the browser is a
// key in the user's history, in any proxy log on the way, and in the referrer
// of whatever they click next.
func (s *SerpAPI) Candidates(ctx context.Context, query string, limit int) ([]Candidate, error) {
	if limit <= 0 {
		return nil, nil
	}
	if !s.Configured() {
		return nil, fmt.Errorf("imagesearch: serpapi key not configured")
	}

	endpoint := s.endpoint + "?" + url.Values{
		"engine":  {"google_images"},
		"q":       {query},
		"api_key": {s.apiKey},
	}.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("imagesearch: build serpapi request: %w", err)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("imagesearch: serpapi search: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("imagesearch: serpapi returned %d", resp.StatusCode)
	}

	var body serpAPIResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("imagesearch: decode serpapi response: %w", err)
	}

	out := make([]Candidate, 0, limit)
	for _, result := range body.ImagesResults {
		if len(out) == limit {
			break
		}
		// Prefer the full-size original; the thumbnail is the fallback when a
		// result has no original. Either way it is downscaled on ingest.
		source := result.Original
		if source == "" {
			source = result.Thumbnail
		}
		if !isFetchableHTTPURL(source) {
			continue
		}
		out = append(out, Candidate{SourceURL: source, Type: TypePhoto, Source: "serpapi"})
	}
	return out, nil
}

// isFetchableHTTPURL rejects anything this server should not be asked to
// retrieve.
//
// The URL comes from a third-party API response, and this server will fetch
// whatever it names from inside the deployment's own network. Restricting it
// to absolute http(s) URLs keeps a crafted response from turning the image
// fetcher into a way to probe the LAN through file:, gopher: or a bare path.
func isFetchableHTTPURL(raw string) bool {
	parsed, err := url.Parse(raw)
	if err != nil {
		return false
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return false
	}
	return parsed.Host != ""
}
