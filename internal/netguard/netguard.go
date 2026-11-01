// Package netguard holds the one private-address predicate shared by the
// web_fetch permission floor and the loop server's spec policy, so the two
// cannot drift apart.
package netguard

import "net"

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
