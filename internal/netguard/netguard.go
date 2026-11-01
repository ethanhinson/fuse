// Package netguard holds the one private-address predicate shared by the
// web_fetch permission floor and the loop server's spec policy, so the two
// cannot drift apart. It also holds the one host spelling and the one
// same-host rule that credential-bearing HTTP clients enforce at the transport.
package netguard

import (
	"net"
	"net/url"
	"strings"
)

// CanonicalHost is the one spelling every host rule compares: lower case, no
// trailing root dot.
func CanonicalHost(h string) string { return strings.ToLower(strings.TrimSuffix(h, ".")) }

// MovesHost reports whether a request to next would leave the host a
// credential was bound to for from: a different canonical hostname, or an
// https to http downgrade. Ports are not part of the host.
func MovesHost(from, next *url.URL) bool {
	if CanonicalHost(from.Hostname()) != CanonicalHost(next.Hostname()) {
		return true
	}
	return strings.EqualFold(from.Scheme, "https") && !strings.EqualFold(next.Scheme, "https")
}

var (
	// cgnat is 100.64.0.0/10 (RFC 6598), which includes the 100.100.100.200
	// metadata address some clouds use.
	cgnat = &net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}
	// nat64 is the 64:ff9b::/96 well-known prefix (RFC 6052); its low 32 bits
	// are an embedded IPv4 address.
	nat64 = &net.IPNet{IP: net.ParseIP("64:ff9b::"), Mask: net.CIDRMask(96, 128)}
)

// Private reports whether ip is loopback, RFC-1918/unique-local, link-local,
// unspecified, multicast-local, CGNAT, or a NAT64 or IPv4-mapped form of any of
// those.
func Private(ip net.IP) bool {
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	} else if nat64.Contains(ip) {
		return Private(net.IP(ip[12:16]))
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() || ip.IsInterfaceLocalMulticast() || cgnat.Contains(ip)
}
