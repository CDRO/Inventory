package httpapi

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/CDRO/Inventory/internal/store"
)

// Pairing limits (docs/specs/03-auth-and-multi-tenancy.md).
const (
	// maxDeviceLabel bounds the label a pairing client may choose. It is shown
	// back to the user in their device list, so it is their text, not a
	// protocol field — but it is still typed by whoever holds the code.
	maxDeviceLabel = 64

	// pairAttemptsPerWindow and pairWindow throttle POST /api/auth/pair, the
	// one unauthenticated endpoint that can mint a session.
	//
	// A pairing code is 256 CSPRNG bits, so guessing one is not the threat this
	// defends against — it is there so a stolen or shoulder-surfed code cannot
	// be brute-forced for variants, and so this endpoint cannot be used to
	// hammer the database from an unauthenticated position.
	pairAttemptsPerWindow = 10
	pairWindow            = time.Minute
)

// DeviceHandler serves pairing and device management.
//
// Pairing is rate-limited "per IP and per user", as spec 03 puts it, and the
// two halves necessarily sit on different endpoints. POST /api/auth/pair is
// unauthenticated — until a code is redeemed there is no user to count
// against — so it is limited by address. The user-side limit lives where the
// user is known: minting codes, since every outstanding code is a live way into
// that account for the next two minutes.
type DeviceHandler struct {
	store   AuthStoreFull
	errors  *ErrorWriter
	limiter *rateLimiter
}

// NewDeviceHandler wires the pairing and device routes.
func NewDeviceHandler(s AuthStoreFull, errs *ErrorWriter) *DeviceHandler {
	return &DeviceHandler{store: s, errors: errs, limiter: newRateLimiter(pairAttemptsPerWindow, pairWindow)}
}

// rateLimited is the refusal both pairing limits share.
func rateLimited(reason string) *Failure {
	return &Failure{
		Status:  http.StatusTooManyRequests,
		Code:    "rate_limited",
		Message: "Too many pairing attempts. Wait a moment and try again.",
		Reason:  reason,
	}
}

// CreatePairingCode serves POST /api/auth/pairing-codes.
//
// The response carries the code and this server's base URL, because the client
// scanning the QR cannot guess where to send it: the server is self-hosted at
// a Tailscale address, not a well-known host.
//
// **The QR never carries a session id** — only a single-use code that dies in
// two minutes. A session id in a QR would turn a photograph of a monitor, or a
// screenshot pasted into a chat, into a permanent credential.
func (h *DeviceHandler) CreatePairingCode(w http.ResponseWriter, r *http.Request) {
	user, ok := UserFrom(r.Context())
	if !ok {
		h.errors.WriteError(w, r, Unauthorized(ReasonSessionMissing))
		return
	}

	// Keyed apart from the address keys Pair uses, so one cannot exhaust the
	// other.
	if !h.limiter.allow("user:" + user.ID.String()) {
		h.errors.WriteError(w, r, rateLimited("pairing-code rate limit for user "+user.ID.String()))
		return
	}

	code, err := h.store.CreatePairingCode(r.Context(), user.ID)
	if err != nil {
		h.errors.WriteError(w, r, Internal(err))
		return
	}

	writeJSON(w, http.StatusCreated, struct {
		Code    string `json:"code"`
		BaseURL string `json:"base_url"`
	}{Code: code, BaseURL: baseURLOf(r)})
}

// Pair serves POST /api/auth/pair — the one unauthenticated route that mints a
// session.
//
// The session id comes back in the response body rather than as a cookie: the
// caller is a native client, which cannot hold an HttpOnly browser cookie and
// will send the id as a Bearer header from here on.
func (h *DeviceHandler) Pair(w http.ResponseWriter, r *http.Request) {
	if !h.limiter.allow("ip:" + clientIP(r)) {
		h.errors.WriteError(w, r, rateLimited("pairing rate limit for "+clientIP(r)))
		return
	}

	var body struct {
		Code        string `json:"code"`
		DeviceLabel string `json:"device_label"`
	}
	if failure := decodeJSON(w, r, &body); failure != nil {
		h.errors.WriteError(w, r, failure)
		return
	}

	label := strings.TrimSpace(body.DeviceLabel)
	if label == "" {
		label = "Paired device"
	}
	// Truncated by character, not byte: a byte cut can land inside a
	// multi-byte character and store a label that is not valid UTF-8.
	if runes := []rune(label); len(runes) > maxDeviceLabel {
		label = string(runes[:maxDeviceLabel])
	}

	userID, err := h.store.RedeemPairingCode(r.Context(), body.Code)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrConflict) {
			// Expired, already used, and never existed are one answer. Telling
			// them apart would say whether a code was ever real, which is the
			// only thing an attacker holding a guess wants to know.
			h.errors.WriteError(w, r, &Failure{
				Status:  http.StatusUnauthorized,
				Code:    CodeUnauthorized,
				Message: "That pairing code is not valid.",
				Reason:  "pairing code rejected: " + err.Error(),
			})
			return
		}
		h.errors.WriteError(w, r, Internal(err))
		return
	}

	session, err := h.store.CreateSession(r.Context(), userID, store.SessionDevice, &label, DeviceSessionTTL)
	if err != nil {
		h.errors.WriteError(w, r, Internal(err))
		return
	}

	writeJSON(w, http.StatusCreated, struct {
		SessionID string    `json:"session_id"`
		Label     string    `json:"label"`
		ExpiresAt time.Time `json:"expires_at"`
	}{SessionID: session.ID, Label: label, ExpiresAt: session.ExpiresAt})
}

// deviceResponse is one of the caller's own sessions.
//
// **The session id is never in this response.** sessions.id is not a row
// identifier that happens to be random — it *is* the bearer token. A list that
// returned it would hand every live credential the user holds, a paired phone's
// year-long one included, to any script that can read this page, so reading
// the device list would be the same as stealing every device on it.
//
// ID is a handle derived from the token instead (see deviceHandle): stable, so
// the client can name a session to revoke, and one-way, so holding it grants
// nothing. `current` is computed server-side against the session making the
// request.
type deviceResponse struct {
	ID         string     `json:"id"`
	Kind       string     `json:"kind"`
	Label      *string    `json:"label"`
	CreatedAt  time.Time  `json:"created_at"`
	LastSeenAt *time.Time `json:"last_seen_at"`
	Current    bool       `json:"current"`
}

// deviceHandle is the public name of a session: the hex SHA-256 of its token.
//
// Tokens are 256 CSPRNG bits, so there is nothing to brute-force through the
// hash, and no salt is needed. A handle is only ever resolved by matching it
// against the caller's own sessions, so knowing someone else's handle does not
// reach their session either.
func deviceHandle(sessionID string) string {
	sum := sha256.Sum256([]byte(sessionID))
	return hex.EncodeToString(sum[:])
}

// ListDevices serves GET /api/auth/devices.
func (h *DeviceHandler) ListDevices(w http.ResponseWriter, r *http.Request) {
	user, ok := UserFrom(r.Context())
	if !ok {
		h.errors.WriteError(w, r, Unauthorized(ReasonSessionMissing))
		return
	}
	current, _ := SessionFrom(r.Context())

	sessions, err := h.store.UserSessions(r.Context(), user.ID)
	if err != nil {
		h.errors.WriteError(w, r, Internal(err))
		return
	}

	out := make([]deviceResponse, 0, len(sessions))
	for _, s := range sessions {
		out = append(out, deviceResponse{
			ID:         deviceHandle(s.ID),
			Kind:       string(s.Kind),
			Label:      s.Label,
			CreatedAt:  s.CreatedAt,
			LastSeenAt: s.LastSeenAt,
			Current:    current != nil && s.ID == current.ID,
		})
	}

	writeJSON(w, http.StatusOK, collection[deviceResponse]{Items: out})
}

// RevokeDevice serves DELETE /api/auth/devices/{session_id}.
//
// The path segment is the device's `id` from the list — its handle, not the
// token (see deviceResponse). A user may revoke only their own sessions:
// another user's handle, or one that names nothing, gets the same 404 — the
// non-enumeration rule that governs storages and the admin area.
func (h *DeviceHandler) RevokeDevice(w http.ResponseWriter, r *http.Request) {
	user, ok := UserFrom(r.Context())
	if !ok {
		h.errors.WriteError(w, r, Unauthorized(ReasonSessionMissing))
		return
	}

	handle := chi.URLParam(r, "session_id")

	// Resolved only against the caller's own sessions, which is both the
	// ownership check and the only way a handle becomes a token. Checking
	// before deleting matters: a delete-then-check would have removed somebody
	// else's session before noticing it was not ours.
	sessions, err := h.store.UserSessions(r.Context(), user.ID)
	if err != nil {
		h.errors.WriteError(w, r, Internal(err))
		return
	}

	target := ""
	for _, s := range sessions {
		if deviceHandle(s.ID) == handle {
			target = s.ID
			break
		}
	}
	if target == "" {
		h.errors.WriteError(w, r, NotFound("session not owned by caller or nonexistent"))
		return
	}

	if err := h.store.DeleteSession(r.Context(), target); err != nil {
		h.errors.WriteError(w, r, Internal(err))
		return
	}

	writeJSON(w, http.StatusNoContent, nil)
}

// baseURLOf reconstructs this server's externally visible base URL.
//
// Traefik terminates TLS and forwards over plain HTTP, so r.TLS is nil even
// when the client used https. X-Forwarded-Proto is what carries the truth, and
// getting it wrong would put an http:// URL in a QR code that a phone then
// cannot reach over a Tailscale HTTPS address.
func baseURLOf(r *http.Request) string {
	scheme := "https"
	if forwarded := r.Header.Get("X-Forwarded-Proto"); forwarded != "" {
		scheme = forwarded
	} else if r.TLS == nil {
		scheme = "http"
	}

	host := r.Header.Get("X-Forwarded-Host")
	if host == "" {
		host = r.Host
	}
	return scheme + "://" + host
}

// clientIP is the address the rate limiter counts against.
//
// chi's RealIP middleware has already normalised X-Forwarded-For into
// RemoteAddr by the time a handler runs, so this reads the result rather than
// re-parsing headers and reaching a different answer.
func clientIP(r *http.Request) string {
	host := r.RemoteAddr
	if idx := strings.LastIndex(host, ":"); idx > 0 {
		host = host[:idx]
	}
	return host
}

// rateLimiter is a fixed-window counter, keyed by caller.
//
// Deliberately in-memory and deliberately simple: this guards one
// unauthenticated endpoint on a single-process household server, and a
// distributed limiter would be machinery for a deployment shape this project
// does not have (docs/specs/01-architecture-and-deployment.md). Restarting the
// server clears the counters, which is acceptable for the same reason.
type rateLimiter struct {
	mu      sync.Mutex
	limit   int
	window  time.Duration
	windows map[string]*rateWindow
}

type rateWindow struct {
	count int
	start time.Time
}

func newRateLimiter(limit int, window time.Duration) *rateLimiter {
	return &rateLimiter{limit: limit, window: window, windows: map[string]*rateWindow{}}
}

// allow records an attempt and reports whether it is within the limit.
func (l *rateLimiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	w, ok := l.windows[key]
	if !ok || now.Sub(w.start) > l.window {
		l.windows[key] = &rateWindow{count: 1, start: now}
		l.sweep(now)
		return true
	}

	w.count++
	return w.count <= l.limit
}

// sweep drops windows that have expired, so a long-running server does not
// accumulate one map entry per address that ever tried to pair.
func (l *rateLimiter) sweep(now time.Time) {
	for key, w := range l.windows {
		if now.Sub(w.start) > l.window {
			delete(l.windows, key)
		}
	}
}
