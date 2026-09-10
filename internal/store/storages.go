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

	row := s.pool.QueryRow(ctx, `
		INSERT INTO storages (id, name) VALUES ($1, $2)
		RETURNING id, name, created_at`, id, name)

	return scanStorage(row)
}

// DeleteStorage removes a storage and everything scoped to it.
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
