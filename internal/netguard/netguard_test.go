package netguard

import (
	"net"
	"testing"
)

func TestPrivate(t *testing.T) {
	for _, s := range []string{"127.0.0.1", "10.0.0.5", "172.16.0.1", "192.168.1.1", "169.254.169.254", "0.0.0.0",
		"100.64.0.1", "100.100.100.200", "100.127.255.255", "::1", "fd00::1", "fe80::1", "::ffff:10.0.0.5",
		"64:ff9b::a00:5", "64:ff9b::7f00:1", "64:ff9b::6464:c8"} {
		if !Private(net.ParseIP(s)) {
			t.Errorf("%s not private", s)
		}
	}
	for _, s := range []string{"8.8.8.8", "100.63.255.255", "100.128.0.1", "2606:4700::1111", "64:ff9b::808:808"} {
		if Private(net.ParseIP(s)) {
			t.Errorf("%s private", s)
		}
	}
}
