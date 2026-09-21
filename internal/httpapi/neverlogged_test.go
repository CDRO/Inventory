package httpapi_test

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CDRO/Inventory/internal/config"
	"github.com/CDRO/Inventory/internal/httpapi"
)

// The sentinels driven through the API below.
//
// Distinctive strings rather than realistic ones on purpose: "password" or
// "secret" occur in field names, error messages and route patterns, so a
// substring search for a realistic value would either find itself everywhere
// or, worse, be quietly satisfied by a match that was not the secret. A
// sentinel that appears nowhere else in the repository can only match the
// value that was actually logged.
const (
	sentinelPassword    = "zzz-plaintext-password-zzz"
	sentinelNewPassword = "zzz-reset-password-zzz"
	sentinelAPIKey      = "zzz-gemini-api-key-zzz"
	sentinelSerpKey     = "zzz-serpapi-key-zzz"
	sentinelSessionKey  = "zzz-session-secret-zzz"

	// An Idempotency-Key has to be a UUID the client generated
	// (docs/specs/12-client-api-contract.md), so this sentinel cannot be
	// zzz-shaped like the rest. It is a fixed UUID that appears nowhere else
	// in the repository, which serves the same purpose.
	sentinelIdemKey = "b7e6d5c4-3a21-4f08-9e7d-6c5b4a392817"
	// And the malformed one, to exercise the refusal path: a rejected key is
	// still a key somebody sent, and the 422 that refuses it is written by the
	// error serializer, which logs.
	sentinelBadIdemKey = "zzz-idempotency-key-zzz"
)

// TestNothingSecretReachesTheLog is the acceptance criterion of
// docs/specs/18-operations-and-observability.md: "grepping a production log
// capture for a live session token, a pairing code, a password, or an API key
// finds nothing".
//
// # Why it drives real requests rather than reading the code
//
// The spec's own wording is about a log capture, and it is right to be: the
// leak this guards against is never a call that obviously logs a password. It
// is a middleware that logs the request headers "for debugging", or an error
// path that logs the whole decoded body, or a filename built from user text.
// None of those look like a password anywhere in the source.
//
// # Why the positive half matters as much
//
// A test that only asserts absence passes perfectly when nothing ran at all —
// an empty buffer contains no secrets. So every flow below is first checked
// to have produced its own completion line. If a route is renamed out from
// under this test, it fails loudly rather than silently becoming vacuous.
func TestNothingSecretReachesTheLog(t *testing.T) {
	t.Parallel()

	capture, logger := newCapturedLog()
	f := newAPIFixture(t, func(d *httpapi.Deps) {
		d.Logger = logger
		// Production wires one logger through both: the completion lines and
		// the serializer's own "request failed" lines
		// (cmd/inventory/main.go). A capture that saw only one of them would
		// miss whichever half leaked.
		//
		// dev, which is the *worse* case for disclosure and therefore the one
		// worth testing: this is the mode in which a reason is serialized into
		// the response as debug_reason, so it is the mode in which a handler
		// is most tempted to put something useful into one.
		d.Errors = httpapi.NewErrorWriter(true, logger)
		d.Config = &config.Config{
			AppEnv: "dev", HTTPPort: "8000",
			GeminiAPIKey:  sentinelAPIKey,
			SerpAPIKey:    sentinelSerpKey,
			SessionSecret: sentinelSessionKey,
		}
	})
	f.auth.withPassword(t, f.user, sentinelPassword)
	f.auth.setAdmin(f.user.ID, true)

	// --- login, successful and failed -------------------------------------
	login := postJSON(f.router, "/api/auth/login",
		`{"username":"`+f.user.Username+`","password":"`+sentinelPassword+`"}`, nil)
	require.Equal(t, http.StatusOK, login.Code)
	cookie := sessionCookieFrom(login)
	require.NotNil(t, cookie)
	liveToken := cookie.Value
	require.NotEmpty(t, liveToken)

	failed := postJSON(f.router, "/api/auth/login",
		`{"username":"`+f.user.Username+`","password":"`+sentinelPassword+`-wrong"}`, nil)
	require.Equal(t, http.StatusUnauthorized, failed.Code,
		"the failed attempt is the one whose body an error path might log")

	// --- pairing ----------------------------------------------------------
	codeRec := f.do(http.MethodPost, "/api/auth/pairing-codes", "")
	require.Equal(t, http.StatusCreated, codeRec.Code)
	var codeBody struct {
		Code string `json:"code"`
	}
	require.NoError(t, json.Unmarshal(codeRec.Body.Bytes(), &codeBody))
	require.NotEmpty(t, codeBody.Code)

	paired := postJSON(f.router, "/api/auth/pair",
		`{"code":"`+codeBody.Code+`","label":"Kitchen phone"}`, nil)
	require.Equal(t, http.StatusCreated, paired.Code)

	// A redeemed code is refused the second time, and a refusal is logged with
	// its reason — the most likely place for the code itself to ride along.
	reused := postJSON(f.router, "/api/auth/pair", `{"code":"`+codeBody.Code+`"}`, nil)
	require.NotEqual(t, http.StatusCreated, reused.Code)

	// --- an upload, with an Idempotency-Key -------------------------------
	image := jpegWithEXIF(t, 8, 4, 6)
	upload := uploadWithIdempotencyKey(t, f, f.base()+"/ingest/shelf-photos", image, sentinelIdemKey)
	require.Equal(t, http.StatusAccepted, upload.Code)

	require.NotEmpty(t, f.ingester.started, "the upload really reached the ingester")
	storedName := f.ingester.started[len(f.ingester.started)-1].Filename
	require.NotEmpty(t, storedName)

	refusedKey := uploadWithIdempotencyKey(t, f, f.base()+"/ingest/shelf-photos", image, sentinelBadIdemKey)
	require.Equal(t, http.StatusUnprocessableEntity, refusedKey.Code,
		"a key that is not a UUID is refused — and the refusal is logged")

	// --- an admin password reset ------------------------------------------
	victim, _ := f.auth.addUser(t, false)
	reset := f.do(http.MethodPost, "/api/admin/users/"+victim.ID.String()+"/password",
		`{"new_password":"`+sentinelNewPassword+`"}`)
	require.Equal(t, http.StatusNoContent, reset.Code)

	// --- the positive half: every flow above logged something -------------
	logged := capture.completions(t)
	patterns := []string{
		"/api/auth/login",
		"/api/auth/pairing-codes",
		"/api/auth/pair",
		"/api/storages/{storage_id}/ingest/shelf-photos",
		"/api/admin/users/{id}/password",
	}
	for _, pattern := range patterns {
		found := false
		for _, line := range logged {
			if line.Path == pattern {
				found = true
				break
			}
		}
		assert.Truef(t, found, "the log carries a completion line for %s — without it the "+
			"absence assertions below prove nothing", pattern)
	}

	// --- the negative half: the spec's list, item by item ------------------
	captured := capture.text()
	require.NotEmpty(t, captured)

	forbidden := map[string]string{
		"a live session token":       liveToken,
		"the fixture session token":  f.session.ID,
		"a pairing code":             codeBody.Code,
		"a password":                 sentinelPassword,
		"a password being reset":     sentinelNewPassword,
		"an Idempotency-Key value":   sentinelIdemKey,
		"a refused Idempotency-Key":  sentinelBadIdemKey,
		"the Gemini API key":         sentinelAPIKey,
		"the SerpAPI key":            sentinelSerpKey,
		"the session secret":         sentinelSessionKey,
		"the stored image file name": storedName,
	}
	for what, secret := range forbidden {
		assert.NotContainsf(t, captured, secret, "%s must never be logged", what)
	}

	// Image *bytes*, not just the name. A handler that logged the decoded body
	// would put the JPEG's own header into the log, which no filename check
	// would notice.
	assert.NotContains(t, captured, string(image[:16]), "no image bytes")
	assert.NotContains(t, captured, "IMG_0001", "not even the name the client sent")
}

// uploadWithIdempotencyKey posts a photo carrying an Idempotency-Key header.
//
// The fixture's own photoUpload puts extra values in the multipart *form*, and
// the key is a header — the distinction matters here, because a header is
// precisely the thing a "log the request headers while debugging" change would
// sweep into the log (docs/specs/12-client-api-contract.md).
func uploadWithIdempotencyKey(t *testing.T, f *apiFixture, path string, image []byte, key string) *httptest.ResponseRecorder {
	t.Helper()

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("image", "IMG_0001.jpg")
	require.NoError(t, err)
	_, err = part.Write(image)
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	req := httptest.NewRequest(http.MethodPost, path, &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Idempotency-Key", key)
	req.AddCookie(&http.Cookie{Name: httpapi.SessionCookie, Value: f.session.ID})

	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	return rec
}

// TestNoLogCallNamesASecretAttribute is the structural companion.
//
// The test above proves that today's routes leak nothing; it cannot prove
// anything about the log call somebody adds next month, which is exactly what
// the spec's "a new log call anywhere in the codebase must be checked against
// [the never-logged list]" asks for. So this one scans the source for a log
// attribute *named* after one of the forbidden things. It is a blunt
// instrument and deliberately so: the cost of renaming an attribute is
// nothing, and the cost of finding out from a log file is high.
func TestNoLogCallNamesASecretAttribute(t *testing.T) {
	t.Parallel()

	// slog.String("token", …), slog.Any("password", …), and so on, for the
	// names the spec's list rules out.
	pattern := regexp.MustCompile(`slog\.\w+\(\s*"(?:[^"]*_)?(?:token|password|passwordhash|password_hash|secret|apikey|api_key|pairing_code|idempotency_key|cookie)(?:_[^"]*)?"`)

	var offenders []string
	root := filepath.Join("..", "..")
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			// Vendored or generated trees have their own rules; this is about
			// the code in this repository.
			if info.Name() == ".git" || info.Name() == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		source, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if pattern.Match(source) {
			offenders = append(offenders, filepath.ToSlash(strings.TrimPrefix(path, root+string(filepath.Separator))))
		}
		return nil
	})
	require.NoError(t, err)

	assert.Emptyf(t, offenders, "a log attribute is named after something "+
		"docs/specs/18-operations-and-observability.md forbids logging; found in: %v", offenders)
}

// TestTheRequestIDIsNotDerivedFromAnythingSensitive.
//
// A request id is echoed to the client and written to the log, so it has to be
// meaningless on its own. Deriving one from the session — an obvious way to
// make ids "useful" — would publish a stable identifier for a credential in
// every response header.
func TestTheRequestIDIsNotDerivedFromAnythingSensitive(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t, func(d *httpapi.Deps) { d.Logger = discardLogger() })

	first := f.do(http.MethodGet, "/api/auth/me", "").Header().Get(httpapi.RequestIDHeader)
	second := f.do(http.MethodGet, "/api/auth/me", "").Header().Get(httpapi.RequestIDHeader)

	require.NotEmpty(t, first)
	assert.NotEqual(t, first, second, "two requests on one session get different ids")
	assert.NotContains(t, first, f.session.ID)
	_, err := uuid.Parse(first)
	assert.NoError(t, err)
}
