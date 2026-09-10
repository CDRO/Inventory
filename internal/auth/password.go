// Package auth owns credential handling: how a password becomes a stored hash,
// how a stored hash verifies an attempt, and the first-boot admin that gives a
// fresh deployment somewhere to log in.
//
// Authorization decisions are not here. Who may see which storage, and whether
// a caller is an admin, are answered by the middleware in internal/httpapi
// against the database (docs/specs/03-auth-and-multi-tenancy.md).
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// argon2id parameters.
//
// These are OWASP's recommended floor: 19 MiB of memory, two iterations, one
// lane. The memory cost is what makes a stolen hash expensive to attack, and
// it is chosen with the deployment target in mind — this runs on a NAS
// alongside PostgreSQL, so a 64 MiB-per-login setting would be a self-inflicted
// denial of service on the box the household depends on.
const (
	argonMemoryKiB = 19 * 1024
	argonTime      = 2
	argonThreads   = 1
	argonSaltBytes = 16
	argonKeyBytes  = 32
)

// ErrInvalidHash means a stored value is not a hash this package wrote.
var ErrInvalidHash = errors.New("auth: malformed password hash")

// HashPassword returns an encoded argon2id hash.
//
// The encoding carries the parameters and the salt alongside the digest, so a
// future increase in cost does not invalidate existing hashes: each one still
// verifies against the parameters it was created with.
func HashPassword(password string) (string, error) {
	salt := make([]byte, argonSaltBytes)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("auth: generate salt: %w", err)
	}

	digest := argon2.IDKey([]byte(password), salt, argonTime, argonMemoryKiB, argonThreads, argonKeyBytes)

	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemoryKiB, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(digest),
	), nil
}

// VerifyPassword reports whether password matches the encoded hash.
//
// The comparison is constant-time. A byte-by-byte compare that returns early
// leaks, through timing, how much of a guess was correct, which turns an
// offline problem into an online one.
//
// A malformed stored hash is an error rather than a false: it means the row is
// corrupt or was written by something else, and treating that as "wrong
// password" would hide the corruption behind a login failure nobody
// investigates.
func VerifyPassword(encoded, password string) (bool, error) {
	params, salt, want, err := decodeHash(encoded)
	if err != nil {
		return false, err
	}

	got := argon2.IDKey([]byte(password), salt, params.time, params.memory, params.threads, uint32(len(want)))

	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

type argonParams struct {
	memory  uint32
	time    uint32
	threads uint8
}

func decodeHash(encoded string) (argonParams, []byte, []byte, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return argonParams{}, nil, nil, ErrInvalidHash
	}

	// Sscanf alone would accept trailing rubbish — "v=19junk" parses as 19 —
	// so each segment is re-rendered and compared against the input. A field
	// this code is willing to misread is a field an attacker gets to choose.
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return argonParams{}, nil, nil, ErrInvalidHash
	}
	if fmt.Sprintf("v=%d", version) != parts[2] {
		return argonParams{}, nil, nil, ErrInvalidHash
	}
	if version != argon2.Version {
		return argonParams{}, nil, nil, fmt.Errorf("%w: unsupported version %d", ErrInvalidHash, version)
	}

	var p argonParams
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.memory, &p.time, &p.threads); err != nil {
		return argonParams{}, nil, nil, ErrInvalidHash
	}
	if fmt.Sprintf("m=%d,t=%d,p=%d", p.memory, p.time, p.threads) != parts[3] {
		return argonParams{}, nil, nil, ErrInvalidHash
	}
	// argon2.IDKey panics on a zero memory, time or thread count, so a hash
	// claiming any of them is refused here rather than taking the process down.
	if p.memory == 0 || p.time == 0 || p.threads == 0 {
		return argonParams{}, nil, nil, fmt.Errorf("%w: zero argon2 parameter", ErrInvalidHash)
	}

	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return argonParams{}, nil, nil, ErrInvalidHash
	}
	digest, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return argonParams{}, nil, nil, ErrInvalidHash
	}
	if len(salt) == 0 || len(digest) == 0 {
		return argonParams{}, nil, nil, ErrInvalidHash
	}

	return p, salt, digest, nil
}
