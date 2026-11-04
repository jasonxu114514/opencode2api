package gateway

import (
	"opencode2api/internal/config"
	modelcatalog "opencode2api/internal/models"
)

// Model naming is a two-way mapping between the name clients use and the
// upstream id that serves the request:
//
//	explicit  models.aliases        — one hand-written rename
//	automatic models.strip_free_suffix — every "-free" id loses the suffix
//
// Both layers compose: an explicit alias always wins, so an operator can still
// pin a name that the automatic rule would have produced differently.

// catalogArgs reports the credential tiers that can serve a request. Model
// availability depends on them, so a name is only "available" when a concrete
// tier would actually answer.
func (g *Gateway) catalogArgs() (hasZen, hasGo, anonymous bool) {
	return g.zenNodes.Len() > 0, g.goNodes.Len() > 0, g.cfg.Anonymous
}

// routable reports whether the gateway can serve the id right now. It performs a
// pure lookup: no request state, no retries, no side effects.
func (g *Gateway) routable(model string) bool {
	if model == "" {
		return false
	}
	hasZen, hasGo, anonymous := g.catalogArgs()
	_, err := g.catalog.Route(model, hasZen, hasGo, anonymous)
	return err == nil
}

// resolveModel maps the name a client sent to the upstream id that serves it,
// and returns the name to echo back. Without aliasing both are the input, so
// behaviour is unchanged for unaliased deployments.
func (g *Gateway) resolveModel(name string) (real, public string) {
	if r, ok := g.cfg.RealModel(name); ok {
		return r, name
	}
	if !g.cfg.Models.StripFreeSuffix || name == "" {
		return name, name
	}
	// The client already used the upstream id; echo exactly what it asked for.
	if _, hasSuffix := config.StripFreeSuffix(name); hasSuffix {
		return name, name
	}
	// Prefer an id that exists as-is (a paid model with keys configured), and
	// only fall back to the free twin when that is the servable spelling.
	if g.routable(name) {
		return name, name
	}
	if candidate := name + config.FreeSuffix; g.routable(candidate) {
		return candidate, name
	}
	return name, name
}

// publicNames maps every routable upstream id to the name discovery should
// publish. Aliases are resolved against the live catalog, so a model the
// upstream adds later is renamed on the next refresh with no config change.
func (g *Gateway) publicNames() map[string]string {
	hasZen, hasGo, anonymous := g.catalogArgs()
	routes, _ := g.catalog.AvailableModels(hasZen, hasGo, anonymous)
	published := make(map[string]string, len(routes))
	routable := make(map[string]bool, len(routes))
	for _, route := range routes {
		routable[route.ID] = true
	}

	// Explicit aliases first: they own their public name outright.
	taken := make(map[string]bool, len(routes))
	for _, route := range routes {
		if public, ok := g.cfg.PublicModel(route.ID); ok {
			published[route.ID] = public
			taken[public] = true
		}
	}
	for _, route := range routes {
		if _, done := published[route.ID]; done {
			continue
		}
		name := route.ID
		if g.cfg.Models.StripFreeSuffix {
			if base, hasSuffix := config.StripFreeSuffix(route.ID); hasSuffix {
				// Skipping collisions keeps every published name unique: when
				// both "x" and "x-free" are routable they stay two entries.
				if !routable[base] && !taken[base] {
					name = base
				}
			}
		}
		published[route.ID] = name
		taken[name] = true
	}
	return published
}

// modelPublication is one entry of the discovery list after renaming.
type modelPublication struct {
	ID     string
	Public string
	Route  modelcatalog.Route
}
