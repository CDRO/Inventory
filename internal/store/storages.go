package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Storage is a tenancy boundary. Its existence is itself confidential: a
// non-admin must have no way to learn that any storage they are not a member
// of exists — not its name, not its id, not the total number
// (docs/specs/03-auth-and-multi-tenancy.md).
type Storage struct {
	ID        uuid.UUID
	Name      string
	CreatedAt time.Time
}

// Member is a user's membership of one storage.
//
// There is no role. Every member of a storage can read and write everything
// belonging to it; the only distinction in the system is admin, which is a
// property of the user and grants nothing inside a storage on its own.
type Member struct {
	UserID      uuid.UUID
	Username    string
	DisplayName string
	AddedAt     time.Time
}

// CreateStorage adds a storage. Only an admin reaches this.
func (s *Store) CreateStorage(ctx context.Context, name string) (*Storage, error) {
	id, err := newID()
	if err != nil {
		return nil, err
	}

	var out *Storage
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			INSERT INTO storages (id, name) VALUES ($1, $2)
			RETURNING id, name, created_at`, id, name)

		storage, err := scanStorage(row)
		if err != nil {
			return err
		}
		if err := seedStarterCategories(ctx, tx, storage.ID); err != nil {
			return err
		}
		out = storage
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// starterCategories is the tree a new storage begins with
// (docs/specs/08-expiration-and-classification.md).
//
// The spec calls these "a starting default the operator should review and
// adjust in-app — they are not a hard requirement". They exist so that the
// very first batch somebody adds gets a sensible expiry date instead of
// falling all the way through to the item-type fallback, and so that the
// category tree is something to edit rather than something to invent.
//
// A NULL shelf life means "inherit", not "no expiry": Household and
// Collectibles leave the decision to whatever a user files underneath them.
var starterCategories = []struct {
	name   string
	parent string // empty for a root
	days   *int
}{
	{name: "Food", days: intPtr(365)},
	{name: "Dairy", parent: "Food", days: intPtr(10)},
	{name: "Produce", parent: "Food", days: intPtr(7)},
	{name: "Meat", parent: "Food", days: intPtr(4)},
	{name: "Canned", parent: "Food", days: intPtr(730)},
	{name: "Household", days: nil},
	{name: "Collectibles", days: nil},
}

func intPtr(n int) *int { return &n }

// seedStarterCategories writes the starter tree for a newly created storage.
//
// It runs in the same transaction as the storage insert, so a storage never
// exists without its categories — a half-seeded storage would present the user
// with a partial tree and no way to tell it apart from one they had edited
// themselves.
//
// The spec describes this as something "the initial migration creates ... per
// new storage", which a migration cannot do: storages are created at runtime,
// long after migrations have run. Creation is the only place it can live.
func seedStarterCategories(ctx context.Context, tx pgx.Tx, storageID uuid.UUID) error {
	ids := make(map[string]uuid.UUID, len(starterCategories))

	for _, category := range starterCategories {
		id, err := newID()
		if err != nil {
			return err
		}

		var parentID *uuid.UUID
		if category.parent != "" {
			parent, ok := ids[category.parent]
			if !ok {
				// The table above is ordered parents-first; a miss means it
				// was edited into an order that cannot be built.
				return fmt.Errorf("store: starter category %q names unknown parent %q",
					category.name, category.parent)
			}
			parentID = &parent
		}

		if _, err := tx.Exec(ctx, `
			INSERT INTO categories (id, storage_id, parent_id, name, default_shelf_life_days)
			VALUES ($1, $2, $3, $4, $5)`,
			id, storageID, parentID, category.name, category.days); err != nil {
			return fmt.Errorf("store: seed starter category %q: %w", category.name, err)
		}
		ids[category.name] = id
	}
	return nil
}

// DeleteStorage removes a storage and everything scoped to it.
//
// Its memberships, locations, categories, products and jobs go with it: every
// one of those tables carries storage_id ... ON DELETE CASCADE
// (migrations/00002_core_schema.sql), so a membership row can never outlive
// the thing it grants access to.
func (s *Store) DeleteStorage(ctx context.Context, id uuid.UUID) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM storages WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("store: delete storage: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// IsStorageMember reports whether a user may access a storage.
//
// This is the data-layer half of the tenancy boundary. It answers one
// question — is there a membership row — and deliberately cannot distinguish
// "the storage does not exist" from "it exists and you are not in it". Both
// are false.
//
// That collapse is the whole point. The two cases must be indistinguishable to
// a caller, because a response that separates them confirms an id names a real
// storage and lets someone probe for the existence of households they cannot
// see. Not representing the difference here means no caller can leak it. The
// HTTP half — answering 404 rather than 403, with a byte-identical body — is
// the middleware's, in docs/specs/04-backend-api-conventions.md.
func (s *Store) IsStorageMember(ctx context.Context, storageID, userID uuid.UUID) (bool, error) {
	var member bool
	err := s.pool.QueryRow(ctx, `
		SELECT EXISTS (
		    SELECT 1 FROM storage_members WHERE storage_id = $1 AND user_id = $2
		)`, storageID, userID).Scan(&member)
	if err != nil {
		return false, fmt.Errorf("store: check storage membership: %w", err)
	}
	return member, nil
}

// AddMember grants a user access to a storage. Re-adding an existing member is
// not an error: the end state the admin asked for is already true.
func (s *Store) AddMember(ctx context.Context, storageID, userID uuid.UUID) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO storage_members (storage_id, user_id) VALUES ($1, $2)
		ON CONFLICT (storage_id, user_id) DO NOTHING`, storageID, userID)
	if err != nil {
		return fmt.Errorf("store: add storage member: %w", err)
	}
	return nil
}

// RemoveMember revokes a user's access to a storage.
//
// Their sessions are untouched: membership is not a credential, and the user
// may well belong to other storages. The next request that names this storage
// simply stops finding a membership row.
func (s *Store) RemoveMember(ctx context.Context, storageID, userID uuid.UUID) error {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM storage_members WHERE storage_id = $1 AND user_id = $2`, storageID, userID)
	if err != nil {
		return fmt.Errorf("store: remove storage member: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ListMembers returns a storage's members, for the admin view.
func (s *Store) ListMembers(ctx context.Context, storageID uuid.UUID) ([]Member, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT u.id, u.username, u.display_name, m.added_at
		  FROM storage_members m
		  JOIN users u ON u.id = m.user_id
		 WHERE m.storage_id = $1
		 ORDER BY m.added_at, u.id`, storageID)
	if err != nil {
		return nil, fmt.Errorf("store: list storage members: %w", err)
	}
	defer rows.Close()

	var out []Member
	for rows.Next() {
		var m Member
		if err := rows.Scan(&m.UserID, &m.Username, &m.DisplayName, &m.AddedAt); err != nil {
			return nil, fmt.Errorf("store: scan storage member: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list storage members: %w", err)
	}
	return out, nil
}

// StoragesForUser returns exactly the storages a user belongs to.
//
// This is what GET /api/auth/me is built from, and it is filtered by
// membership rather than listing storages and marking which are accessible.
// An empty result is a normal state — a user an admin has not yet added
// anywhere — and must not be presented as an error or as evidence that
// storages exist which they cannot see.
//
// Being an admin adds nothing here: admin rights are orthogonal to membership,
// so an admin who wants to use a storage must be added to it like anyone else.
func (s *Store) StoragesForUser(ctx context.Context, userID uuid.UUID) ([]Storage, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT s.id, s.name, s.created_at
		  FROM storages s
		  JOIN storage_members m ON m.storage_id = s.id
		 WHERE m.user_id = $1
		 ORDER BY s.created_at, s.id`, userID)
	if err != nil {
		return nil, fmt.Errorf("store: list storages for user: %w", err)
	}
	defer rows.Close()

	var out []Storage
	for rows.Next() {
		storage, err := scanStorage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *storage)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list storages for user: %w", err)
	}
	return out, nil
}

// ListStorages returns every storage. Admin-only: no non-admin path may reach
// this, because a global list is exactly what the non-enumeration rule forbids.
func (s *Store) ListStorages(ctx context.Context) ([]Storage, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, name, created_at FROM storages ORDER BY created_at, id`)
	if err != nil {
		return nil, fmt.Errorf("store: list storages: %w", err)
	}
	defer rows.Close()

	var out []Storage
	for rows.Next() {
		storage, err := scanStorage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *storage)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list storages: %w", err)
	}
	return out, nil
}

func scanStorage(row rowScanner) (*Storage, error) {
	var s Storage
	err := row.Scan(&s.ID, &s.Name, &s.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: scan storage: %w", err)
	}
	return &s, nil
}
