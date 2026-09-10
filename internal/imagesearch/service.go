package imagesearch

import (
	"context"
	"errors"
	"log/slog"
)

// Suggestion counts (docs/specs/07-shopping-list-reconciliation.md).
const (
	// WantIcons is the vector slot: one icon, always attempted, with a generic
	// fallback so the slot is filled even when nothing relevant is found.
	WantIcons = 1
	// WantPhotos is the photograph slots.
	WantPhotos = 2
	// WantTotal is what a complete response looks like.
	WantTotal = WantIcons + WantPhotos

	// candidateOverFetch is how many extra provider results to ask for.
	// Candidates are dropped when they fail to download or normalize, and
	// asking for exactly three would leave gaps every time one did.
	candidateOverFetch = 3
)

// Suggestion is one image offered to the user.
//
// URL is a path on our own origin. There is deliberately no field carrying the
// provider's URL: a struct that could hold one is a struct that will eventually
// be serialized with one.
type Suggestion struct {
	Type   SuggestionType `json:"type"`
	URL    string         `json:"url"`
	Source string         `json:"source"`
}

// Service produces image suggestions for a product name.
type Service struct {
	icons  Provider
	photos Provider
	cache  *Cache
	log    *slog.Logger
}

// NewService wires the providers to the cache. Either provider may be nil,
// which simply means that kind of suggestion is unavailable.
func NewService(icons, photos Provider, cache *Cache, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{icons: icons, photos: photos, cache: cache, log: log}
}

// Suggest returns up to three suggestions for query, all served from our own
// origin.
//
// urlFor builds the client-facing URL from a cache hash; the caller supplies it
// because the route is storage-scoped and this package has no business knowing
// the shape of the HTTP surface.
//
// A provider that is unreachable or rate-limited costs its slots, not the whole
// response: the user can still proceed with a manually uploaded photo or none
// at all, and a New Item flow that failed outright because Google was busy
// would be a worse system.
func (s *Service) Suggest(ctx context.Context, query string, urlFor func(hash string) string) []Suggestion {
	out := make([]Suggestion, 0, WantTotal)

	out = append(out, s.collect(ctx, s.icons, query, WantIcons, urlFor)...)
	out = append(out, s.collect(ctx, s.photos, query, WantPhotos, urlFor)...)

	return out
}

// collect fetches candidates from one provider and turns the usable ones into
// suggestions.
func (s *Service) collect(ctx context.Context, provider Provider, query string, want int, urlFor func(string) string) []Suggestion {
	if provider == nil || want <= 0 {
		return nil
	}

	candidates, err := provider.Candidates(ctx, query, want+candidateOverFetch)
	if err != nil {
		s.log.WarnContext(ctx, "imagesearch: provider unavailable, degrading",
			slog.String("query", query), slog.Any("err", err))
		return nil
	}

	out := make([]Suggestion, 0, want)
	for _, candidate := range candidates {
		if len(out) == want {
			break
		}

		hash, err := s.cache.Fetch(ctx, candidate.SourceURL)
		if err != nil {
			// An unusable candidate is already remembered as such and will not
			// be attempted again; anything else is transient. Either way the
			// next candidate gets its turn.
			if !errors.Is(err, ErrUnusableCached) {
				s.log.WarnContext(ctx, "imagesearch: candidate skipped",
					slog.String("query", query), slog.Any("err", err))
			}
			continue
		}

		out = append(out, Suggestion{
			Type:   candidate.Type,
			URL:    urlFor(hash),
			Source: candidate.Source,
		})
	}
	return out
}
