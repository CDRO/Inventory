package httpapi

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/CDRO/Inventory/internal/store"
)

// Error codes. Stable, machine-readable, snake_case: the frontend switches on
// these, so renaming one is a breaking API change.
const (
	CodeUnauthorized     = "unauthorized"
	CodeNotFound         = "not_found"
	CodeConflict         = "conflict"
	CodeValidationFailed = "validation_failed"
	CodePayloadTooLarge  = "payload_too_large"
	CodeModelUnavailable = "model_unavailable"
	CodeInternal         = "internal_error"
)

// debug_reason values, named in docs/specs/03-auth-and-multi-tenancy.md.
const (
	ReasonSessionMissing   = "session_missing"
	ReasonSessionExpired   = "session_expired"
	ReasonNotStorageMember = "not_storage_member"
	ReasonStorageNotFound  = "storage_not_found"
	ReasonNotAdmin         = "not_admin"
	ReasonAdminAreaHidden  = "admin_area_hidden"
)

// APIError is what a caller sees. It is unexported-by-convention in the sense
// that nothing constructs one directly: WriteError is the only writer.
type APIError struct {
	Code    string              `json:"code"`
	Message string              `json:"message"`
	Fields  map[string][]string `json:"fields,omitempty"`
	// DebugReason names the exact check that failed. It is populated in
	// exactly one place — WriteError, and only when the environment is dev.
	DebugReason string `json:"debug_reason,omitempty"`
}

type errorEnvelope struct {
	Error APIError `json:"error"`
}

// Failure is an error destined for a client: a status, a stable code, and the
// internal reason that must never reach production output.
//
// Handlers build one of these (usually via the helpers below) and hand it to
// WriteError. They do not decide whether Reason is disclosed — that is the
// serializer's job, and taking the decision away from the handler is the whole
// design.
type Failure struct {
	Status  int
	Code    string
	Message string
	Fields  map[string][]string
	// Reason is the internal explanation. It is always safe to set: whether it
	// is ever serialized depends on the environment, not on the caller.
	Reason string
	// Err is the underlying cause. It is logged, never serialized.
	Err error
}

func (f *Failure) Error() string {
	if f.Err != nil {
		return f.Code + ": " + f.Err.Error()
	}
	return f.Code
}

func (f *Failure) Unwrap() error { return f.Err }

// ErrorWriter serializes every error the API returns.
//
// **This is the only place an error envelope is produced, and the only place
// debug_reason can be attached.** The gating lives here rather than in the
// handlers for a specific reason: a rule that each handler has to remember is
// a rule that gets forgotten, and forgetting it leaks the name of an internal
// authorization check to an anonymous caller in production. A handler cannot
// make that mistake because it has no way to write an error envelope itself —
// it can only describe the failure and hand it over.
//
// Construct one with NewErrorWriter so the dev flag is read once, from the
// configuration, rather than from the environment at each call.
type ErrorWriter struct {
	// dev mirrors config.Config.IsDev(). It is a plain bool, fixed at
	// construction: reading APP_ENV per request would make the disclosure
	// rule depend on process state that could change under a running server.
	dev bool
	log *slog.Logger
}

// NewErrorWriter returns the serializer. dev must come from
// config.Config.IsDev(); nothing else may decide it.
func NewErrorWriter(dev bool, log *slog.Logger) *ErrorWriter {
	if log == nil {
		log = slog.Default()
	}
	return &ErrorWriter{dev: dev, log: log}
}

// WriteError renders a Failure as the API's one error shape.
//
// In production the response carries only code, message and — for validation
// failures — the field map. The reason is written to the server log instead,
// where an operator can see it and a caller cannot.
func (w *ErrorWriter) WriteError(rw http.ResponseWriter, r *http.Request, failure *Failure) {
	if failure == nil {
		failure = &Failure{Status: http.StatusInternalServerError, Code: CodeInternal, Message: "Internal error."}
	}
	if failure.Status == 0 {
		failure.Status = http.StatusInternalServerError
	}
	if failure.Code == "" {
		failure.Code = CodeInternal
	}
	if failure.Message == "" {
		failure.Message = defaultMessage(failure.Code)
	}

	// The reason always reaches the log, in both environments. Production
	// hides it from the response, not from the operator.
	if failure.Reason != "" || failure.Err != nil {
		w.log.LogAttrs(r.Context(), slog.LevelInfo, "request failed",
			slog.Int("status", failure.Status),
			slog.String("code", failure.Code),
			slog.String("reason", failure.Reason),
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Any("err", failure.Err),
		)
	}

	body := errorEnvelope{Error: APIError{
		Code:    failure.Code,
		Message: failure.Message,
		Fields:  failure.Fields,
	}}

	// The single gate. Everything above is identical in both environments.
	if w.dev {
		body.Error.DebugReason = failure.Reason
	}

	rw.Header().Set("Content-Type", "application/json; charset=utf-8")
	rw.Header().Set("Cache-Control", "no-store")
	rw.WriteHeader(failure.Status)
	// An encode failure here means the client hung up; there is nothing left
	// to tell them.
	_ = json.NewEncoder(rw).Encode(body)
}

// NotFound is the answer to an unknown resource **and** to one the caller may
// not see.
//
// The two callers pass different reasons, which is exactly as much difference
// as the system permits: the reason is visible in dev and in the log, and the
// response a production client receives is byte-identical either way. A `403`
// here, or a message that named which case applied, would confirm that an id
// refers to a real row and let someone map out households they cannot see
// (docs/specs/03-auth-and-multi-tenancy.md).
func NotFound(reason string) *Failure {
	return &Failure{
		Status:  http.StatusNotFound,
		Code:    CodeNotFound,
		Message: "Not found.",
		Reason:  reason,
	}
}

// Unauthorized is a missing or expired session.
func Unauthorized(reason string) *Failure {
	return &Failure{
		Status:  http.StatusUnauthorized,
		Code:    CodeUnauthorized,
		Message: "Authentication required.",
		Reason:  reason,
	}
}

// Conflict is a legal resource in a state that forbids the action.
func Conflict(message string, err error) *Failure {
	if message == "" {
		message = "That action conflicts with the current state."
	}
	return &Failure{Status: http.StatusConflict, Code: CodeConflict, Message: message, Err: err}
}

// ValidationFailed carries the per-field messages a form needs.
func ValidationFailed(fields map[string][]string, err error) *Failure {
	return &Failure{
		Status:  http.StatusUnprocessableEntity,
		Code:    CodeValidationFailed,
		Message: "The request could not be processed.",
		Fields:  fields,
		Err:     err,
	}
}

// PayloadTooLarge is an upload past the hard cap.
func PayloadTooLarge() *Failure {
	return &Failure{
		Status:  http.StatusRequestEntityTooLarge,
		Code:    CodePayloadTooLarge,
		Message: "The uploaded file is too large.",
	}
}

// ModelUnavailable is the configured vision model no longer being offered.
func ModelUnavailable(model string) *Failure {
	return &Failure{
		Status:  http.StatusServiceUnavailable,
		Code:    CodeModelUnavailable,
		Message: "The configured vision model is unavailable.",
		Reason:  "configured model not offered by the provider: " + model,
	}
}

// Internal is an unexpected failure. The cause is logged, never serialized.
func Internal(err error) *Failure {
	return &Failure{
		Status:  http.StatusInternalServerError,
		Code:    CodeInternal,
		Message: "Internal error.",
		Err:     err,
	}
}

// FromStoreError maps the store's domain errors onto the status codes in
// docs/specs/04-backend-api-conventions.md.
//
// store.ErrNotFound already covers both "no such row" and "a row in another
// storage", so this mapping cannot reintroduce the distinction the store
// deliberately dropped.
func FromStoreError(err error, reason string) *Failure {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, store.ErrNotFound):
		return NotFound(reason)
	case errors.Is(err, store.ErrConflict):
		return Conflict(err.Error(), err)
	case errors.Is(err, store.ErrValidation):
		return ValidationFailed(nil, err)
	default:
		return Internal(err)
	}
}

func defaultMessage(code string) string {
	switch code {
	case CodeUnauthorized:
		return "Authentication required."
	case CodeNotFound:
		return "Not found."
	case CodeConflict:
		return "That action conflicts with the current state."
	case CodeValidationFailed:
		return "The request could not be processed."
	case CodePayloadTooLarge:
		return "The uploaded file is too large."
	case CodeModelUnavailable:
		return "The configured vision model is unavailable."
	default:
		return "Internal error."
	}
}
