// Package logging owns the application's structured-logging setup
// (docs/specs/18-operations-and-observability.md).
//
// For a system one person operates on a NAS, `docker compose logs app` *is*
// the observability stack. So: JSON through log/slog, to stdout, and nothing
// else. The application writes no log files and rotates nothing — Docker's log
// driver does retention, and a second copy on a volume is a second thing to
// fill the disk.
//
// # Why a handler wrapper rather than a logger per request
//
// The spec requires that the request id appear "on every other line logged
// during that request", not only on the completion line. Threading a
// *slog.Logger through every constructor to achieve that would mean every
// background service, store helper and provider client taking one — and the
// single call site that kept using slog.Default() would silently drop out of
// the trace with nothing failing.
//
// Handler does it from the other end. Every slog call already carries a
// context (the *Context variants) or can be given one, so a handler that reads
// the request id out of the context attaches it to whatever was logged, from
// wherever, with no plumbing. The cost is that a log call made with the
// non-context variant — slog.Warn rather than slog.WarnContext — has no
// context to read and so loses the id. That is a real trap, and the reason the
// request-path call sites in this repository use the *Context forms.
package logging

import (
	"context"
	"io"
	"log/slog"
)

type ctxKey int

const ctxRequestID ctxKey = iota

// WithRequestID returns a context carrying the id of the request being
// handled. The HTTP middleware is its only caller.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxRequestID, id)
}

// RequestIDFrom returns the request id in ctx, or "" outside a request.
func RequestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(ctxRequestID).(string)
	return id
}

// New returns the application logger: JSON records at info and above, written
// to w.
//
// Info is the floor because the spec's own level vocabulary starts there —
// info for requests and lifecycle events, warn for degraded-but-handled, error
// for 5xx and failed jobs. Nothing in the system logs at debug, so there is no
// level knob to get wrong in a deployment.
func New(w io.Writer) *slog.Logger {
	return slog.New(&Handler{inner: slog.NewJSONHandler(w, &slog.HandlerOptions{Level: slog.LevelInfo})})
}

// Handler adds the context's request id to every record that has one.
//
// It is exported so a test can wrap its own handler and still observe the same
// enrichment the production logger applies — a "nothing secret is logged" test
// that inspected records the real handler would have decorated differently
// would be testing the wrong pipeline.
type Handler struct {
	inner slog.Handler
}

// NewHandler wraps h so that records logged with a request-scoped context
// carry request_id.
func NewHandler(h slog.Handler) *Handler { return &Handler{inner: h} }

func (h *Handler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

func (h *Handler) Handle(ctx context.Context, record slog.Record) error {
	if id := RequestIDFrom(ctx); id != "" {
		record.AddAttrs(slog.String("request_id", id))
	}
	return h.inner.Handle(ctx, record)
}

// WithAttrs and WithGroup re-wrap rather than returning the inner handler.
//
// Returning slog.Handler from the embedded handler directly — which is what
// embedding it and forgetting these two would do — quietly unwraps the
// enrichment: every logger derived with slog.With() would stop attaching
// request ids, and only the loggers nobody derived would still work.
func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &Handler{inner: h.inner.WithAttrs(attrs)}
}

func (h *Handler) WithGroup(name string) slog.Handler {
	return &Handler{inner: h.inner.WithGroup(name)}
}
