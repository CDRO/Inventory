package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"

	"github.com/CDRO/Inventory/internal/logging"
)

// RequestIDHeader is the response header carrying the request id
// (docs/specs/18-operations-and-observability.md). It is what turns "it failed
// around noon" into one grep.
const RequestIDHeader = "X-Request-Id"

// routeUnmatched is the path value logged when no route matched at all.
//
// The spec rules out logging the raw URL — it would fill the log with ids that
// are noise and query strings that can carry user text — and a miss has no
// route pattern to log instead. A fixed token says "nothing matched" without
// echoing whatever was probed back into the log file.
const routeUnmatched = "unmatched"

// logScope carries the identifiers the authorization gates resolve, so the
// completion line can name them.
//
// It is a mutable value in the request context, which is unusual enough to
// justify: user_id and storage_id are not known when the logging middleware
// runs — they are the *result* of RequireSession and RequireStorageMember, and
// those gates publish their findings by deriving a new request with a new
// context, which the outer middleware cannot see. A pointer placed in the
// context on the way in, filled in by the gates, and read on the way out is
// how the outermost frame learns what the innermost one decided.
//
// One request is handled by one goroutine, and the read happens after
// ServeHTTP has returned, so there is no race to guard against here.
type logScope struct {
	userID    string
	storageID string
}

type scopeKey struct{}

// scopeFrom returns the scope for this request, or nil when the request is not
// being logged (a handler exercised directly in a test, say). Every caller
// must tolerate nil rather than assume the middleware ran.
func scopeFrom(ctx context.Context) *logScope {
	scope, _ := ctx.Value(scopeKey{}).(*logScope)
	return scope
}

// noteUser records the authenticated caller for the completion log line.
func noteUser(ctx context.Context, id uuid.UUID) {
	if scope := scopeFrom(ctx); scope != nil {
		scope.userID = id.String()
	}
}

// noteStorage records the storage a request was scoped to.
func noteStorage(ctx context.Context, id uuid.UUID) {
	if scope := scopeFrom(ctx); scope != nil {
		scope.storageID = id.String()
	}
}

// RequestLogger mints a request id and logs exactly one completion line per
// request (docs/specs/18-operations-and-observability.md).
//
// # What is on the line, and what is deliberately not
//
// method, the chi route *pattern*, status, duration, and the user and storage
// ids once the gates have resolved them. Not the raw URL: it is full of
// UUIDs that are noise in aggregate, and its query string can carry text a
// person typed. Not headers, not bodies, not the session cookie — see the
// spec's "never logged" list, which this middleware is the main opportunity
// to violate.
//
// # Ordering
//
// Register this **before** middleware.Recoverer, which makes it the outer of
// the two. A panic then reaches Recoverer first, is turned into a 500, and
// unwinds here as an ordinary completed request with a status — so the line
// says 500 rather than the 200 an unwrapped panic would have recorded, and
// there is still exactly one line rather than none.
//
// # It does not decide anything
//
// The middleware reads status and ids and writes a log line. It has no say in
// what a response contains, and in particular it is not a second place where
// debug_reason is decided: that remains the error serializer's, and only its
// (errors.go).
func RequestLogger(log *slog.Logger) func(http.Handler) http.Handler {
	if log == nil {
		log = slog.Default()
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := newRequestID()
			// Set before the handler runs: headers are frozen the moment
			// anything calls WriteHeader, so a response that starts writing
			// immediately would otherwise carry no id at all.
			w.Header().Set(RequestIDHeader, id)

			scope := &logScope{}
			ctx := logging.WithRequestID(r.Context(), id)
			ctx = context.WithValue(ctx, scopeKey{}, scope)
			r = r.WithContext(ctx)

			wrapped := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
			started := time.Now()

			defer func() {
				attrs := []slog.Attr{
					slog.String("method", r.Method),
					slog.String("path", routePattern(r)),
					slog.Int("status", statusOf(wrapped)),
					slog.Int64("duration_ms", time.Since(started).Milliseconds()),
				}
				if scope.userID != "" {
					attrs = append(attrs, slog.String("user_id", scope.userID))
				}
				if scope.storageID != "" {
					attrs = append(attrs, slog.String("storage_id", scope.storageID))
				}
				log.LogAttrs(ctx, levelForStatus(statusOf(wrapped)), "request", attrs...)
			}()

			next.ServeHTTP(wrapped, r)
		})
	}
}

// newRequestID returns a UUIDv7, matching every other id in the system
// (docs/specs/02-data-model.md). A failure to read the system's randomness is
// not worth failing a request over, so a v4 stands in — the id only has to be
// unique enough to correlate lines of one log.
func newRequestID() string {
	if id, err := uuid.NewV7(); err == nil {
		return id.String()
	}
	return uuid.NewString()
}

// routePattern returns the chi route template for this request.
//
// It is only populated once routing has happened, so this must be called after
// the handler has run. A request that matched nothing gets routeUnmatched
// rather than its own URL.
func routePattern(r *http.Request) string {
	rctx := chi.RouteContext(r.Context())
	if rctx == nil {
		return routeUnmatched
	}
	if pattern := rctx.RoutePattern(); pattern != "" {
		return pattern
	}
	return routeUnmatched
}

// statusOf reports the status actually sent. chi's wrapper leaves it zero when
// a handler returned without writing anything, which net/http turns into a
// 200 on the wire — so that is what the log must say.
func statusOf(w middleware.WrapResponseWriter) int {
	if status := w.Status(); status != 0 {
		return status
	}
	return http.StatusOK
}

// levelForStatus implements the spec's level vocabulary: error for a 5xx,
// info for everything else.
//
// A 4xx stays at info deliberately. Those are the system working — a refused
// session, a 404 for a storage somebody may not see — and promoting them to
// warn would make a log of normal operation look like a log of trouble.
func levelForStatus(status int) slog.Level {
	if status >= http.StatusInternalServerError {
		return slog.LevelError
	}
	return slog.LevelInfo
}
