package httpapi

import "net/http"

// ClientVersionHeader is what a native client names itself with,
// `<name>/<version>` (docs/specs/12-client-api-contract.md).
const ClientVersionHeader = "X-Client-Version"

// maxClientVersionLog bounds what is copied into a log line. The header is
// attacker-controlled in the ordinary sense that any client sets it, and an
// unbounded string would let one client make every log entry about a failed
// request as large as it liked. It is not a sanitizer: slog quotes the value,
// and this is a household inventory server, not a log-ingestion pipeline.
const maxClientVersionLog = 128

// clientVersionFrom reads the client's self-reported version.
//
// **This is read in exactly one place — WriteError, for the log line.** The
// spec's rule is that the header is for diagnostics only and must never gate
// behavior, and structure is how that rule is kept rather than discipline:
// the one function that sees this header cannot change a status, a body, or a
// query, because all it does is produce a response that was already decided.
// Version-sniffing to alter a response is how one API quietly becomes several,
// and there is nowhere in this package to write that code.
//
// It is deliberately not request-logging middleware either. Logging every
// request belongs to docs/specs/18-operations-logging-audit-upgrades.md; until
// that ships, the moment a client's version actually matters to anyone is the
// moment its request failed, and that is where it is recorded.
func clientVersionFrom(r *http.Request) string {
	if r == nil {
		return ""
	}
	version := r.Header.Get(ClientVersionHeader)
	if len(version) > maxClientVersionLog {
		return version[:maxClientVersionLog]
	}
	return version
}
