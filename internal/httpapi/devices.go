package httpapi

import (
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
type DeviceHandler struct {
	store   AuthStoreFull
	errors  *ErrorWriter
	limiter *rateLimiter
}

// NewDeviceHandler wires the pairing and device routes.
func NewDeviceHandler(s AuthStoreFull, errs *ErrorWriter) *DeviceHandler {
	return &DeviceHandler{store: s, errors: errs, limiter: newRateLimiter(pairAttemptsPerWindow, pairWindow)}
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
	if !h.limiter.allow(clientIP(r)) {
		h.errors.WriteError(w, r, &Failure{
			Status:  http.StatusTooManyRequests,
			Code:    "rate_limited",
			Message: "Too many pairing attempts. Wait a moment and try again.",
			Reason:  "pairing rate limit for " + clientIP(r),
		})
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
	if len(label) > maxDeviceLabel {
		label = label[:maxDeviceLabel]
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
// The session id is deliberately absent. It is a bearer credential, and a list
// endpoint that handed back every one of them would turn read access to this
// page into full account takeover. `current` is what the UI actually needs —
// "which of these is me" — and it is computed server-side by comparing against
// the session that made the request.
type deviceResponse struct {
	Kind       string     `json:"kind"`
	Label      *string    `json:"label"`
	CreatedAt  time.Time  `json:"created_at"`
	LastSeenAt *time.Time `json:"last_seen_at"`
	Current    bool       `json:"current"`
	// RevokeID is the id needed to revoke this session. It is the session id,
	// and it is returned only because revocation needs to name one — see the
	// note above about why the list is otherwise id-free. It is the caller's
	// own session either way.
	RevokeID string `json:"revoke_id"`
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
			Kind:       string(s.Kind),
			Label:      s.Label,
			CreatedAt:  s.CreatedAt,
			LastSeenAt: s.LastSeenAt,
			Current:    current != nil && s.ID == current.ID,
			RevokeID:   s.ID,
		})
	}

	writeJSON(w, http.StatusOK, collection[deviceResponse]{Items: out})
}

// RevokeDevice serves DELETE /api/auth/devices/{session_id}.
//
// A user may revoke only their own sessions. Another user's session id gets
// the same 404 as one that never existed — the same non-enumeration rule that
// governs storages and the admin area.
func (h *DeviceHandler) RevokeDevice(w http.ResponseWriter, r *http.Request) {
	user, ok := UserFrom(r.Context())
	if !ok {
		h.errors.WriteError(w, r, Unauthorized(ReasonSessionMissing))
		return
	}

	target := chi.URLParam(r, "session_id")

	// Ownership is checked by listing the caller's own sessions rather than by
	// deleting and inspecting the row count: a delete-then-check would have
	// removed somebody else's session before noticing it was not ours.
	sessions, err := h.store.UserSessions(r.Context(), user.ID)
	if err != nil {
		h.errors.WriteError(w, r, Internal(err))
		return
	}

	owned := false
	for _, s := range sessions {
		if s.ID == target {
			owned = true
			break
		}
	}
	if !owned {
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
