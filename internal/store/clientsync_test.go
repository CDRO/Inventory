package store_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CDRO/Inventory/internal/store"
)

// TestPairingCodeIsRedeemableExactlyOnce is the rule that matters most on this
// table: the code is the one unauthenticated credential that mints a session,
// so a second redemption must fail even inside the TTL.
func TestPairingCodeIsRedeemableExactlyOnce(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	userID := newUser(t, ctx)

	code, err := s.CreatePairingCode(ctx, userID)
	require.NoError(t, err)

	got, err := s.RedeemPairingCode(ctx, code)
	require.NoError(t, err)
	assert.Equal(t, userID, got)

	_, err = s.RedeemPairingCode(ctx, code)
	require.ErrorIs(t, err, store.ErrNotFound, "a code must not be redeemable twice")

	var usedAt *time.Time
	require.NoError(t, testPool.QueryRow(ctx,
		`SELECT used_at FROM pairing_codes WHERE code = $1`, code).Scan(&usedAt))
	assert.NotNil(t, usedAt, "redemption records when the code was consumed")
}

// TestPairingCodeRaceYieldsOneWinner — two clients scanning the same QR at
// once must not both get a session. The single-use rule has to hold under
// concurrency, not just in sequence.
func TestPairingCodeRaceYieldsOneWinner(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	userID := newUser(t, ctx)

	code, err := s.CreatePairingCode(ctx, userID)
	require.NoError(t, err)

	const racers = 8
	var wg sync.WaitGroup
	results := make(chan error, racers)

	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.RedeemPairingCode(ctx, code)
			results <- err
		}()
	}
	wg.Wait()
	close(results)

	var wins, losses int
	for err := range results {
		if err == nil {
			wins++
		} else {
			require.ErrorIs(t, err, store.ErrNotFound)
			losses++
		}
	}

	assert.Equal(t, 1, wins, "exactly one racer may redeem the code")
	assert.Equal(t, racers-1, losses)
}

func TestExpiredPairingCodeIsNotRedeemable(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	userID := newUser(t, ctx)

	code, err := s.CreatePairingCode(ctx, userID)
	require.NoError(t, err)

	// Age it past its TTL rather than waiting two minutes.
	_, err = execTest(ctx,
		`UPDATE pairing_codes SET expires_at = now() - interval '1 second' WHERE code = $1`, code)
	require.NoError(t, err)

	_, err = s.RedeemPairingCode(ctx, code)
	require.ErrorIs(t, err, store.ErrNotFound)
}

func TestUnknownPairingCodeIsNotFound(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()

	_, err := s.RedeemPairingCode(ctx, "not-a-real-code")
	require.ErrorIs(t, err, store.ErrNotFound)

	_, err = s.RedeemPairingCode(ctx, "")
	require.ErrorIs(t, err, store.ErrNotFound)
}

// TestIdempotencyReplaysTheSameRequest covers the honest retry: a phone whose
// response was lost re-sends and must get the original answer, not a second
// debit.
func TestIdempotencyReplaysTheSameRequest(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	userID := newUser(t, ctx)

	key := "key-" + randomSuffix()
	hash := store.HashRequest("POST", "/api/storages/x/consume", []byte(`{"qty":1}`))

	missing, err := s.LookupIdempotent(ctx, key, userID, hash)
	require.NoError(t, err)
	assert.Nil(t, missing, "an unseen key is not a hit")

	require.NoError(t, s.RecordIdempotent(ctx, key, userID, nil, hash, 201, []byte(`{"ok":true}`)))

	replay, err := s.LookupIdempotent(ctx, key, userID, hash)
	require.NoError(t, err)
	require.NotNil(t, replay)
	assert.Equal(t, 201, replay.Status)
	assert.JSONEq(t, `{"ok":true}`, string(replay.Body))
}

// TestIdempotencyKeyReuseWithDifferentBodyIsRejected — replaying here would
// hand back a response belonging to an entirely different call, which is worse
// than the duplicate the key was meant to prevent.
func TestIdempotencyKeyReuseWithDifferentBodyIsRejected(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	userID := newUser(t, ctx)

	key := "key-" + randomSuffix()
	original := store.HashRequest("POST", "/api/storages/x/consume", []byte(`{"qty":1}`))
	different := store.HashRequest("POST", "/api/storages/x/consume", []byte(`{"qty":99}`))

	require.NoError(t, s.RecordIdempotent(ctx, key, userID, nil, original, 201, []byte(`{"ok":true}`)))

	_, err := s.LookupIdempotent(ctx, key, userID, different)

	require.ErrorIs(t, err, store.ErrValidation)
}

// TestIdempotencyKeysAreScopedPerUser — the primary key is (key, user_id)
// precisely so one client's "key-1" cannot collide with another's.
func TestIdempotencyKeysAreScopedPerUser(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	alice := newUser(t, ctx)
	bob := newUser(t, ctx)

	key := "shared-" + randomSuffix()
	hash := store.HashRequest("POST", "/api/x", []byte(`{}`))

	require.NoError(t, s.RecordIdempotent(ctx, key, alice, nil, hash, 201, []byte(`{"who":"alice"}`)))

	bobsView, err := s.LookupIdempotent(ctx, key, bob, hash)
	require.NoError(t, err)
	assert.Nil(t, bobsView, "Bob must not see Alice's recorded response")
}

func TestHashRequestSeparatesFields(t *testing.T) {
	t.Parallel()

	// Without the field separator, method+path+body could be re-partitioned to
	// produce the same digest for different requests.
	a := store.HashRequest("POST", "/a", []byte("bc"))
	b := store.HashRequest("POST", "/ab", []byte("c"))

	assert.NotEqual(t, a, b, "field boundaries must be part of the digest")
	assert.Equal(t, a, store.HashRequest("POST", "/a", []byte("bc")), "hashing is deterministic")
}

// TestSessionKindsAreIndependent is what makes "revoke my phone" work: the
// device session must be removable without touching the browser's.
func TestSessionKindsAreIndependent(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	userID := newUser(t, ctx)

	browser, err := s.CreateSession(ctx, userID, store.SessionBrowser, nil, time.Hour)
	require.NoError(t, err)
	phone, err := s.CreateSession(ctx, userID, store.SessionDevice, ptrString("Pixel 9"), 30*24*time.Hour)
	require.NoError(t, err)

	assert.Equal(t, store.SessionBrowser, browser.Kind)
	assert.Nil(t, browser.Label, "a browser session carries no device label")
	require.NotNil(t, phone.Label)
	assert.Equal(t, "Pixel 9", *phone.Label)

	sessions, err := s.UserSessions(ctx, userID)
	require.NoError(t, err)
	assert.Len(t, sessions, 2)

	require.NoError(t, s.DeleteSession(ctx, phone.ID))

	_, err = s.LookupSession(ctx, phone.ID)
	assert.ErrorIs(t, err, store.ErrNotFound, "the revoked device is gone")

	still, err := s.LookupSession(ctx, browser.ID)
	require.NoError(t, err, "revoking a phone must not sign the laptop out")
	assert.Equal(t, browser.ID, still.ID)
}

// TestSessionTokenIsOpaqueAndUnpredictable pins the one deliberate exception
// to UUIDv7: a session id is a bearer credential, so it must not embed a
// timestamp.
func TestSessionTokenIsOpaqueAndUnpredictable(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	userID := newUser(t, ctx)

	seen := map[string]bool{}
	for i := 0; i < 20; i++ {
		session, err := s.CreateSession(ctx, userID, store.SessionBrowser, nil, time.Hour)
		require.NoError(t, err)

		assert.GreaterOrEqual(t, len(session.ID), 43, "256 bits base64url-encodes to 43 characters")
		assert.NotContains(t, session.ID, "=", "the encoding is unpadded base64url")

		_, err = uuid.Parse(session.ID)
		assert.Error(t, err, "a session id must not be a UUID")

		require.False(t, seen[session.ID], "tokens must never repeat")
		seen[session.ID] = true
	}
}

// TestExpiredSessionIsClearedOnLookup — a credential has to stop working the
// moment it lapses, not whenever the sweep next runs.
func TestExpiredSessionIsClearedOnLookup(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	userID := newUser(t, ctx)

	session, err := s.CreateSession(ctx, userID, store.SessionBrowser, nil, time.Hour)
	require.NoError(t, err)

	_, err = execTest(ctx,
		`UPDATE sessions SET expires_at = now() - interval '1 second' WHERE id = $1`, session.ID)
	require.NoError(t, err)

	_, err = s.LookupSession(ctx, session.ID)

	require.ErrorIs(t, err, store.ErrNotFound)
	assert.Equal(t, 0, countRows(t, ctx, `SELECT count(*) FROM sessions WHERE id = $1`, session.ID),
		"the lapsed row is cleared on lookup, not left for the sweep")
}

// TestTouchSessionIsThrottled — last_seen_at exists for a "last active"
// display. Rewriting it on every request would be a database write per
// request for a column nobody reads that often.
func TestTouchSessionIsThrottled(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	userID := newUser(t, ctx)

	session, err := s.CreateSession(ctx, userID, store.SessionDevice, ptrString("Pixel 9"), time.Hour)
	require.NoError(t, err)

	require.NoError(t, s.TouchSession(ctx, session.ID))

	var first *time.Time
	require.NoError(t, testPool.QueryRow(ctx,
		`SELECT last_seen_at FROM sessions WHERE id = $1`, session.ID).Scan(&first))
	require.NotNil(t, first, "the first touch records activity")

	// Several more touches in quick succession must all be suppressed.
	for i := 0; i < 5; i++ {
		require.NoError(t, s.TouchSession(ctx, session.ID))
	}

	var second *time.Time
	require.NoError(t, testPool.QueryRow(ctx,
		`SELECT last_seen_at FROM sessions WHERE id = $1`, session.ID).Scan(&second))
	require.NotNil(t, second)
	assert.Equal(t, *first, *second, "a touch within the hour must not rewrite the row")

	// Age it past the throttle; the next touch is allowed through.
	_, err = execTest(ctx,
		`UPDATE sessions SET last_seen_at = now() - interval '2 hours' WHERE id = $1`, session.ID)
	require.NoError(t, err)

	require.NoError(t, s.TouchSession(ctx, session.ID))

	var third *time.Time
	require.NoError(t, testPool.QueryRow(ctx,
		`SELECT last_seen_at FROM sessions WHERE id = $1`, session.ID).Scan(&third))
	require.NotNil(t, third)
	assert.True(t, third.After(*second), "once the throttle window passes, activity is recorded again")
}

func TestSweepsRemoveOnlyDeadRows(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	userID := newUser(t, ctx)

	live, err := s.CreateSession(ctx, userID, store.SessionBrowser, nil, time.Hour)
	require.NoError(t, err)
	dead, err := s.CreateSession(ctx, userID, store.SessionBrowser, nil, time.Hour)
	require.NoError(t, err)
	_, err = execTest(ctx,
		`UPDATE sessions SET expires_at = now() - interval '1 second' WHERE id = $1`, dead.ID)
	require.NoError(t, err)

	removed, err := s.SweepSessions(ctx)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, removed, int64(1))

	assert.Equal(t, 1, countRows(t, ctx, `SELECT count(*) FROM sessions WHERE id = $1`, live.ID),
		"the live session survives the sweep")
	assert.Equal(t, 0, countRows(t, ctx, `SELECT count(*) FROM sessions WHERE id = $1`, dead.ID))
}
