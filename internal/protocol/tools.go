package protocol

import (
	"strconv"
	"strings"
)

// ClientToolPrefix marks every client tool the gateway renames on a shaped
// free-tier request. The prefix keeps client tools distinct from the official
// OpenCode set the gateway appends, and lets responses be restored to the names
// the client declared.
const ClientToolPrefix = "clint_"

// toolWireNameLimit is the longest tool name the upstreams accept.
const toolWireNameLimit = 64

// ToolRenames records the client-to-wire tool names of one shaped request. The
// zero value and a nil pointer are both empty and safe to use.
type ToolRenames struct {
	toWire   map[string]string
	toClient map[string]string
}

// NewToolRenames returns an empty rename set.
func NewToolRenames() *ToolRenames {
	return &ToolRenames{toWire: map[string]string{}, toClient: map[string]string{}}
}

// Empty reports whether no tool was renamed.
func (r *ToolRenames) Empty() bool {
	return r == nil || len(r.toWire) == 0
}

// Count reports how many client tools were renamed.
func (r *ToolRenames) Count() int {
	if r == nil {
		return 0
	}
	return len(r.toWire)
}

// Wire returns the wire name registered for an exact client name.
func (r *ToolRenames) Wire(client string) (string, bool) {
	if r == nil || client == "" {
		return "", false
	}
	wire, ok := r.toWire[client]
	return wire, ok
}

// Client returns the client name registered for an exact wire name.
func (r *ToolRenames) Client(wire string) (string, bool) {
	if r == nil || wire == "" {
		return "", false
	}
	client, ok := r.toClient[wire]
	return client, ok
}

// Add records one client-to-wire mapping. Adding the same pair twice is a
// no-op; a conflicting pair keeps the first wire name so the map stays
// invertible.
func (r *ToolRenames) Add(client, wire string) {
	if r == nil || client == "" || wire == "" || client == wire {
		return
	}
	if existing, ok := r.toWire[client]; ok {
		if existing != wire {
			return
		}
	}
	if _, taken := r.toClient[wire]; taken {
		return
	}
	r.toWire[client] = wire
	r.toClient[wire] = client
}

// HasPrefix reports whether any wire name starts with prefix.
func (r *ToolRenames) HasPrefix(prefix string) bool {
	if r == nil || prefix == "" {
		return false
	}
	for wire := range r.toClient {
		if strings.HasPrefix(wire, prefix) {
			return true
		}
	}
	return false
}

// HasLongerPrefix reports whether any wire name is longer than prefix and
// starts with it. A stream fragment builder uses this to hold a partial name
// back until the next chunk resolves it.
func (r *ToolRenames) HasLongerPrefix(prefix string) bool {
	if r == nil || prefix == "" {
		return false
	}
	for wire := range r.toClient {
		if len(wire) > len(prefix) && strings.HasPrefix(wire, prefix) {
			return true
		}
	}
	return false
}

// Restore maps a wire name back to the client name. When the exact mapping is
// gone (for example after a configuration reload mid-stream), stripping the
// prefix is the best available recovery; a name without it is returned as is.
func (r *ToolRenames) Restore(wire string) string {
	if client, ok := r.Client(wire); ok {
		return client
	}
	if strings.HasPrefix(wire, ClientToolPrefix) {
		if rest := strings.TrimPrefix(wire, ClientToolPrefix); rest != "" {
			return rest
		}
	}
	return wire
}

// ClientToolWireName builds the wire name for one client tool. The name keeps
// only [A-Za-z0-9_-], collapses every other run to a single underscore, and is
// trimmed; an empty result becomes "tool". It is truncated to fit alongside the
// prefix and the 64-byte limit, and collisions with used names get a _2, _3, …
// suffix. The chosen name is recorded in used.
func ClientToolWireName(name string, used map[string]bool) string {
	if used == nil {
		used = map[string]bool{}
	}
	clean := sanitizeToolName(name)
	if clean == "" {
		clean = "tool"
	}
	if max := toolWireNameLimit - len(ClientToolPrefix); len(clean) > max {
		clean = clean[:max]
	}
	base := ClientToolPrefix + clean
	if !used[base] {
		used[base] = true
		return base
	}
	for suffix := 2; ; suffix++ {
		tag := "_" + strconv.Itoa(suffix)
		cut := toolWireNameLimit - len(tag)
		if cut > len(base) {
			cut = len(base)
		}
		candidate := base[:cut] + tag
		if !used[candidate] {
			used[candidate] = true
			return candidate
		}
	}
}

// sanitizeToolName reduces a client tool name to the character set upstreams
// accept, collapsing invalid runs into one underscore.
func sanitizeToolName(name string) string {
	var builder strings.Builder
	underscored := false
	for _, char := range name {
		switch {
		case char >= 'a' && char <= 'z', char >= 'A' && char <= 'Z', char >= '0' && char <= '9', char == '-', char == '_':
			builder.WriteRune(char)
			underscored = char == '_'
		default:
			if !underscored {
				builder.WriteByte('_')
				underscored = true
			}
		}
	}
	return strings.Trim(builder.String(), "_-")
}
