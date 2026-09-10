package auth_test

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/argon2"

	"github.com/CDRO/Inventory/internal/auth"
)

func TestHashAndVerifyRoundTrip(t *testing.T) {
	t.Parallel()

	const password = "correct horse battery staple"

	hash, err := auth.HashPassword(password)
	require.NoError(t, err)

	ok, err := auth.VerifyPassword(hash, password)
	require.NoError(t, err)
	assert.True(t, ok)
}

func TestVerifyRejectsWrongPassword(t *testing.T) {
	t.Parallel()

	hash, err := auth.HashPassword("the real password")
	require.NoError(t, err)

	for _, attempt := range []string{
		"the real passwore",  // one character off
		"the real password ", // trailing space
		"The real password",  // case
		"",
		"totally different",
	} {
		ok, err := auth.VerifyPassword(hash, attempt)
		require.NoError(t, err)
		assert.Falsef(t, ok, "%q must not verify", attempt)
	}
}

// TestHashIsSaltedPerCall is the property that makes a stolen users table
// expensive rather than merely inconvenient. Identical passwords hashing to
// identical strings would let an attacker sort the table and attack the most
// common value once for everybody who chose it.
func TestHashIsSaltedPerCall(t *testing.T) {
	t.Parallel()

	const password = "same password twice"

	first, err := auth.HashPassword(password)
	require.NoError(t, err)
	second, err := auth.HashPassword(password)
	require.NoError(t, err)

	assert.NotEqual(t, first, second, "the same password must not produce the same hash twice")

	// Both must still verify: different salts, same password.
	for _, hash := range []string{first, second} {
		ok, err := auth.VerifyPassword(hash, password)
		require.NoError(t, err)
		assert.True(t, ok)
	}
}

// TestHashEncodesItsParameters guards upgradability. The cost is written into
// the hash, so raising it later leaves existing hashes verifiable against the
// parameters they were made with — rather than locking every account out on
// the deploy that changes a constant.
func TestHashEncodesItsParameters(t *testing.T) {
	t.Parallel()

	hash, err := auth.HashPassword("x")
	require.NoError(t, err)

	assert.True(t, strings.HasPrefix(hash, "$argon2id$"), "the algorithm must be named in the hash")
	assert.Contains(t, hash, "$v=19$", "the argon2 version must be recorded")
	assert.Regexp(t, `\$m=\d+,t=\d+,p=\d+\$`, hash, "memory, time and parallelism must be recorded")

	parts := strings.Split(hash, "$")
	require.Len(t, parts, 6, "encoded form is $argon2id$v=..$m=..,t=..,p=..$salt$digest")
	assert.NotEmpty(t, parts[4], "salt")
	assert.NotEmpty(t, parts[5], "digest")
}

// TestHashDoesNotContainThePassword is the obvious mistake worth pinning: an
// encoding bug that stored the plaintext would still round-trip through
// VerifyPassword and pass every other test in this file.
func TestHashDoesNotContainThePassword(t *testing.T) {
	t.Parallel()

	const password = "SuperDistinctivePassphrase123"

	hash, err := auth.HashPassword(password)
	require.NoError(t, err)

	assert.NotContains(t, hash, password)
}

// TestVerifyRejectsMalformedHash — a corrupt row is an error, not a silent
// "wrong password". Treating it as a failed login would hide the corruption
// behind something nobody investigates.
func TestVerifyRejectsMalformedHash(t *testing.T) {
	t.Parallel()

	valid, err := auth.HashPassword("x")
	require.NoError(t, err)
	parts := strings.Split(valid, "$")

	tests := map[string]string{
		"empty":               "",
		"plaintext":           "hunter2",
		"bcrypt":              "$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy",
		"wrong algorithm":     "$argon2i$v=19$m=19456,t=2,p=1$" + parts[4] + "$" + parts[5],
		"missing parameters":  "$argon2id$v=19$" + parts[4] + "$" + parts[5],
		"bad base64 salt":     "$argon2id$v=19$m=19456,t=2,p=1$!!!not-base64!!!$" + parts[5],
		"unsupported version": "$argon2id$v=13$m=19456,t=2,p=1$" + parts[4] + "$" + parts[5],
	}

	for name, encoded := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			ok, err := auth.VerifyPassword(encoded, "x")
			assert.False(t, ok)
			require.Error(t, err, "a malformed hash must be reported, not swallowed as a mismatch")
			assert.ErrorIs(t, err, auth.ErrInvalidHash)
		})
	}
}

// TestVerifyHonoursEncodedParameters proves the parameters in the hash are
// actually used rather than being decoration overridden by the constants. A
// hash made with different settings must still verify.
func TestVerifyHonoursEncodedParameters(t *testing.T) {
	t.Parallel()

	// Same password, deliberately weaker settings than the package default.
	const password = "parameterised"
	weak := "$argon2id$v=19$m=8,t=1,p=1$c29tZXNhbHRzb21lc2FsdA$" +
		mustDigest(t, password, "c29tZXNhbHRzb21lc2FsdA", 8, 1, 1)

	ok, err := auth.VerifyPassword(weak, password)
	require.NoError(t, err)
	assert.True(t, ok, "a hash made with other parameters must verify against those parameters")

	ok, err = auth.VerifyPassword(weak, "wrong")
	require.NoError(t, err)
	assert.False(t, ok)
}

// mustDigest computes an argon2id digest with explicit parameters, so a test
// can build a hash that was not made with the package's own constants.
func mustDigest(t *testing.T, password, saltB64 string, memory, time uint32, threads uint8) string {
	t.Helper()

	salt, err := base64.RawStdEncoding.DecodeString(saltB64)
	require.NoError(t, err)

	digest := argon2.IDKey([]byte(password), salt, time, memory, threads, 32)
	return base64.RawStdEncoding.EncodeToString(digest)
}
