//go:build linux

package warp

import "syscall"

// ipv6FREEBIND is IPV6_FREEBIND (Linux 2.6.22+), which lets a socket bind an
// IPv6 source address that is routed to this host but not assigned to an
// interface. golang.org/x/sys/unix does not export the constant yet.
const ipv6FREEBIND = 78

// freebindControl permits binding an outer source address from the configured
// prefix. Without it the kernel refuses every address the host did not assign
// to an interface, which is the normal case for a routed /64.
func freebindControl(_, _ string, raw syscall.RawConn) error {
	var sockErr error
	if err := raw.Control(func(fd uintptr) {
		sockErr = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IPV6, ipv6FREEBIND, 1)
	}); err != nil {
		return err
	}
	return sockErr
}
