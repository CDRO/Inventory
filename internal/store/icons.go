package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/CDRO/Inventory/internal/iconlib"
)

// Icon is a row of the global icon library (docs/specs/42-local-icon-library.md):
// either one of the vendored set imported at `migrate up`, or one a person
// uploaded for a case the vendored set has nothing close enough for.
//
// Global, not per-storage, the same deliberate exception catalog_products
// already is: an icon is not household data.
type Icon struct {
	ID        uuid.UUID
	Name      string
	SVGBody   string
	Source    string
	CreatedBy *uuid.UUID
	CreatedAt time.Time
}

// Icon sources (icons.source's CHECK constraint).
const (
	IconSourceVendored = "vendored"
	IconSourceUploaded = "uploaded"
)

// ImportIcons inserts one icons row per iconlib.Icon, `ON CONFLICT (name) DO
// NOTHING`, and returns how many rows were actually new.
//
// Safe to call more than once — the acceptance criterion `inventory icons
// import` itself rests on (docs/specs/42-local-icon-library.md): a second run
// against an already-populated table changes nothing and does not error.
//
// One statement per icon rather than a single multi-row INSERT or a
// pgx.Batch: this runs once, at `migrate up`, against a few thousand rows —
// not a request path — and a plain loop is the shape every other write in
// this package already uses.
func (s *Store) ImportIcons(ctx context.Context, icons []iconlib.Icon) (int, error) {
	inserted := 0
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		for _, icon := range icons {
			id, err := newID()
			if err != nil {
				return err
			}
			tag, err := tx.Exec(ctx, `
				INSERT INTO icons (id, name, svg_body, source)
				VALUES ($1, $2, $3, $4)
				ON CONFLICT (name) DO NOTHING`,
				id, icon.Name, icon.SVGBody, IconSourceVendored)
			if err != nil {
				return fmt.Errorf("store: import icon %q: %w", icon.Name, err)
			}
			inserted += int(tag.RowsAffected())
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return inserted, nil
}

// NewIcon is the input to CreateIcon.
type NewIcon struct {
	Name      string
	SVGBody   string
	CreatedBy uuid.UUID
}

// CreateIcon stores a person-uploaded icon
// (docs/specs/42-local-icon-library.md, "Uploading a custom icon").
//
// A name collision — with a vendored icon or with another upload — is
// ErrValidation, which the API answers 422: nobody can shadow noto:cheese-
// wedge, and the uploader picks a different name instead of being told an
// id they cannot see is already using theirs.
func (s *Store) CreateIcon(ctx context.Context, in NewIcon) (*Icon, error) {
	id, err := newID()
	if err != nil {
		return nil, err
	}

	row := s.pool.QueryRow(ctx, `
		INSERT INTO icons (id, name, svg_body, source, created_by)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id, name, svg_body, source, created_by, created_at`,
		id, in.Name, in.SVGBody, IconSourceUploaded, in.CreatedBy)

	icon, err := scanIcon(row)
	if isUniqueViolation(err) {
		return nil, fmt.Errorf("%w: an icon named %q already exists", ErrValidation, in.Name)
	}
	if err != nil {
		return nil, fmt.Errorf("store: create icon: %w", err)
	}
	return icon, nil
}

// IconSVG loads one icon's SVG body, for GET
// .../icons/{id}/svg (docs/specs/42-local-icon-library.md).
//
// Icons are global and shared by every storage, so unlike a product picture
// this performs no storage-ownership check of its own — the caller has
// already been confirmed a member of *some* storage by RequireStorageMember,
// which is the whole access rule this shared, non-sensitive catalogue needs.
func (s *Store) IconSVG(ctx context.Context, id uuid.UUID) (string, error) {
	var body string
	err := s.pool.QueryRow(ctx, `SELECT svg_body FROM icons WHERE id = $1`, id).Scan(&body)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrNotFound
		}
		return "", fmt.Errorf("store: load icon svg: %w", err)
	}
	return body, nil
}

func scanIcon(row pgx.Row) (*Icon, error) {
	var icon Icon
	if err := row.Scan(&icon.ID, &icon.Name, &icon.SVGBody, &icon.Source, &icon.CreatedBy, &icon.CreatedAt); err != nil {
		return nil, err
	}
	return &icon, nil
}
