package httpapi_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CDRO/Inventory/internal/httpapi"
	"github.com/CDRO/Inventory/internal/logging"
)

// capturedLog is a log sink a test can read back.
//
// It wraps the real logging.Handler rather than a bare JSON handler, so what a
// test inspects is what production would have written — request ids included.
// A sink that skipped the wrapper would happily prove that nothing carries a
// request id.
type capturedLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func newCapturedLog() (*capturedLog, *slog.Logger) {
	capture := &capturedLog{}
	handler := logging.NewHandler(slog.NewJSONHandler(capture, &slog.HandlerOptions{Level: slog.LevelInfo}))
	return capture, slog.New(handler)
}

func (c *capturedLog) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Write(p)
}

// text is the raw capture, for the "this string appears nowhere" assertions.
func (c *capturedLog) text() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

// logLine is one decoded record. Only the fields the spec names are typed; the
// rest stays in Extra so a test can assert about a key without this struct
// having to know every key in advance.
type logLine struct {
	Level      string
	Msg        string
	RequestID  string
	Method     string
	Path       string
	Status     int
	DurationMS int64
	UserID     string
	StorageID  string
	Extra      map[string]any
}

func (c *capturedLog) lines(t *testing.T) []logLine {
	t.Helper()

	var out []logLine
	for _, raw := range strings.Split(strings.TrimSpace(c.text()), "\n") {
		if raw == "" {
			continue
		}
		var fields map[string]any
		require.NoError(t, json.Unmarshal([]byte(raw), &fields), "log line is not JSON: %s", raw)

		line := logLine{Extra: fields}
		line.Level, _ = fields["level"].(string)
		line.Msg, _ = fields["msg"].(string)
		line.RequestID, _ = fields["request_id"].(string)
		line.Method, _ = fields["method"].(string)
		line.Path, _ = fields["path"].(string)
		if status, ok := fields["status"].(float64); ok {
			line.Status = int(status)
		}
		if duration, ok := fields["duration_ms"].(float64); ok {
			line.DurationMS = int64(duration)
		}
		line.UserID, _ = fields["user_id"].(string)
		line.StorageID, _ = fields["storage_id"].(string)
		out = append(out, line)
	}
	return out
}

// completions returns just the one-per-request completion lines.
func (c *capturedLog) completions(t *testing.T) []logLine {
	t.Helper()

	var out []logLine
	for _, line := range c.lines(t) {
		if line.Msg == "request" {
			out = append(out, line)
		}
	}
	return out
}

// chiRouterLogging mounts one handler behind the same two middlewares, in the
// same order, that NewRouter uses: RequestLogger outside, Recoverer inside.
//
// Reproducing the order rather than reaching for the real router is the point
// of these two tests — they are about what the pair does to a status code, and
// the real router has no route that returns a chosen status or panics on
// demand.
func chiRouterLogging(logger *slog.Logger, handler http.HandlerFunc) http.Handler {
	r := chi.NewRouter()
	r.Use(httpapi.RequestLogger(logger))
	r.Use(silenceRecovererStack)
	r.Use(middleware.Recoverer)
	r.Get("/boom", handler)
	return r
}

// silenceRecovererStack keeps chi's Recoverer from dumping a stack trace onto
// stderr during the panic test.
//
// Recoverer prints one itself only when no chi LogEntry is in the context, so
// putting a do-nothing entry there is enough. The alternative — letting it
// print — leaves a stack trace in the middle of a passing suite, which reads
// as a failure to whoever runs it next.
func silenceRecovererStack(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, middleware.WithLogEntry(r, quietLogEntry{}))
	})
}

type quietLogEntry struct{}

func (quietLogEntry) Write(int, int, http.Header, time.Duration, any) {}
func (quietLogEntry) Panic(any, []byte)                               {}

// TestEveryRequestProducesExactlyOneCompletionLine is the first acceptance
// criterion of docs/specs/18-operations-and-observability.md.
//
// "Exactly one" is the part worth testing: a middleware that logged on the way
// in as well as on the way out doubles the volume of the only observability
// surface this deployment has, and every field would still be correct.
func TestEveryRequestProducesExactlyOneCompletionLine(t *testing.T) {
	t.Parallel()

	capture, logger := newCapturedLog()
	f := newAPIFixture(t, func(d *httpapi.Deps) { d.Logger = logger })

	rec := f.do(http.MethodGet, "/api/storages/"+f.storageID.String()+"/locations", "")
	require.Equal(t, http.StatusOK, rec.Code)

	lines := capture.completions(t)
	require.Len(t, lines, 1)

	line := lines[0]
	assert.Equal(t, "INFO", line.Level)
	assert.Equal(t, http.MethodGet, line.Method)
	assert.Equal(t, "/api/storages/{storage_id}/locations", line.Path,
		"the route pattern, never the raw URL with its ids in it")
	assert.Equal(t, http.StatusOK, line.Status)
	assert.NotEmpty(t, line.RequestID)
	assert.GreaterOrEqual(t, line.DurationMS, int64(0))
	assert.Equal(t, f.user.ID.String(), line.UserID)
	assert.Equal(t, f.storageID.String(), line.StorageID)

	// The ids resolved by the gates are UUIDs and leak nothing; the raw path
	// they came from would have carried the same ids plus any query string,
	// which is what the spec rules out.
	assert.NotContains(t, capture.text(), "/api/storages/"+f.storageID.String()+"/locations")
}

// TestTheRequestIDOnTheHeaderIsTheOneInTheLog is the other half of that
// criterion — and the half that makes the id useful at all. An id that
// appeared in the log but not on the response is an id nobody can quote when
// reporting a problem.
func TestTheRequestIDOnTheHeaderIsTheOneInTheLog(t *testing.T) {
	t.Parallel()

	capture, logger := newCapturedLog()
	f := newAPIFixture(t, func(d *httpapi.Deps) { d.Logger = logger })

	rec := f.do(http.MethodGet, "/api/storages/"+f.storageID.String()+"/locations", "")

	header := rec.Header().Get(httpapi.RequestIDHeader)
	require.NotEmpty(t, header)
	_, err := uuid.Parse(header)
	require.NoError(t, err, "a UUID, like every other id in the system")

	lines := capture.completions(t)
	require.Len(t, lines, 1)
	assert.Equal(t, header, lines[0].RequestID)
}

// TestEveryLineLoggedDuringARequestCarriesTheSameRequestID is the spec's "and
// on every other line logged during that request".
//
// Driven through a failure so that a *second* line exists: the error
// serializer writes one of its own for every refusal (errors.go). If only the
// completion line carried the id, correlating a 500 with the reason it was a
// 500 would still be guesswork, which is the whole thing this is for.
func TestEveryLineLoggedDuringARequestCarriesTheSameRequestID(t *testing.T) {
	t.Parallel()

	capture, logger := newCapturedLog()
	f := newAPIFixture(t, func(d *httpapi.Deps) {
		d.Logger = logger
		// The same logger behind the serializer, as cmd/inventory wires it.
		d.Errors = httpapi.NewErrorWriter(false, logger)
	})

	rec := f.do(http.MethodGet, "/api/storages/"+uuid.NewString()+"/locations", "")
	require.Equal(t, http.StatusNotFound, rec.Code)

	lines := capture.lines(t)
	require.GreaterOrEqual(t, len(lines), 2, "the refusal and the completion")

	id := rec.Header().Get(httpapi.RequestIDHeader)
	require.NotEmpty(t, id)
	for _, line := range lines {
		assert.Equal(t, id, line.RequestID, "line %q carries the request id", line.Msg)
	}
}

// TestA500IsLoggedAtErrorAnd4xxAtInfo pins the level vocabulary.
//
// Promoting refusals to warn would make a log of an ordinary day — expired
// sessions, 404s for storages people may not see — look like a log of an
// incident, and an operator who learns to ignore warnings ignores the ones
// that matter.
func TestA500IsLoggedAtErrorAnd4xxAtInfo(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		status    int
		wantLevel string
	}{
		{name: "not found stays info", status: http.StatusNotFound, wantLevel: "INFO"},
		{name: "server error is error", status: http.StatusInternalServerError, wantLevel: "ERROR"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			capture, logger := newCapturedLog()
			router := chiRouterLogging(logger, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
			})

			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/boom", nil))
			require.Equal(t, tc.status, rec.Code)

			lines := capture.completions(t)
			require.Len(t, lines, 1)
			assert.Equal(t, tc.wantLevel, lines[0].Level)
		})
	}
}

// TestAPanicIsOneCompletionLineSayingFiveHundred.
//
// The middleware is registered outside Recoverer precisely so this holds. The
// other order logs the status the handler had reached before it panicked —
// 200, because nothing had been written — which is worse than not logging it
// at all: a 500 the operator can see is a bug report, a 200 that was really a
// panic is a lie in the record.
func TestAPanicIsOneCompletionLineSayingFiveHundred(t *testing.T) {
	t.Parallel()

	capture, logger := newCapturedLog()
	router := chiRouterLogging(logger, func(http.ResponseWriter, *http.Request) {
		panic("handler exploded")
	})

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/boom", nil))
	require.Equal(t, http.StatusInternalServerError, rec.Code)

	lines := capture.completions(t)
	require.Len(t, lines, 1, "exactly one line, even for a panic")
	assert.Equal(t, http.StatusInternalServerError, lines[0].Status)
	assert.Equal(t, "ERROR", lines[0].Level)
}

// TestAnUnmatchedPathIsLoggedAsUnmatchedRatherThanEchoedBack.
//
// A miss has no route pattern, and falling back to the raw URL would put
// whatever was probed — a path traversal attempt, a token pasted into the
// address bar — into the log file, which is the one place the spec says such
// things must never end up.
func TestAnUnmatchedPathIsLoggedAsUnmatchedRatherThanEchoedBack(t *testing.T) {
	t.Parallel()

	capture, logger := newCapturedLog()
	f := newAPIFixture(t, func(d *httpapi.Deps) {
		d.Logger = logger
		// **Both** halves of the pipeline on the capture. Wiring only
		// Deps.Logger makes this test pass while the error serializer — which
		// logs a line of its own for every refusal, and a 404 is a refusal —
		// writes the raw path to a discard logger nobody looks at. That is
		// exactly how this assertion passed while the leak it names was live.
		d.Errors = httpapi.NewErrorWriter(false, logger)
	})

	const probe = "/nothing/here/zzz-probe-value-zzz"
	rec := f.anonymous(http.MethodGet, probe, "")
	require.Equal(t, http.StatusNotFound, rec.Code)

	lines := capture.completions(t)
	require.Len(t, lines, 1)
	assert.Equal(t, "unmatched", lines[0].Path)
	assert.NotContains(t, capture.text(), "zzz-probe-value-zzz",
		"the probed path appears nowhere in the log — not on the completion "+
			"line, and not in the serializer's reason either")
}

// TestTheServerNeverLogsARawURL is the general form of the test above.
//
// The spec's reason for logging the route pattern is not tidiness: a raw URL
// carries the ids of whatever it addressed, and a query string carries
// whatever a person typed into a search box. Both halves of the log pipeline
// have to honour that, so this drives a matched route, a refusal, and a miss,
// each with a sentinel in the path *and* in the query, and asserts none of
// them survived anywhere.
func TestTheServerNeverLogsARawURL(t *testing.T) {
	t.Parallel()

	capture, logger := newCapturedLog()
	f := newAPIFixture(t, func(d *httpapi.Deps) {
		d.Logger = logger
		d.Errors = httpapi.NewErrorWriter(false, logger)
	})

	const inQuery = "zzz-typed-into-a-search-box-zzz"

	// A route that exists, with a query string.
	require.Equal(t, http.StatusOK,
		f.do(http.MethodGet, f.base()+"/products?q="+inQuery, "").Code)
	// A storage the caller may not see — the refusal the serializer logs.
	require.Equal(t, http.StatusNotFound,
		f.do(http.MethodGet, "/api/storages/"+uuid.NewString()+"/locations?q="+inQuery, "").Code)
	// A path that does not exist at all, with a sentinel in the path itself.
	require.Equal(t, http.StatusNotFound,
		f.do(http.MethodGet, "/zzz-probed-path-zzz", "").Code)
	// And a verb no route accepts.
	require.Equal(t, http.StatusNotFound,
		f.do(http.MethodPut, "/zzz-probed-path-zzz", "{}").Code)

	captured := capture.text()
	require.NotEmpty(t, captured)
	require.GreaterOrEqual(t, len(capture.completions(t)), 4,
		"all four requests logged, so the assertions below are not vacuous")

	assert.NotContains(t, captured, inQuery, "no query string reaches the log")
	assert.NotContains(t, captured, "zzz-probed-path-zzz", "and no raw path either")
}

// TestHealthzReportsTheBuiltVersion, and an unstamped build says "dev".
func TestHealthzReportsTheBuiltVersion(t *testing.T) {
	t.Parallel()

	tests := []struct{ name, version, want string }{
		{name: "a stamped build reports its version", version: "2026-09-21.1", want: "2026-09-21.1"},
		{name: "an unstamped build reports dev", version: "", want: "dev"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			router := httpapi.NewRouter(httpapi.Deps{
				DB: stubPinger{}, Vision: stubVision{status: "ok"},
				Logger: discardLogger(), Version: tc.version,
			})

			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
			require.Equal(t, http.StatusOK, rec.Code)

			var body httpapi.HealthResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
			assert.Equal(t, tc.want, body.Version)
		})
	}
}
