package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRecomputeAllProgressIsolatesAgainstAConcurrentWrite is the regression
// review-go asked for on #52 finding 2's fix: the earlier
// TestRecomputeSnapshotIsolationIsConsistent (store_test package) proved
// REPEATABLE READ semantics in the abstract, but never called
// RecomputeAllProgress or loadRecomputeSnapshot at all — reverting the
// actual fix would have left it green.
//
// This test uses recomputeSnapshotSync to land a real concurrent write
// exactly between loadRecomputeSnapshot's two reads, then calls
// RecomputeAllProgress itself and checks the write survived untouched.
// Before the fix (two independent pool queries), existingProgressPairs
// would see the freshly-inserted row, find no events for it in the map
// allScoringEvents already captured, and recomputeOnePair would reset its
// XP to zero. After the fix, the whole snapshot is one transaction started
// before the hook runs, so neither read can see the concurrent insert: the
// pair is absent from both, and the recompute never touches it.
//
// It needs its own throwaway database and a second raw connection, neither
// of which the store_test package's testStore/testPool are reachable to
// supply from here — package store test files cannot see package
// store_test symbols. So this bootstraps its own, the same shape
// internal/migrate/migrate_test.go's newTestDatabase uses for the same
// reason, in its own package.
func TestRecomputeAllProgressIsolatesAgainstAConcurrentWrite(t *testing.T) {
	adminDSN := os.Getenv("DATABASE_URL")
	if adminDSN == "" {
		t.Skip("DATABASE_URL not set; run via `docker compose run --rm app go test ./...`")
	}

	dbName := "inventory_test_race_" + raceTestSuffix(t)
	admin, err := sql.Open("pgx", adminDSN)
	require.NoError(t, err)
	defer func() { _ = admin.Close() }()
	_, err = admin.Exec(fmt.Sprintf(`CREATE DATABASE %q`, dbName))
	require.NoError(t, err)
	t.Cleanup(func() {
		drop, err := sql.Open("pgx", adminDSN)
		if err != nil {
			return
		}
		defer func() { _ = drop.Close() }()
		_, _ = drop.Exec(fmt.Sprintf(`DROP DATABASE IF EXISTS %q WITH (FORCE)`, dbName))
	})

	parsed, err := url.Parse(adminDSN)
	require.NoError(t, err)
	parsed.Path = "/" + dbName
	dsn := parsed.String()

	migrateDB, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	require.NoError(t, goose.SetDialect("postgres"))
	goose.SetLogger(goose.NopLogger())
	require.NoError(t, goose.Up(migrateDB, "../../migrations"))
	require.NoError(t, migrateDB.Close())

	ctx := context.Background()
	s, err := Open(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(s.Close)

	rawPool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(rawPool.Close)

	storageID, err := newID()
	require.NoError(t, err)
	_, err = rawPool.Exec(ctx, `INSERT INTO storages (id, name) VALUES ($1, 'Race')`, storageID)
	require.NoError(t, err)
	userID, err := newID()
	require.NoError(t, err)
	_, err = rawPool.Exec(ctx,
		`INSERT INTO users (id, username, password_hash, display_name) VALUES ($1, $2, 'x', 'Race User')`,
		userID, "race-"+raceTestSuffix(t))
	require.NoError(t, err)

	recomputeSnapshotSync = func() {
		_, err := rawPool.Exec(ctx,
			`INSERT INTO user_progress (storage_id, user_id, xp) VALUES ($1, $2, 999)`, storageID, userID)
		require.NoError(t, err)
	}
	t.Cleanup(func() { recomputeSnapshotSync = nil })

	_, err = s.RecomputeAllProgress(ctx)
	require.NoError(t, err)

	var xp int
	require.NoError(t, rawPool.QueryRow(ctx,
		`SELECT xp FROM user_progress WHERE storage_id = $1 AND user_id = $2`, storageID, userID).Scan(&xp))
	assert.Equal(t, 999, xp,
		"the concurrent write must survive untouched: the recompute's snapshot must not have seen this pair at all")
}

func raceTestSuffix(t *testing.T) string {
	t.Helper()
	buf := make([]byte, 8)
	_, err := rand.Read(buf)
	require.NoError(t, err)
	return hex.EncodeToString(buf)
}
