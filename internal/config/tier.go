package config

// Tier names a keyed upstream tier. It is also the type of the "prefer"
// setting, which picks the tier that is tried first when both serve a model.
type Tier string

const (
	// TierZen is the keyed Zen tier, served by upstream.zen, whose default is
	// the v1 /zen origin. Anonymous routes resolve to TierZen too, although
	// the keyless lane sends to its own origins (see UpstreamConfig.Anonymous).
	TierZen Tier = "zen"
	// TierGo is the keyed Go plan tier, served by upstream.go, whose default
	// is the v2 /zen/go origin.
	TierGo Tier = "go"
)
