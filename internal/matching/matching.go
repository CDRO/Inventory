// Package matching resolves a piece of free text — a shopping-list line, a
// label Gemini read off a shelf photo, a spoken consumption entry — to a
// product this storage already knows about, a product the anonymous catalog
// already describes, or neither.
//
// It is deliberately one package shared by
// docs/specs/07-shopping-list-reconciliation.md,
// docs/specs/06-vision-shelf-ingestion.md and
// docs/specs/09-consumption-logging.md. Three copies of "is this the same
// product?" would drift, and the one that drifted would be the one deciding
// whether to spend money on an external API call.
//
// # Nothing here calls an external service
//
// Both stages are database queries against indexes that already exist. Stage 3
// is not a third lookup — it is the *absence* of a result, reported so the
// caller can decide whether this is worth an image search. That ordering is
// the point: the expensive path is reached only when everything already known
// has missed, and this package never reaches it itself.
//
// # What the caller may show a user
//
// A catalog result carries display fields only. The catalog row's id, its
// created_at and any hint of which storage first described it stay inside this
// package — see CatalogMatch. A response that leaked them would tell a user
// that other households exist and that one of them owns this description,
// which docs/specs/03-auth-and-multi-tenancy.md forbids.
package matching

import (
	"context"
	"fmt"

	"github.com/google/uuid"
)

// Match thresholds, in trigram similarity (pg_trgm `similarity()`, 0..1).
//
// docs/specs/07-shopping-list-reconciliation.md calls these a starting default
// to be tuned against real behavior, which is exactly why they are named
// constants in one place rather than literals at the query sites: tuning them
// must be a one-line change with a visible diff, not an archaeology exercise.
const (
	// ExactThreshold is where a single best local match is confident enough to
	// be offered as "this is the product", pre-filled and ready to confirm.
	ExactThreshold = 0.6

	// AmbiguousThreshold is the floor for showing local candidates at all.
	// Below it, the text is treated as naming something this storage does not
	// have yet, and the catalog gets its turn.
	AmbiguousThreshold = 0.35

	// AmbiguousMargin decides "one clear winner" versus "several close
	// candidates". If the runner-up is within this much of the best score, a
	// human should choose — picking for them is how "cream" silently becomes
	// "sour cream" in someone's inventory.
	AmbiguousMargin = 0.05
)

// Result limits.
const (
	// MaxLocalCandidates bounds the ambiguous-state list. The UI asks a person
	// to pick one; a list longer than this is a search interface, not a
	// question.
	MaxLocalCandidates = 3

	// MaxVariants bounds the catalog variant siblings offered alongside a hit
	// (the spec suggests 5). Same reasoning: a short list of alternatives, not
	// a catalog browser.
	MaxVariants = 5
)

// Status is the state a matched line lands in. The values are the ones
// shopping_list_items.status accepts (docs/specs/02-data-model.md).
type Status string

const (
	// StatusExactMatch is a confident single local product.
	StatusExactMatch Status = "exact_match"
	// StatusAmbiguous is several plausible local products, or one that is only
	// plausible. A person picks.
	StatusAmbiguous Status = "ambiguous"
	// StatusNewItem is nothing local. Catalog may still have pre-filled data;
	// Result.Catalog says whether it did.
	StatusNewItem Status = "new_item"
)

// LocalCandidate is one product this storage already has.
type LocalCandidate struct {
	ProductID  uuid.UUID
	Name       string
	Similarity float64
}

// CatalogMatch is what the anonymous catalog knows about a product.
//
// The catalog row's own id is present because the *server* needs it — accepting
// a hit copies these fields into a storage-local product, and declining one
// links the newly created catalog row to it as a variant. It must never be
// serialized to a client: see the package doc, and the response types in
// internal/httpapi, which build their own display-only shapes rather than
// marshalling this struct.
type CatalogMatch struct {
	// ID is server-side only. Never put it in a response body.
	ID uuid.UUID

	DisplayName          string
	CategoryPath         *string
	ItemType             string
	ImageURL             *string
	IconName             *string
	DefaultShelfLifeDays *int

	// Variants are sibling or child display names, for the one-click
	// alternatives that make an abbreviated line usable: "thomatoes, c."
	// trigram-matches the base "tomatoes", and the variants then offer
	// "cherry tomatoes" — which no amount of string matching on ", c." would
	// have produced.
	Variants []CatalogVariant
}

// CatalogVariant is one alternative offered alongside a catalog hit.
type CatalogVariant struct {
	// ID is server-side only, for the same reason as CatalogMatch.ID.
	ID          uuid.UUID
	DisplayName string
}

// Result is the outcome of matching one piece of text.
type Result struct {
	// Query is the text that was matched, after normalization.
	Query string

	Status Status

	// Product is the confident local match. Set only when Status is
	// StatusExactMatch.
	Product *LocalCandidate

	// Candidates are the local products a person should choose between. Set
	// only when Status is StatusAmbiguous.
	Candidates []LocalCandidate

	// Catalog is the anonymous catalog's description, when stage 2 hit. Nil
	// for a stage-3 miss, which is the only case where the caller should
	// consider an external image search.
	Catalog *CatalogMatch
}

// NeedsExternalLookup reports whether every cheap stage missed.
//
// This is the one place that decides an external call is warranted, so the
// rule "stages 1 and 2 must both miss" lives here rather than being restated
// by each caller — one of which would eventually get it wrong in the expensive
// direction.
func (r Result) NeedsExternalLookup() bool {
	return r.Status == StatusNewItem && r.Catalog == nil
}

// Store is the slice of the database this package needs. It is an interface so
// the staging logic can be tested without a database, and so that the package
// cannot reach for anything else.
type Store interface {
	// SimilarProducts returns this storage's products ordered by descending
	// trigram similarity to text, considering only rows at or above
	// minSimilarity.
	SimilarProducts(ctx context.Context, storageID uuid.UUID, text string, minSimilarity float64, limit int) ([]LocalCandidate, error)

	// SimilarCatalogProduct returns the best catalog row for text, or nil when
	// nothing is close enough.
	SimilarCatalogProduct(ctx context.Context, text string, minSimilarity float64) (*CatalogMatch, error)

	// CatalogVariantsOf returns the siblings or children linked to a catalog
	// row through base_id, ranked by similarity to text.
	CatalogVariantsOf(ctx context.Context, catalogID uuid.UUID, text string, limit int) ([]CatalogVariant, error)
}

// Service runs the staged match.
type Service struct {
	store Store
}

// New returns a Service backed by store.
func New(store Store) *Service {
	return &Service{store: store}
}

// MatchProductCandidates resolves text against, in order, this storage's own
// products and then the anonymous catalog.
//
// Blank text is a validation error rather than a silent empty result: an empty
// shopping-list line is a caller bug, and answering "no match" would hide it
// behind a plausible-looking new_item the user then has to dismiss.
func (s *Service) MatchProductCandidates(ctx context.Context, storageID uuid.UUID, text string) (Result, error) {
	query := NormalizeQuery(text)
	if query == "" {
		return Result{}, fmt.Errorf("matching: cannot match empty text")
	}

	result := Result{Query: query}

	// Stage 1 — this storage's own products.
	local, err := s.store.SimilarProducts(ctx, storageID, query, AmbiguousThreshold, MaxLocalCandidates)
	if err != nil {
		return Result{}, fmt.Errorf("matching: local products: %w", err)
	}

	if len(local) > 0 {
		best := local[0]
		clearWinner := len(local) == 1 || best.Similarity-local[1].Similarity > AmbiguousMargin

		if best.Similarity >= ExactThreshold && clearWinner {
			result.Status = StatusExactMatch
			result.Product = &best
			return result, nil
		}

		// Either the best score is only plausible, or two candidates are too
		// close to separate. Both are questions for a person.
		result.Status = StatusAmbiguous
		result.Candidates = local
		return result, nil
	}

	// Stage 2 — the anonymous catalog. Reached only when this storage has
	// nothing resembling the text.
	result.Status = StatusNewItem

	catalog, err := s.store.SimilarCatalogProduct(ctx, query, AmbiguousThreshold)
	if err != nil {
		return Result{}, fmt.Errorf("matching: catalog: %w", err)
	}
	if catalog == nil {
		// Stage 3 is this: no pre-filled data. The caller decides whether the
		// line is worth an external image search; this package does not make
		// that call itself.
		return result, nil
	}

	// Stage 2b — the variants around the hit.
	variants, err := s.store.CatalogVariantsOf(ctx, catalog.ID, query, MaxVariants)
	if err != nil {
		return Result{}, fmt.Errorf("matching: catalog variants: %w", err)
	}
	catalog.Variants = variants
	result.Catalog = catalog

	return result, nil
}
