package store

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// SessionKind separates a browser login from a paired native client.
//
// A device gets its own session row rather than sharing the browser's, which
// is what makes "revoke my phone" possible without signing the user out of
// their laptop (docs/specs/12-client-api-contract.md).
type SessionKind string

const (
	SessionBrowser SessionKind = "browser"
	SessionDevice  SessionKind = "device"
)

// tokenBytes is 256 bits, per docs/specs/02-data-model.md. A session id is a
// bearer credential, so it is CSPRNG output rather than a UUIDv7 — a v7 embeds
// a timestamp and is partially predictable, which is a bad property for a
// secret.
const tokenBytes = 32

// lastSeenThrottle bounds how often last_seen_at is rewritten. The column
// exists to show a useful "last active" in the device list; without a throttle
// it would mean a database write on every single request.
const lastSeenThrottle = time.Hour

// Session is a server-side session. The client holds only the id.
type Session struct {
	ID         string
	UserID     uuid.UUID
	Kind       SessionKind
	Label      *string
	LastSeenAt *time.Time
	CreatedAt  time.Time
	ExpiresAt  time.Time
}

// NewToken returns a fresh opaque session id: 256 CSPRNG bits, base64url,
// unpadded.
func NewToken() (string, error) {
	buf := make([]byte, tokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("store: generate token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// CreateSession issues a session for a user.
func (s *Store) CreateSession(ctx context.Context, userID uuid.UUID, kind SessionKind, label *string, ttl time.Duration) (*Session, error) {
	token, err := NewToken()
	if err != nil {
		return nil, err
	}
	if kind == "" {
		kind = SessionBrowser
	}

	row := s.pool.QueryRow(ctx, `
		INSERT INTO sessions (id, user_id, kind, label, expires_at)
		VALUES ($1, $2, $3, $4, now() + $5::interval)
		RETURNING id, user_id, kind, label, last_seen_at, created_at, expires_at`,
		token, userID, string(kind), label, ttl.String())

	return scanSession(row)
}

// LookupSession returns a live session, deleting it if it has expired.
//
// Expired rows are cleared lazily on lookup as well as by a periodic sweep, so
// a credential stops working the moment it lapses rather than whenever the
// sweep next happens to run.
func (s *Store) LookupSession(ctx context.Context, id string) (*Session, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT id, user_id, kind, label, last_seen_at, created_at, expires_at
		  FROM sessions WHERE id = $1`, id)

	session, err := scanSession(row)
	if err != nil {
		return nil, err
	}

	if !session.ExpiresAt.After(time.Now()) {
		if _, err := s.pool.Exec(ctx, `DELETE FROM sessions WHERE id = $1`, id); err != nil {
			return nil, fmt.Errorf("store: delete expired session: %w", err)
		}
		return nil, ErrNotFound
	}
	return session, nil
}

// TouchSession records activity, at most once per hour per session.
//
// The throttle is in the WHERE clause rather than in Go so that concurrent
// requests cannot race into two writes: the database decides whether the
// stored value is already recent enough.
func (s *Store) TouchSession(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE sessions
		   SET last_seen_at = now()
		 WHERE id = $1
		   AND (last_seen_at IS NULL OR last_seen_at < now() - $2::interval)`,
		id, lastSeenThrottle.String())
	if err != nil {
		return fmt.Errorf("store: touch session: %w", err)
	}
	return nil
}

// DeleteSession revokes one session immediately.
func (s *Store) DeleteSession(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM sessions WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("store: delete session: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// UserSessions lists a user's live sessions, for the device list they revoke
// from.
func (s *Store) UserSessions(ctx context.Context, userID uuid.UUID) ([]Session, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, user_id, kind, label, last_seen_at, created_at, expires_at
		  FROM sessions
		 WHERE user_id = $1 AND expires_at > now()
		 ORDER BY created_at`, userID)
	if err != nil {
		return nil, fmt.Errorf("store: list sessions: %w", err)
	}
	defer rows.Close()

	var out []Session
	for rows.Next() {
		session, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *session)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list sessions: %w", err)
	}
	return out, nil
}

// SweepSessions deletes expired rows.
func (s *Store) SweepSessions(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM sessions WHERE expires_at <= now()`)
	if err != nil {
		return 0, fmt.Errorf("store: sweep sessions: %w", err)
	}
	return tag.RowsAffected(), nil
}

func scanSession(row rowScanner) (*Session, error) {
	var s Session
	var kind string
	err := row.Scan(&s.ID, &s.UserID, &kind, &s.Label, &s.LastSeenAt, &s.CreatedAt, &s.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: scan session: %w", err)
	}
	s.Kind = SessionKind(kind)
	return &s, nil
}
