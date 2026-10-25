//go:build !linux

package warp

import "syscall"

// freebindControl is a no-op outside Linux: the configured source address must
// already be assigned to an interface.
func freebindControl(_, _ string, _ syscall.RawConn) error { return nil }
