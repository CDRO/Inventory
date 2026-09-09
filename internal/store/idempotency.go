package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// IdempotencyRetention is how long a recorded response stays replayable. Long
// enough to cover a phone that was offline overnight, short enough that the
// table does not grow forever (docs/specs/12-client-api-contract.md).
const IdempotencyRetention = 7 * 24 * time.Hour

// IdempotentResponse is a previously recorded reply to the same request.
type IdempotentResponse struct {
	Status int
	Body   []byte
}

// HashRequest builds the fingerprint stored alongside an idempotency key.
//
// It covers method, path and body together: a client that reuses a key for a
// genuinely different request is a bug, and the hash is what makes that
// distinguishable from an honest retry.
func HashRequest(method, path string, body []byte) string {
	sum := sha256.New()
	sum.Write([]byte(method))
	sum.Write([]byte{0})
	sum.Write([]byte(path))
	sum.Write([]byte{0})
	sum.Write(body)
	return hex.EncodeToString(sum.Sum(nil))
}

// LookupIdempotent returns a recorded response for this key, if any.
//
// A hit whose request_hash differs from the current request is not a replay —
// it is the same key being used for a different call. That returns
// ErrValidation so the caller can answer 422, rather than replaying a response
// that belongs to some other request entirely.
//
// The key is scoped by user, so one client's key can never collide with
// another's.
func (s *Store) LookupIdempotent(ctx context.Context, key string, userID uuid.UUID, requestHash string) (*IdempotentResponse, error) {
	var status int
	var body []byte
	var storedHash string

	err := s.pool.QueryRow(ctx, `
		SELECT request_hash, response_status, response_body
		  FROM idempotency_records
		 WHERE key = $1 AND user_id = $2`, key, userID).Scan(&storedHash, &status, &body)

	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: lookup idempotency record: %w", err)
	}

	if storedHash != requestHash {
		return nil, fmt.Errorf("%w: idempotency key reused for a different request", ErrValidation)
	}
	return &IdempotentResponse{Status: status, Body: body}, nil
}

// RecordIdempotent stores the response for a key so a retry replays it.
//
// Concurrent first attempts are resolved by the primary key: the loser's
// insert is ignored and the winner's response is the one that will be
// replayed, which is the behaviour a client retrying into a race needs.
func (s *Store) RecordIdempotent(ctx context.Context, key string, userID uuid.UUID, storageID *uuid.UUID, requestHash string, status int, body []byte) error {
	if key == "" {
		return fmt.Errorf("%w: idempotency key must not be empty", ErrValidation)
	}

	if _, err := s.pool.Exec(ctx, `
		INSERT INTO idempotency_records (key, user_id, storage_id, request_hash, response_status, response_body)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (key, user_id) DO NOTHING`,
		key, userID, storageID, requestHash, status, body); err != nil {
		return fmt.Errorf("store: record idempotency: %w", err)
	}
	return nil
}

// SweepIdempotencyRecords deletes rows past the retention window.
func (s *Store) SweepIdempotencyRecords(ctx context.Context, now time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM idempotency_records WHERE created_at < $1`, now.Add(-IdempotencyRetention))
	if err != nil {
		return 0, fmt.Errorf("store: sweep idempotency records: %w", err)
	}
	return tag.RowsAffected(), nil
}
