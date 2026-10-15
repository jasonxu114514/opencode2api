package httpx

// clientVersion is the OpenCode release the gateway claims to be. Upstream
// accepts a range of releases (the version number itself is not validated),
// so this only needs bumping when it starts rejecting older ones.
const clientVersion = "2.0.29"

// UserAgent reports the gateway as an OpenCode client using the shape upstream
// expects: "opencode/<channel>/<version>/<name>". The previous
// "opencode/<version> (<os> <arch>; <runtime>)" form is rejected with 403
// FreeTierError even when the rest of the request is identical.
func UserAgent() string {
	return "opencode/latest/" + clientVersion + "/cli"
}
