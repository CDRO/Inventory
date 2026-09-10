package httpapi_test

import (
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CDRO/Inventory/internal/httpapi"
	"github.com/CDRO/Inventory/internal/store"
)

// writeFailure renders one failure through the serializer and returns the
// recorded response.
func writeFailure(dev bool, failure *httpapi.Failure) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/storages/018f/products", nil)
	httpapi.NewErrorWriter(dev, discardLogger()).WriteError(rec, req, failure)
	return rec
}

// TestDebugReasonAppearsOnlyInDev is the invariant in its most direct form.
//
// Every failure kind is rendered twice, with the same reason. Under dev the
// reason is disclosed; under anything else the response must not contain it in
// any form — not in the field, not in the message, not anywhere in the bytes.
func TestDebugReasonAppearsOnlyInDev(t *testing.T) {
	t.Parallel()

	failures := map[string]*httpapi.Failure{
		"unauthorized":  httpapi.Unauthorized(httpapi.ReasonSessionMissing),
		"not found":     httpapi.NotFound(httpapi.ReasonNotStorageMember),
		"storage gone":  httpapi.NotFound(httpapi.ReasonStorageNotFound),
		"not admin":     httpapi.NotFound(httpapi.ReasonNotAdmin),
		"admin hidden":  httpapi.NotFound(httpapi.ReasonAdminAreaHidden),
		"model missing": httpapi.ModelUnavailable("gemini-1.0-pro"),
	}

	for name, failure := range failures {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			reason := failure.Reason
			require.NotEmpty(t, reason, "the fixture must carry a reason to leak")

			dev := writeFailure(true, cloneFailure(failure))
			assert.Contains(t, dev.Body.String(), reason,
				"dev is where the reason is supposed to be visible")

			prod := writeFailure(false, cloneFailure(failure))
			assert.NotContains(t, prod.Body.String(), reason,
				"production must not disclose which internal check failed")
			assert.NotContains(t, prod.Body.String(), "debug_reason",
				"the field itself must be absent, not merely empty")
		})
	}
}

// TestProductionErrorShapeIsMinimal pins what a production caller actually
// receives, so a future addition to the envelope has to be a deliberate change
// to this test rather than an accident.
func TestProductionErrorShapeIsMinimal(t *testing.T) {
	t.Parallel()

	rec := writeFailure(false, httpapi.NotFound(httpapi.ReasonNotStorageMember))

	var envelope map[string]map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &envelope))

	inner, ok := envelope["error"]
	require.True(t, ok, "every error is wrapped in an \"error\" object")

	keys := make([]string, 0, len(inner))
	for k := range inner {
		keys = append(keys, k)
	}
	assert.ElementsMatch(t, []string{"code", "message"}, keys,
		"a production error carries a code and a message, nothing else")
	assert.Equal(t, "not_found", inner["code"])
}

// TestUnknownAndInaccessibleAreByteIdentical is the 404-not-403 rule at the
// serializer.
//
// The two cases arrive with different reasons — that is how an operator tells
// them apart in the log, and how a developer sees them in dev. What a
// production client receives must be indistinguishable down to the byte,
// because any difference at all confirms that an id names a real storage and
// turns id-guessing into a way to map households you cannot see.
func TestUnknownAndInaccessibleAreByteIdentical(t *testing.T) {
	t.Parallel()

	inaccessible := writeFailure(false, httpapi.NotFound(httpapi.ReasonNotStorageMember))
	nonexistent := writeFailure(false, httpapi.NotFound(httpapi.ReasonStorageNotFound))
	adminArea := writeFailure(false, httpapi.NotFound(httpapi.ReasonNotAdmin))

	assert.Equal(t, nonexistent.Code, inaccessible.Code)
	assert.Equal(t, nonexistent.Body.Bytes(), inaccessible.Body.Bytes(),
		"an inaccessible storage must be byte-identical to a nonexistent one")
	assert.Equal(t, nonexistent.Header(), inaccessible.Header(),
		"headers must not differ either")

	assert.Equal(t, nonexistent.Body.Bytes(), adminArea.Body.Bytes(),
		"the admin area must not announce its own existence")
	assert.Equal(t, nonexistent.Header(), adminArea.Header())
}

// TestDebugReasonLivesInExactlyOneFile is the structural half of the rule.
//
// The behavioural tests above prove the serializer gates correctly. They
// cannot prove that some future handler will not write its own envelope with
// its own reason field — which is precisely how this leaks in a codebase where
// the rule is "remember to strip it". This test fails the moment the string
// appears anywhere but the one file allowed to know about it.
func TestDebugReasonLivesInExactlyOneFile(t *testing.T) {
	t.Parallel()

	// Matches an emission — a struct tag or a string literal — rather than a
	// mention. Prose about the rule is fine; a second place that can put the
	// field on the wire is not.
	offenders := grepGoSources(t, []string{`json:"debug_reason`, `"debug_reason"`}, "internal/httpapi/errors.go")

	assert.Emptyf(t, offenders, "debug_reason may only be written by the single serializer; also found in: %v", offenders)
}

// TestEncodingAUserLeaksNothing is the behavioural half of the admin
// invariant, and the one that matters.
//
// An earlier version of this test only grepped for a `json:"is_admin` tag. It
// passed because no such tag existed — which proved nothing: Go's encoder
// emits an exported field under its own name when there is no tag at all, so
// json.Encode of the *store.User already sitting in the request context (the
// obvious shortcut for a first cut of GET /api/auth/me) would have put
// "IsAdmin":true straight into the response, and the grep would still have
// been green. The same applied to the argon2 hash.
//
// So this marshals the real struct and reads the bytes.
func TestEncodingAUserLeaksNothing(t *testing.T) {
	t.Parallel()

	user := store.User{
		ID:           uuid.New(),
		Username:     "tizian",
		PasswordHash: "$argon2id$v=19$m=19456,t=2,p=1$c2FsdA$ZGlnZXN0",
		DisplayName:  "Tizian",
		IsAdmin:      true,
	}

	encoded, err := json.Marshal(user)
	require.NoError(t, err)
	body := string(encoded)

	assert.NotContains(t, body, "IsAdmin", "the field name must not survive encoding")
	assert.NotContains(t, body, "is_admin")
	assert.NotContains(t, body, "true", "nor its value under any name")
	assert.NotContains(t, body, "argon2", "and the password hash least of all")
	assert.NotContains(t, body, "PasswordHash")

	// The harmless fields are still there, so this is not passing because the
	// struct encodes to nothing.
	assert.Contains(t, body, "tizian")
}

// TestIsAdminIsNeverSerialized is the structural half: no declaration
// anywhere may name the field on the wire.
//
// Scanning the source rather than a sample of responses is deliberate. A test
// that checked three known endpoints would say nothing about the fourth one
// somebody adds next month. The patterns cover a struct tag and a map literal,
// which are the two ways a field reaches JSON under a chosen name.
func TestIsAdminIsNeverSerialized(t *testing.T) {
	t.Parallel()

	offenders := grepGoSources(t, []string{`json:"is_admin`, `"is_admin"`}, "")

	assert.Emptyf(t, offenders, "is_admin must never be put on the wire; found in: %v", offenders)
}

// TestRouterWithoutAnErrorWriterStillHidesReasons — the fail-safe default.
//
// Forgetting to pass an ErrorWriter must mean "no reasons disclosed", never
// "all of them". A default that opened up disclosure would make the safest
// configuration the one nobody wrote down.
func TestRouterWithoutAnErrorWriterStillHidesReasons(t *testing.T) {
	t.Parallel()

	router := httpapi.NewRouter(httpapi.Deps{}) // no Errors, no DB, no assets

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/no/such/route", nil))

	require.Equal(t, http.StatusNotFound, rec.Code)
	assert.NotContains(t, rec.Body.String(), "debug_reason")
	assert.NotContains(t, rec.Body.String(), "/no/such/route",
		"the reason names the path; it must not be disclosed by default")
	assert.Contains(t, rec.Body.String(), `"not_found"`,
		"and it must still be the API error shape, not chi plain text")
}

// TestFromStoreErrorMapsDomainErrors checks the status-code table.
func TestFromStoreErrorMapsDomainErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{"not found", store.ErrNotFound, http.StatusNotFound, "not_found"},
		{"wrapped not found", errors.New("x"), http.StatusInternalServerError, "internal_error"},
		{"conflict", store.ErrConflict, http.StatusConflict, "conflict"},
		{"validation", store.ErrValidation, http.StatusUnprocessableEntity, "validation_failed"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			failure := httpapi.FromStoreError(tc.err, "some reason")
			require.NotNil(t, failure)
			assert.Equal(t, tc.wantStatus, failure.Status)
			assert.Equal(t, tc.wantCode, failure.Code)
		})
	}

	assert.Nil(t, httpapi.FromStoreError(nil, ""), "no error is not a failure")
}

// TestInternalErrorDoesNotLeakItsCause — the cause goes to the log; a caller
// gets a fixed message. Database errors quote table and column names.
func TestInternalErrorDoesNotLeakItsCause(t *testing.T) {
	t.Parallel()

	cause := errors.New(`pq: relation "storage_members" does not exist`)

	rec := writeFailure(false, httpapi.Internal(cause))

	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.NotContains(t, rec.Body.String(), "storage_members")
	assert.NotContains(t, rec.Body.String(), "relation")

	// Not even in dev: the cause is an error, not a debug reason, and the
	// distinction is what keeps schema details out of both.
	devRec := writeFailure(true, httpapi.Internal(cause))
	assert.NotContains(t, devRec.Body.String(), "storage_members")
}

// cloneFailure returns a copy, because WriteError fills in defaults on the
// value it is given and the fixtures are shared between subtests.
func cloneFailure(f *httpapi.Failure) *httpapi.Failure {
	copied := *f
	return &copied
}

// grepGoSources returns the non-test Go files that contain any of the needles,
// excluding the one file allowed to.
func grepGoSources(t *testing.T, needles []string, allowed string) []string {
	t.Helper()

	root, err := filepath.Abs(filepath.Join("..", ".."))
	require.NoError(t, err)

	var offenders []string
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" || d.Name() == "docs" || d.Name() == ".claude" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}

		rel, relErr := filepath.Rel(root, path)
		require.NoError(t, relErr)
		rel = filepath.ToSlash(rel)
		if allowed != "" && rel == allowed {
			return nil
		}

		body, readErr := os.ReadFile(path)
		require.NoError(t, readErr)
		for _, needle := range needles {
			if strings.Contains(string(body), needle) {
				offenders = append(offenders, rel)
				break
			}
		}
		return nil
	})
	require.NoError(t, err)
	return offenders
}
