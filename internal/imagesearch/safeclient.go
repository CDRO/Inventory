package imagesearch

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"
)

// maxRedirects bounds a candidate's redirect chain. Image CDNs redirect
// routinely, so refusing outright would drop legitimate photos; a handful of
// hops is plenty and stops a redirect loop from tying up a request.
const maxRedirects = 5

// SafeHTTPClient returns a client for fetching URLs that came from somewhere
// outside this system.
//
// # Why this exists
//
// This server fetches whatever a third-party API names, from inside the
// deployment's own network — a NAS that can very likely reach the router's
// admin page, other containers, and on a cloud host the instance metadata
// endpoint. Validating the URL string is not enough on its own, and the gap is
// specific: a perfectly ordinary `https://cdn.example/photo.jpg` passes every
// check and then answers `302 Location: http://169.254.169.254/…`. Go's default
// client follows that without asking anyone.
//
// # Why the check is at dial time
//
// Guarding redirects alone would still leave DNS rebinding: a hostname that
// resolves to a public address when it is validated and to 127.0.0.1 a moment
// later when it is dialled. Checking inside DialContext closes both, because
// the address being checked is the address being connected to — the dial is
// made to the very IP that was just approved, rather than re-resolving the name
// and hoping for the same answer.
//
// CheckRedirect is kept as well, so a hop to a non-HTTP scheme or an endless
// chain fails with a message naming the redirect rather than a bare dial error.
func SafeHTTPClient(timeout time.Duration) *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}

	transport := &http.Transport{
		// No proxy, deliberately — not even from the environment.
		//
		// The guarantee below is "the IP this dials is an IP that was
		// checked". A proxy breaks it at the root: the dial goes to the
		// proxy's address, the proxy is the thing that resolves and connects
		// to the real target, and the check above it inspects the wrong host
		// entirely. With HTTPS_PROXY set, an attacker's redirect to
		// 169.254.169.254 would be honoured by the proxy and this code would
		// never see the address.
		//
		// Nothing here needs a proxy: these are fetches of public images from
		// the open internet. A deployment that genuinely requires one to reach
		// the internet needs this guard rewritten to validate the *target*
		// rather than the connection, which is a different design.
		Proxy: nil,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(address)
			if err != nil {
				return nil, fmt.Errorf("imagesearch: bad address %q: %w", address, err)
			}

			addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
			if err != nil {
				return nil, fmt.Errorf("imagesearch: resolve %q: %w", host, err)
			}
			if len(addrs) == 0 {
				return nil, fmt.Errorf("imagesearch: %q resolved to nothing", host)
			}

			// Every answer is checked, not just the one about to be used: a
			// name that resolves to both a public address and a private one is
			// not a name to fetch from at all.
			for _, addr := range addrs {
				if IsBlockedIP(addr.IP) {
					return nil, fmt.Errorf("imagesearch: refusing to connect to %s (%s)", addr.IP, host)
				}
			}

			// Dial the address that was just checked, by IP. Passing the
			// hostname back to the dialer would resolve it a second time, and
			// the second answer is the one an attacker controls.
			return dialer.DialContext(ctx, network, net.JoinHostPort(addrs[0].IP.String(), port))
		},
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 15 * time.Second,
		MaxIdleConns:          10,
		IdleConnTimeout:       30 * time.Second,
	}

	return &http.Client{
		Timeout:   timeout,
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return fmt.Errorf("imagesearch: stopped after %d redirects", maxRedirects)
			}
			if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
				return fmt.Errorf("imagesearch: refusing redirect to %q", req.URL.Scheme)
			}
			return nil
		},
	}
}

// IsBlockedIP reports whether an address is one this server must never fetch
// from.
//
// The list is everything that is not somewhere on the public internet: the
// loopback interface, RFC 1918 space and its IPv6 equivalent, link-local
// (which is where cloud instance metadata lives, at 169.254.169.254), and the
// unspecified and multicast ranges, which name no single host to fetch from
// anyway.
//
// Blocking by range rather than by a list of known-dangerous addresses is
// deliberate: the interesting targets on a home NAS are not a fixed set. The
// router's admin page, a printer, another container's debug port and the
// metadata endpoint are all just "not the public internet", and enumerating
// them would mean missing whatever the next deployment happens to run.
func IsBlockedIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	// An IPv4-mapped IPv6 address (::ffff:127.0.0.1) must be judged as the
	// IPv4 address it actually is.
	if mapped := ip.To4(); mapped != nil {
		ip = mapped
	}

	switch {
	case ip.IsLoopback(),
		ip.IsPrivate(),
		ip.IsLinkLocalUnicast(),
		ip.IsLinkLocalMulticast(),
		ip.IsInterfaceLocalMulticast(),
		ip.IsMulticast(),
		ip.IsUnspecified():
		return true
	}

	// Carrier-grade NAT (100.64.0.0/10) is not covered by IsPrivate but is
	// just as much "somebody else's internal network" from here.
	if ip4 := ip.To4(); ip4 != nil && ip4[0] == 100 && ip4[1] >= 64 && ip4[1] <= 127 {
		return true
	}

	return false
}
