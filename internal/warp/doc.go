// Package warp carries outbound TCP traffic through Cloudflare WARP using a
// userspace WireGuard device and a gVisor network stack. It needs no TUN
// device, root privileges, or system routes: UDP sockets carry WireGuard
// packets, and the netstack terminates TCP inside the process.
//
// The implementation is adapted from github.com/sudogeeker/ai-route
// (internal/warp), which is MIT licensed. Only the parts this gateway needs
// are kept: registration, exit verification, and a small rotating pool.
package warp
