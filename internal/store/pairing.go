package store

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// PairingTTL is how long a QR pairing code stays redeemable. Short on purpose:
// the code appears on a screen, and the window only has to cover the walk from
// laptop to phone (docs/specs/12-client-api-contract.md).
const PairingTTL = 2 * time.Minute

// CreatePairingCode mints a single-use code for a user.
func (s *Store) CreatePairingCode(ctx context.Context, userID uuid.UUID) (string, error) {
	code, err := NewToken()
	if err != nil {
		return "", err
	}

	if _, err := s.pool.Exec(ctx, `
		INSERT INTO pairing_codes (code, user_id, expires_at)
		VALUES ($1, $2, now() + $3::interval)`,
		code, userID, PairingTTL.String()); err != nil {
		return "", fmt.Errorf("store: create pairing code: %w", err)
	}
	return code, nil
}

// RedeemPairingCode consumes a code and returns the user it belongs to.
//
// Redemption is exactly once. The UPDATE carries the whole condition — unused,
// unexpired — and its RETURNING tells us whether it matched, so two clients
// racing on the same code cannot both win: the second finds used_at already
// set and gets nothing.
//
// This is the one unauthenticated endpoint that mints a session, so it is the
// one worth guessing at. The lookup is by primary key, which means the
// database is doing an equality comparison on attacker-supplied input; the
// constant-time check below is what stops the *application* adding a timing
// signal on top of that, and the code is 256 CSPRNG bits so the search space
// is not attackable regardless.
func (s *Store) RedeemPairingCode(ctx context.Context, code string) (uuid.UUID, error) {
	if code == "" {
		return uuid.Nil, ErrNotFound
	}

	var userID uuid.UUID
	var stored string
	err := s.pool.QueryRow(ctx, `
		UPDATE pairing_codes
		   SET used_at = now()
		 WHERE code = $1
		   AND used_at IS NULL
		   AND expires_at > now()
		RETURNING code, user_id`, code).Scan(&stored, &userID)

	if errors.Is(err, pgx.ErrNoRows) {
		// Unknown, already used, or expired — all the same answer. Telling
		// them apart would confirm to a guesser that a code once existed.
		return uuid.Nil, ErrNotFound
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("store: redeem pairing code: %w", err)
	}

	if subtle.ConstantTimeCompare([]byte(stored), []byte(code)) != 1 {
		return uuid.Nil, ErrNotFound
	}
	return userID, nil
}

// SweepPairingCodes deletes used and expired rows.
func (s *Store) SweepPairingCodes(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM pairing_codes WHERE used_at IS NOT NULL OR expires_at <= now()`)
	if err != nil {
		return 0, fmt.Errorf("store: sweep pairing codes: %w", err)
	}
	return tag.RowsAffected(), nil
}
