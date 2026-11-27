package admin

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// cloudflareClientIPHeader is set by Cloudflare's edge on every proxied
// request. The edge overwrites any client-supplied value, so it is reliable
// only when the direct peer is the local cloudflared connector.
const cloudflareClientIPHeader = "CF-Connecting-IP"

// TrustCloudflareClientIP makes the management server take the client address
// from CF-Connecting-IP, but only for requests whose direct peer is a loopback
// address (a cloudflared connector on the same host). Without it every
// tunneled visitor shares 127.0.0.1, so five failed logins from anywhere lock
// the operator out as well. Call before serving requests. Any local process
// can still connect to the loopback listener and choose this header; local
// processes are trusted in that deployment. Off by default.
func (a *Server) TrustCloudflareClientIP() { a.trustCloudflareIP = true }

// clientIP returns the address used for the admin rate limits and audit logs.
// By default it is the direct peer and deliberately ignores X-Forwarded-For:
// the header is client-controlled, so trusting it would let anyone escape the
// login and Playground limits by rotating a value. See TrustCloudflareClientIP
// for the only forwarded header that may replace it.
func (a *Server) clientIP(r *http.Request) string {
	peer := peerIP(r)
	if !a.trustCloudflareIP {
		return peer
	}
	if address, err := netip.ParseAddr(peer); err != nil || !address.Unmap().IsLoopback() {
		return peer
	}
	// Exactly one well-formed value; otherwise stay in the shared loopback
	// bucket, which is stricter, never looser.
	values := r.Header.Values(cloudflareClientIPHeader)
	if len(values) != 1 {
		return peer
	}
	address, err := netip.ParseAddr(strings.TrimSpace(values[0]))
	if err != nil || address.Zone() != "" {
		return peer
	}
	return address.Unmap().String()
}

// peerIP returns the IP address of the direct peer, without the port.
func peerIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	return r.RemoteAddr
}

// rateLimitKey groups IPv6 clients by their /64: one subscriber usually owns a
// whole /64 and could otherwise rotate addresses to reset every per-IP limit.
// IPv4 and unparsable values are used as is.
func rateLimitKey(client string) string {
	address, err := netip.ParseAddr(client)
	if err != nil {
		return client
	}
	address = address.Unmap()
	if address.Is4() {
		return address.String()
	}
	prefix, err := address.WithZone("").Prefix(64)
	if err != nil {
		return client
	}
	return prefix.String()
}
