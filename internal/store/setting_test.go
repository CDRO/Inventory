package store_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CDRO/Inventory/internal/store"
)

// TestSettingReturnsStoredValue and its siblings exercise Store.Setting end to
// end against a real database, closing the gap TestSettingErrorClassification
// (internal/store/store_test.go) leaves: that test pins the two predicates
// isNoRows/isUndefinedTable, but not that the switch arm built on them
// actually returns ("", false, nil) rather than, say, propagating the error.
// A regression there would make a fresh deployment report model_unavailable
// permanently, and nothing here would have caught it before this test.
func TestSettingReturnsStoredValue(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := requireDB(t)

	key := "test-setting-" + randomSuffix()
	_, err := execTest(ctx, `INSERT INTO settings (key, value) VALUES ($1, $2)`, key, "gemini-2.5-flash")
	require.NoError(t, err)

	value, ok, err := s.Setting(ctx, key)
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, "gemini-2.5-flash", value)
}

func TestSettingNoRowIsNoOverride(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := requireDB(t)

	value, ok, err := s.Setting(ctx, "nonexistent-key-"+randomSuffix())
	require.NoError(t, err)
	assert.False(t, ok)
	assert.Equal(t, "", value)
}

// TestSettingUndefinedTableIsNoOverride opens a database that has never been
// migrated — no settings table at all, matching a fresh deployment before the
// spec-02 migration runs — and checks Setting degrades to "no override"
// rather than failing. Deliberately does not use the package's testStore,
// which is always fully migrated; this needs its own throwaway, unmigrated
// database to reproduce SQLSTATE 42P01 for real.
func TestSettingUndefinedTableIsNoOverride(t *testing.T) {
	adminDSN := requireAdminDSN(t)

	cleanup, dsn, err := createTestDatabase(adminDSN, "inventory_test_unmigrated_"+randomSuffix())
	require.NoError(t, err)
	t.Cleanup(cleanup)

	ctx := context.Background()
	fresh, err := store.Open(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { fresh.Close() })

	value, ok, err := fresh.Setting(ctx, "gemini_model")
	require.NoError(t, err)
	assert.False(t, ok)
	assert.Equal(t, "", value)
}
