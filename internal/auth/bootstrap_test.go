package auth_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CDRO/Inventory/internal/auth"
	"github.com/CDRO/Inventory/internal/store"
)

// fakeUsers is an in-memory stand-in, so the bootstrap rules can be exercised
// without a database.
type fakeUsers struct {
	count      int
	countErr   error
	created    []store.NewUser
	createErr  error
	byUsername map[string]*store.User
}

func (f *fakeUsers) CountUsers(context.Context) (int, error) {
	return f.count, f.countErr
}

func (f *fakeUsers) CreateUser(_ context.Context, in store.NewUser) (*store.User, error) {
	if f.createErr != nil {
		return nil, f.createErr
	}
	f.created = append(f.created, in)
	return &store.User{ID: uuid.New(), Username: in.Username, IsAdmin: in.IsAdmin}, nil
}

func (f *fakeUsers) UserByUsername(_ context.Context, username string) (*store.User, error) {
	if user, ok := f.byUsername[username]; ok {
		return user, nil
	}
	return nil, store.ErrNotFound
}

func TestEnsureInitialAdminCreatesOnEmptyTable(t *testing.T) {
	t.Parallel()

	users := &fakeUsers{count: 0}

	created, err := auth.EnsureInitialAdmin(context.Background(), users, "admin", "s3cret")

	require.NoError(t, err)
	assert.True(t, created)
	require.Len(t, users.created, 1)

	got := users.created[0]
	assert.Equal(t, "admin", got.Username)
	assert.True(t, got.IsAdmin, "the bootstrap account is the way in; it has to be an admin")
	assert.NotEmpty(t, got.PasswordHash)
	assert.NotContains(t, got.PasswordHash, "s3cret", "the password must be hashed, not stored")

	ok, err := auth.VerifyPassword(got.PasswordHash, "s3cret")
	require.NoError(t, err)
	assert.True(t, ok)
}

// TestEnsureInitialAdminIsNoOpWhenAnyUserExists is the rule that keeps this
// from being a permanent backdoor.
//
// The condition is "no users at all", not "no admin". An install whose only
// admin was deliberately demoted or deleted must not have one reappear on the
// next restart keyed to whatever ADMIN_INITIAL_* happens to be in the
// environment.
func TestEnsureInitialAdminIsNoOpWhenAnyUserExists(t *testing.T) {
	t.Parallel()

	for name, count := range map[string]int{
		"one ordinary user": 1,
		"a populated table": 42,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			users := &fakeUsers{count: count}

			created, err := auth.EnsureInitialAdmin(context.Background(), users, "admin", "s3cret")

			require.NoError(t, err)
			assert.False(t, created)
			assert.Empty(t, users.created, "no account may be created once the system has users")
		})
	}
}

func TestEnsureInitialAdminRejectsBlankCredentials(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct{ user, pass string }{
		"no username":    {"", "s3cret"},
		"blank username": {"   ", "s3cret"},
		"no password":    {"admin", ""},
		"neither":        {"", ""},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			users := &fakeUsers{count: 0}

			created, err := auth.EnsureInitialAdmin(context.Background(), users, tc.user, tc.pass)

			require.Error(t, err, "a blank credential must fail loudly, not create an account nobody can use")
			assert.False(t, created)
			assert.Empty(t, users.created)
		})
	}
}

func TestEnsureInitialAdminSurfacesStoreFailures(t *testing.T) {
	t.Parallel()

	boom := errors.New("connection refused")

	_, err := auth.EnsureInitialAdmin(context.Background(), &fakeUsers{countErr: boom}, "admin", "pw")
	require.ErrorIs(t, err, boom, "a database that cannot be counted must not look like a populated one")

	_, err = auth.EnsureInitialAdmin(context.Background(), &fakeUsers{count: 0, createErr: boom}, "admin", "pw")
	require.ErrorIs(t, err, boom)
}

func TestAuthenticateAcceptsCorrectPassword(t *testing.T) {
	t.Parallel()

	hash, err := auth.HashPassword("right")
	require.NoError(t, err)

	users := &fakeUsers{byUsername: map[string]*store.User{
		"tizian": {Username: "tizian", PasswordHash: hash},
	}}

	user, err := auth.Authenticate(context.Background(), users, "tizian", "right")

	require.NoError(t, err)
	require.NotNil(t, user)
	assert.Equal(t, "tizian", user.Username)
}

// TestAuthenticateGivesTheSameAnswerForBothFailures — an error that says which
// half was wrong tells an attacker whether an account exists, on a system with
// no public registration where the set of usernames is meant to be private.
func TestAuthenticateGivesTheSameAnswerForBothFailures(t *testing.T) {
	t.Parallel()

	hash, err := auth.HashPassword("right")
	require.NoError(t, err)

	users := &fakeUsers{byUsername: map[string]*store.User{
		"tizian": {Username: "tizian", PasswordHash: hash},
	}}

	_, wrongPassword := auth.Authenticate(context.Background(), users, "tizian", "wrong")
	_, unknownUser := auth.Authenticate(context.Background(), users, "nobody", "wrong")

	require.ErrorIs(t, wrongPassword, auth.ErrBadCredentials)
	require.ErrorIs(t, unknownUser, auth.ErrBadCredentials)
	assert.Equal(t, unknownUser.Error(), wrongPassword.Error(),
		"the two failures must be indistinguishable, message included")
}
