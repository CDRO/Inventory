package httpapi

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Credential rate limiting (docs/specs/14-account-self-service.md).
//
// **One limiter covers POST /api/auth/login, POST /api/auth/pair and
// POST /api/auth/password**, not one per endpoint. Three separate counters
// would each have to be exhausted separately, so ten guesses per endpoint is
// thirty guesses against the same account from the same address — and the
// three endpoints are interchangeable to an attacker, because all three take
// a secret and say whether it was right.
//
// argon2id is the other half of the reason. Every guess costs this server
// 19 MiB and a couple of hundred milliseconds, on a NAS the household also
// relies on for everything else, so cutting guessing off early protects the
// box as much as the account.
const (
	// credentialFailuresPerWindow is how many failed attempts a key may
	// accumulate before the next one is refused. The eleventh within the
	// window is the first to get a 429.
	credentialFailuresPerWindow = 10

	// credentialWindow is the fixed window those failures are counted in.
	credentialWindow = 15 * time.Minute
)

// credentialKeys are the counters one credential attempt is charged against:
// the caller's address always, and the submitted username when there is one.
//
// Counting per username as well as per address is what stops a botnet from
// spreading ten guesses per address across a thousand addresses. The username
// is counted **whether or not it exists** — a limiter that only counted real
// usernames would answer "429" for real accounts and "401" for imaginary
// ones, turning the defence itself into the user enumeration that
// docs/specs/03-auth-and-multi-tenancy.md exists to prevent.
func credentialKeys(ip, username string) []string {
	keys := make([]string, 0, 2)
	if ip != "" {
		keys = append(keys, "ip:"+ip)
	}
	if username != "" {
		keys = append(keys, "user:"+username)
	}
	return keys
}

// rateLimiter is a fixed-window counter, keyed by caller.
//
// Deliberately in-memory and deliberately simple. This is abuse damping on a
// single-process household server (docs/specs/01-architecture-and-deployment.md),
// so there is no coordination problem to solve and a table plus a sweep would
// be machinery for a deployment shape this project does not have. A restart
// clears the counters, which docs/specs/14-account-self-service.md calls out
// as an accepted trade rather than an oversight.
//
// Two counting styles share the type. The credential endpoints count
// **failures**: blocked before the work, record after a refusal, reset after a
// success. Minting a pairing code counts **attempts**, through allow, because
// there is no such thing as a failed mint to count.
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

// blocked reports whether any of these keys has already reached the limit, and
// how long until the earliest-freeing of the blocked ones opens up.
//
// It counts nothing. Checking before doing the expensive work is what keeps a
// caller who is already over the limit from costing an argon2 verification per
// request, and it is why the refusal happens without the attempt being charged
// a second time.
func (l *rateLimiter) blocked(keys ...string) (time.Duration, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.blockedLocked(time.Now(), keys)
}

// record charges one failed attempt against every key.
func (l *rateLimiter) record(keys ...string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.recordLocked(time.Now(), keys)
}

// reset clears every key a success matched.
//
// docs/specs/14-account-self-service.md: "A success resets the counters it
// matched." Someone who mistypes their password nine times and then gets it
// right is not mid-attack, and leaving the count standing would lock them out
// of their own next login attempt.
func (l *rateLimiter) reset(keys ...string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, key := range keys {
		delete(l.windows, key)
	}
}

// allow records an attempt and reports whether it was within the limit — the
// attempt-counting style, for an endpoint with no failure to count. The
// duration is how long until the window frees, meaningful only when the
// attempt was refused.
//
// One lock for both halves, so two concurrent callers cannot both read a
// count below the limit and then both increment past it.
func (l *rateLimiter) allow(key string) (time.Duration, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	keys := []string{key}
	retryAfter, blocked := l.blockedLocked(now, keys)
	l.recordLocked(now, keys)
	return retryAfter, !blocked
}

// blockedLocked is blocked's body; the caller holds the mutex.
func (l *rateLimiter) blockedLocked(now time.Time, keys []string) (time.Duration, bool) {
	var retryAfter time.Duration
	found := false
	for _, key := range keys {
		w, ok := l.windows[key]
		if !ok || now.Sub(w.start) >= l.window || w.count < l.limit {
			// An elapsed window is not a block: it is about to be replaced by
			// a fresh one on the next record.
			continue
		}
		found = true
		if left := l.window - now.Sub(w.start); left > retryAfter {
			retryAfter = left
		}
	}
	return retryAfter, found
}

// recordLocked is record's body; the caller holds the mutex.
func (l *rateLimiter) recordLocked(now time.Time, keys []string) {
	// Swept on every record rather than only when a new window opens: without
	// it a long-running server accumulates one map entry per address that has
	// ever failed, and those entries are never revisited.
	l.sweep(now)
	for _, key := range keys {
		w, ok := l.windows[key]
		if !ok || now.Sub(w.start) >= l.window {
			l.windows[key] = &rateWindow{count: 1, start: now}
			continue
		}
		w.count++
	}
}

// sweep drops elapsed windows. The caller holds the mutex.
func (l *rateLimiter) sweep(now time.Time) {
	for key, w := range l.windows {
		if now.Sub(w.start) >= l.window {
			delete(l.windows, key)
		}
	}
}

// rateLimited is the refusal every rate-limited endpoint shares — the 429
// row docs/specs/04-backend-api-conventions.md's status table gained for
// docs/specs/14-account-self-service.md.
//
// Retry-After rides on the Failure rather than being set on the
// ResponseWriter here, so the header is emitted by the one error serializer
// alongside the body it belongs to (errors.go).
func rateLimited(retryAfter time.Duration, reason string) *Failure {
	return &Failure{
		Status:     http.StatusTooManyRequests,
		Code:       CodeRateLimited,
		Message:    "Too many attempts. Wait a moment and try again.",
		Reason:     reason,
		RetryAfter: retryAfter,
	}
}

// The forwarded-address headers Traefik sets. Read only from a peer on the
// internal network — see TrustedRealIP.
const (
	headerForwardedFor = "X-Forwarded-For"
	headerRealIP       = "X-Real-IP"
)

// TrustedRealIP resolves the caller's address into r.RemoteAddr, honouring
// the proxy's forwarded headers **only when the connection peer is on the
// internal network**.
//
// This replaces chi's middleware.RealIP, which rewrites RemoteAddr from
// X-Forwarded-For or X-Real-IP whoever the peer is. That is fine for logging
// and fatal for a rate limiter: any client can send its own X-Forwarded-For,
// so a limiter keyed on the result counts a value the attacker chooses, and
// ten guesses per address becomes unlimited guesses with a counter that
// resets on demand. docs/specs/14-account-self-service.md requires the
// address to come "from the connection, or from the Traefik-supplied
// X-Forwarded-For only when the connection peer is the internal network —
// never from the header alone".
//
// **The rightmost X-Forwarded-For entry wins, not the leftmost.** The list
// reads oldest-to-newest, so any entry a client sent itself appears to the
// left of the one the trusted proxy observed. chi takes the leftmost, which
// is precisely the forgeable one; taking the rightmost means the address is
// the one Traefik actually saw whether it replaced the header (its default,
// with no trustedIPs configured) or appended to it.
func TrustedRealIP(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if forwarded := forwardedClientIP(r); forwarded != "" {
			r.RemoteAddr = forwarded
		}
		next.ServeHTTP(w, r)
	})
}

// forwardedClientIP returns the address the proxy reports, or "" when there is
// no trustworthy one — an untrusted peer, an absent header, or a value that is
// not an IP address at all.
func forwardedClientIP(r *http.Request) string {
	if !isInternalPeer(r.RemoteAddr) {
		return ""
	}

	// X-Forwarded-For first and X-Real-IP only as a fallback, deliberately:
	// XFF is the header that carries the whole chain, so it is the only one
	// whose rightmost entry can be reasoned about. X-Real-IP is a single
	// value with no such structure, useful only when XFF is absent.
	if chain := r.Header.Get(headerForwardedFor); chain != "" {
		hops := strings.Split(chain, ",")
		if candidate := strings.TrimSpace(hops[len(hops)-1]); net.ParseIP(candidate) != nil {
			return candidate
		}
	}
	if candidate := strings.TrimSpace(r.Header.Get(headerRealIP)); net.ParseIP(candidate) != nil {
		return candidate
	}
	return ""
}

// isInternalPeer reports whether the TCP peer is something that could be this
// deployment's own reverse proxy.
//
// Traefik reaches the app over the Compose-managed bridge network
// (docs/specs/01-architecture-and-deployment.md), so the peer is an RFC 1918
// address; loopback covers a direct local run. Anything else — including a
// Tailscale 100.64.0.0/10 address, which is a *client*, never the proxy — is
// an outside caller whose headers say nothing.
func isInternalPeer(remoteAddr string) bool {
	ip := net.ParseIP(hostOf(remoteAddr))
	if ip == nil {
		return false
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()
}

// clientIP is the address the rate limiters count against: the host half of
// r.RemoteAddr, as TrustedRealIP has resolved it.
func clientIP(r *http.Request) string {
	return hostOf(r.RemoteAddr)
}

// hostOf strips the port from an address, tolerating one that has none.
//
// TrustedRealIP leaves a bare IP behind when it rewrites RemoteAddr, and a
// bare IPv6 address contains colons of its own — so cutting at the last colon
// would turn "2001:db8::1" into "2001:db8:", quietly keying the limiter on a
// value that is not an address.
func hostOf(addr string) string {
	if host, _, err := net.SplitHostPort(addr); err == nil {
		return host
	}
	return strings.Trim(addr, "[]")
}
