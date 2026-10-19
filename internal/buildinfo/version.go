// Package buildinfo shares the executable version with HTTP handlers.
package buildinfo

// Version is set once by the command before any background work starts, from
// the value the release build injected into its own version variable. It is
// reported by /api/monitor and by the gateway health endpoints.
var Version = "dev"
