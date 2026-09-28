package store

import (
	"context"
	"fmt"

	"github.com/google/uuid"
)

// IconSuggestion is one result of a local icon search
// (docs/specs/40-icon-picker.md): enough to render the actual icon and, for
// an alias hit, why it matched.
//
// SVGBody is populated for a vendored icon, and safe to inline directly —
// the vendored set is trusted content (docs/specs/42-local-icon-library.md,
// this project's trusted-external-provider posture). An uploaded icon is
// person-supplied and untrusted, so it carries IconID instead: the caller
// renders it through GET .../icons/{id}/svg via an <img> element, the same
// rendering discipline icons.go's ServeSVG already documents, never injected
// inline.
type IconSuggestion struct {
	IconName     string
	Source       string
	SVGBody      string
	IconID       uuid.UUID
	MatchedAlias string // empty for a direct icons.name hit
}

// iconSearchMinSimilarity is deliberately far below matching.AmbiguousThreshold
// (internal/matching): that threshold decides whether to auto-accept a single
// best guess, while this backs an incremental search box a person is reading
// results from as they type — a browsing search wants a wide net, not a
// confidence gate.
const iconSearchMinSimilarity = 0.15

// SearchIconAliases returns icon_aliases rows whose alias is trigram-similar
// to query, one row per icon_name (its best-matching alias), ranked by that
// similarity, joined to the icons table for the actual icon to render.
//
// icon_name is not a foreign key to icons.name (docs/specs/40-icon-picker.md,
// by design), so the join excludes an alias left behind by a renamed or
// removed icon rather than surfacing a result with nothing to render.
func (s *Store) SearchIconAliases(ctx context.Context, query string, limit int) ([]IconSuggestion, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT icon_name, alias, id, svg_body, source
		  FROM (
			SELECT DISTINCT ON (a.icon_name)
			       a.icon_name, a.alias, i.id, i.svg_body, i.source,
			       similarity(a.alias, $1) AS sim
			  FROM icon_aliases a
			  JOIN icons i ON i.name = a.icon_name
			 WHERE similarity(a.alias, $1) >= $2
			 ORDER BY a.icon_name, sim DESC, a.alias
		  ) dedup
		 ORDER BY sim DESC, icon_name
		 LIMIT $3`, query, iconSearchMinSimilarity, limit)
	if err != nil {
		return nil, fmt.Errorf("store: search icon aliases: %w", err)
	}
	defer rows.Close()

	var out []IconSuggestion
	for rows.Next() {
		var hit IconSuggestion
		if err := rows.Scan(&hit.IconName, &hit.MatchedAlias, &hit.IconID, &hit.SVGBody, &hit.Source); err != nil {
			return nil, fmt.Errorf("store: scan icon alias hit: %w", err)
		}
		out = append(out, hit)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: search icon aliases: %w", err)
	}
	return out, nil
}

// SearchIconsByName returns icons rows whose name is trigram-similar to
// query, ranked by that similarity — the second tier of
// docs/specs/40-icon-picker.md's search, a direct-name hit rather than an
// alias hit.
func (s *Store) SearchIconsByName(ctx context.Context, query string, limit int) ([]IconSuggestion, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, name, svg_body, source
		  FROM icons
		 WHERE similarity(name, $1) >= $2
		 ORDER BY similarity(name, $1) DESC, name
		 LIMIT $3`, query, iconSearchMinSimilarity, limit)
	if err != nil {
		return nil, fmt.Errorf("store: search icons by name: %w", err)
	}
	defer rows.Close()

	var out []IconSuggestion
	for rows.Next() {
		var hit IconSuggestion
		if err := rows.Scan(&hit.IconID, &hit.IconName, &hit.SVGBody, &hit.Source); err != nil {
			return nil, fmt.Errorf("store: scan icon name hit: %w", err)
		}
		out = append(out, hit)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: search icons by name: %w", err)
	}
	return out, nil
}

// CreateIconAlias records that iconName was found by searching alias,
// `ON CONFLICT (icon_name, alias) DO NOTHING` — the same insert-and-ignore
// shape catalog_products already uses. Insert-only, like catalog_products:
// there is no edit or delete (docs/specs/40-icon-picker.md), so a wrong
// alias is harmless clutter, not a correctness bug.
func (s *Store) CreateIconAlias(ctx context.Context, iconName, alias string, createdBy uuid.UUID) error {
	id, err := newID()
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO icon_aliases (id, icon_name, alias, created_by)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (icon_name, alias) DO NOTHING`,
		id, iconName, alias, createdBy)
	if err != nil {
		return fmt.Errorf("store: create icon alias: %w", err)
	}
	return nil
}
