package httpx

import (
	"fmt"
	"runtime"
)

// clientVersion is the OpenCode release the gateway claims to be on the
// /inference lane. Upstream accepts a range of releases (the version number
// itself is not validated), so this only needs bumping when it starts rejecting
// older ones.
const clientVersion = "2.0.29"

// legacyClientVersion is the pre-2.x OpenCode release reported on the legacy
// /zen/v1 lane, matching the v1.3.7 User-Agent shape.
const legacyClientVersion = "1.18.31"

// UserAgent reports the gateway as an OpenCode 2.x client using the shape the
// /inference gate expects: "opencode/<channel>/<version>/<name>".
func UserAgent() string {
	return "opencode/latest/" + clientVersion + "/cli"
}

// LegacyUserAgent reports the gateway using the v1.3.7 OpenCode 1.18.x
// User-Agent shape ("opencode/<version> (<os> <arch>; <runtime>)") for the
// legacy /zen/v1 lane.
func LegacyUserAgent() string {
	return fmt.Sprintf("opencode/%s (%s %s; %s)", legacyClientVersion, runtime.GOOS, runtime.GOARCH, runtime.Version())
}
