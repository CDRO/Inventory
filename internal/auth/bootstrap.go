package auth

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/CDRO/Inventory/internal/store"
)

// UserStore is the slice of the store this package needs, declared here so the
// bootstrap can be exercised without a database.
type UserStore interface {
	CountUsers(ctx context.Context) (int, error)
	CreateUser(ctx context.Context, in store.NewUser) (*store.User, error)
}

// EnsureInitialAdmin creates the first admin when the users table is empty.
//
// This is the only account-creation path that does not go through the admin
// view, and it exists so a fresh deployment has somewhere to log in — there is
// no public registration, so without it a new install would be locked out of
// itself (docs/specs/03-auth-and-multi-tenancy.md).
//
// It is a no-op once any user exists. The condition is deliberately "no users
// at all" rather than "no admin": an install whose only admin was demoted or
// deleted on purpose must not have one silently reappear on the next restart,
// which would be a permanent backdoor keyed to whatever is in the environment.
//
// Returns whether an account was created.
func EnsureInitialAdmin(ctx context.Context, users UserStore, username, password string) (bool, error) {
	username = strings.TrimSpace(username)
	if username == "" || password == "" {
		return false, fmt.Errorf("auth: initial admin needs both a username and a password")
	}

	count, err := users.CountUsers(ctx)
	if err != nil {
		return false, fmt.Errorf("auth: count users for bootstrap: %w", err)
	}
	if count > 0 {
		return false, nil
	}

	hash, err := HashPassword(password)
	if err != nil {
		return false, fmt.Errorf("auth: hash initial admin password: %w", err)
	}

	if _, err := users.CreateUser(ctx, store.NewUser{
		Username:     username,
		PasswordHash: hash,
		DisplayName:  username,
		IsAdmin:      true,
	}); err != nil {
		return false, fmt.Errorf("auth: create initial admin: %w", err)
	}
	return true, nil
}

// Authenticate verifies a username and password, returning the user on
// success.
//
// A missing user and a wrong password are the same failure, ErrBadCredentials,
// and both pay the cost of a hash verification: returning early for an unknown
// username makes login timing a username oracle, letting someone enumerate who
// has an account on the system.
func Authenticate(ctx context.Context, users LoginStore, username, password string) (*store.User, error) {
	user, err := users.UserByUsername(ctx, username)
	if err != nil {
		if isNotFound(err) {
			// Spend the work anyway, against a hash that cannot match, so the
			// unknown-username path costs what the known one costs.
			_, _ = VerifyPassword(decoyHash(), password)
			return nil, ErrBadCredentials
		}
		return nil, err
	}

	ok, err := VerifyPassword(user.PasswordHash, password)
	if err != nil {
		return nil, fmt.Errorf("auth: verify password for %q: %w", username, err)
	}
	if !ok {
		return nil, ErrBadCredentials
	}
	return user, nil
}

// LoginStore is the lookup Authenticate needs.
type LoginStore interface {
	UserByUsername(ctx context.Context, username string) (*store.User, error)
}

// ErrBadCredentials is returned for both an unknown username and a wrong
// password. The caller must not distinguish them: an error that says which one
// was wrong tells an attacker whether an account exists.
var ErrBadCredentials = errors.New("auth: invalid credentials")

// decoyHash is a real argon2id hash of a value nobody knows, computed once on
// first use with the same parameters as a live hash.
//
// Verifying against it is how the unknown-username path spends the same work
// as the known one. Without it, login latency answers "does this account
// exist?" — a fast rejection means no user, a slow one means the password was
// wrong — and that is a user enumeration oracle on a system with no public
// registration, where the set of usernames is meant to be private.
//
// This removes the dominant timing signal, which is the argon2 work. It does
// not make the two paths bit-identical in duration; the database lookup still
// differs. Equalising that fully is not something this layer can promise.
var decoyHash = sync.OnceValue(func() string {
	filler := make([]byte, 32)
	if _, err := rand.Read(filler); err != nil {
		// Fall back to a fixed value rather than failing login entirely: a
		// predictable decoy is still a decoy, since it is never compared
		// against anything an attacker controls.
		filler = []byte("inventory-decoy-fallback-value--")
	}
	hash, err := HashPassword(string(filler))
	if err != nil {
		return ""
	}
	return hash
})

func isNotFound(err error) bool {
	return errors.Is(err, store.ErrNotFound)
}
