package store_test

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CDRO/Inventory/internal/auth"
	"github.com/CDRO/Inventory/internal/store"
)

// TestEnsureInitialAdminAgainstARealDatabase exercises the bootstrap end to
// end, on a database of its own.
//
// The unit tests for EnsureInitialAdmin use a fake, which means the query that
// actually decides whether a fresh install gets an admin — CountUsers — was
// never run against PostgreSQL. That gap matters more than most: if the count
// is wrong the install either locks itself out permanently or grows an admin
// it should not have, and neither shows up until someone tries to log in.
//
// It needs an empty users table, so it cannot share the package database that
// every other test is inserting into. It creates and drops its own.
func TestEnsureInitialAdminAgainstARealDatabase(t *testing.T) {
	adminDSN := os.Getenv("DATABASE_URL")
	if adminDSN == "" {
		t.Skip("DATABASE_URL not set; run via `docker compose run --rm app go test ./...`")
	}

	ctx := context.Background()
	dbName := "inventory_bootstrap_" + randomSuffix()

	cleanup, dsn, err := createTestDatabase(adminDSN, dbName)
	require.NoError(t, err)
	t.Cleanup(cleanup)
	require.NoError(t, migrateTestDatabase(dsn))

	fresh, err := store.Open(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(fresh.Close)

	// A migrated database has no users at all.
	count, err := fresh.CountUsers(ctx)
	require.NoError(t, err)
	require.Equal(t, 0, count, "a freshly migrated database must start with no accounts")

	created, err := auth.EnsureInitialAdmin(ctx, fresh, "admin", "first-boot-secret")
	require.NoError(t, err)
	assert.True(t, created)

	count, err = fresh.CountUsers(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, count)

	// The account is usable and is an admin.
	admin, err := fresh.UserByUsername(ctx, "admin")
	require.NoError(t, err)
	assert.True(t, admin.IsAdmin, "the bootstrap account is the only way in; it must be an admin")
	assert.Equal(t, "admin", admin.DisplayName)

	ok, err := auth.VerifyPassword(admin.PasswordHash, "first-boot-secret")
	require.NoError(t, err)
	assert.True(t, ok, "the configured password must actually log in")

	isAdmin, err := fresh.IsAdmin(ctx, admin.ID)
	require.NoError(t, err)
	assert.True(t, isAdmin)

	// Running again is a no-op, and does not disturb the existing account.
	created, err = auth.EnsureInitialAdmin(ctx, fresh, "admin", "a-different-password")
	require.NoError(t, err)
	assert.False(t, created, "the bootstrap must not fire once the system has users")

	count, err = fresh.CountUsers(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, count)

	unchanged, err := fresh.UserByUsername(ctx, "admin")
	require.NoError(t, err)
	assert.Equal(t, admin.PasswordHash, unchanged.PasswordHash,
		"a second run must not silently reset the password to whatever is in the environment")
}

// TestBootstrapDoesNotFireForANonAdminOnlyInstall is the backdoor test.
//
// The gate is "no users", not "no admin". An install whose only admin was
// deliberately demoted or deleted must not have one reappear on the next
// restart keyed to whatever ADMIN_INITIAL_* happens to be set to — that would
// be a permanent way back in for anyone who can read the environment.
func TestBootstrapDoesNotFireForANonAdminOnlyInstall(t *testing.T) {
	adminDSN := os.Getenv("DATABASE_URL")
	if adminDSN == "" {
		t.Skip("DATABASE_URL not set; run via `docker compose run --rm app go test ./...`")
	}

	ctx := context.Background()
	dbName := "inventory_nobackdoor_" + randomSuffix()

	cleanup, dsn, err := createTestDatabase(adminDSN, dbName)
	require.NoError(t, err)
	t.Cleanup(cleanup)
	require.NoError(t, migrateTestDatabase(dsn))

	fresh, err := store.Open(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(fresh.Close)

	// One ordinary, non-admin account exists. Nobody can reach the admin area.
	_, err = fresh.CreateUser(ctx, store.NewUser{
		Username: "ordinary", PasswordHash: "x", DisplayName: "Ordinary", IsAdmin: false,
	})
	require.NoError(t, err)

	created, err := auth.EnsureInitialAdmin(ctx, fresh, "admin", "would-be-backdoor")
	require.NoError(t, err)

	assert.False(t, created, "an install with users must never grow an admin from the environment")
	count, err := fresh.CountUsers(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, count)
	assert.Equal(t, 0, countRowsIn(t, ctx, dsn, `SELECT count(*) FROM users WHERE is_admin`))
}

// TestUserLookupsAgainstTheDatabase covers the read paths that back a login
// attempt and a session lookup.
func TestUserLookupsAgainstTheDatabase(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()

	username := "lookup-" + randomSuffix()
	created, err := s.CreateUser(ctx, store.NewUser{
		Username: username, PasswordHash: "hash", DisplayName: "Lookup Target", IsAdmin: true,
	})
	require.NoError(t, err)

	t.Run("by username", func(t *testing.T) {
		got, err := s.UserByUsername(ctx, username)
		require.NoError(t, err)
		assert.Equal(t, created.ID, got.ID)
		assert.Equal(t, "hash", got.PasswordHash, "the hash must come back for verification")
		assert.True(t, got.IsAdmin, "is_admin round-trips through the row")
	})

	t.Run("by id", func(t *testing.T) {
		got, err := s.UserByID(ctx, created.ID)
		require.NoError(t, err)
		assert.Equal(t, username, got.Username)
	})

	t.Run("unknown username is ErrNotFound", func(t *testing.T) {
		_, err := s.UserByUsername(ctx, "no-such-user-"+randomSuffix())
		assert.ErrorIs(t, err, store.ErrNotFound,
			"a missing account must be distinguishable from a database failure")
	})

	t.Run("unknown id is ErrNotFound", func(t *testing.T) {
		_, err := s.UserByID(ctx, newUUID(t))
		assert.ErrorIs(t, err, store.ErrNotFound)
	})
}

// TestCreateUserDefaultsToNonAdmin — an account created without saying so must
// not arrive with admin rights.
func TestCreateUserDefaultsToNonAdmin(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()

	created, err := s.CreateUser(ctx, store.NewUser{
		Username: "plain-" + randomSuffix(), PasswordHash: "x", DisplayName: "Plain",
	})
	require.NoError(t, err)

	assert.False(t, created.IsAdmin)

	isAdmin, err := s.IsAdmin(ctx, created.ID)
	require.NoError(t, err)
	assert.False(t, isAdmin)
}

func TestListUsersIncludesCreatedAccounts(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()

	username := "listed-" + randomSuffix()
	created, err := s.CreateUser(ctx, store.NewUser{
		Username: username, PasswordHash: "x", DisplayName: "Listed",
	})
	require.NoError(t, err)

	users, err := s.ListUsers(ctx)
	require.NoError(t, err)

	var found bool
	for _, u := range users {
		if u.ID == created.ID {
			found = true
			assert.Equal(t, username, u.Username)
		}
	}
	assert.True(t, found, "a created account must appear in the admin list")
}

func TestCountUsersTracksInsertsAndDeletes(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()

	before, err := s.CountUsers(ctx)
	require.NoError(t, err)

	created, err := s.CreateUser(ctx, store.NewUser{
		Username: "counted-" + randomSuffix(), PasswordHash: "x", DisplayName: "Counted",
	})
	require.NoError(t, err)

	during, err := s.CountUsers(ctx)
	require.NoError(t, err)
	assert.Equal(t, before+1, during)

	require.NoError(t, s.DeleteUser(ctx, created.ID))

	after, err := s.CountUsers(ctx)
	require.NoError(t, err)
	assert.Equal(t, before, after)
}

func TestCreateAndListStorages(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()

	name := "Storage " + randomSuffix()
	created, err := s.CreateStorage(ctx, name)
	require.NoError(t, err)

	assert.Equal(t, name, created.Name)
	assert.NotEqual(t, "00000000-0000-0000-0000-000000000000", created.ID.String(),
		"the id is supplied by the application, not left to a default")
	assert.False(t, created.CreatedAt.IsZero())

	storages, err := s.ListStorages(ctx)
	require.NoError(t, err)

	var found bool
	for _, storage := range storages {
		if storage.ID == created.ID {
			found = true
			assert.Equal(t, name, storage.Name)
		}
	}
	assert.True(t, found, "a created storage must appear in the admin list")
}

func TestDeleteStorageReportsAnUnknownID(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()

	err := s.DeleteStorage(ctx, newUUID(t))

	assert.ErrorIs(t, err, store.ErrNotFound)
}
