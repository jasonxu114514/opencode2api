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
	// byCase and byShape map a folded client name back to the one declared
	// client tool it names. A value of "" marks a fold two tools share: that
	// name is never guessed between.
	byCase  map[string]string
	byShape map[string]string
}

// NewToolRenames returns an empty rename set.
func NewToolRenames() *ToolRenames {
	return &ToolRenames{
		toWire:   map[string]string{},
		toClient: map[string]string{},
		byCase:   map[string]string{},
		byShape:  map[string]string{},
	}
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
	r.bindFolded(r.byCase, strings.ToLower(client), client)
	r.bindFolded(r.byShape, foldToolName(client), client)
}

// bindFolded records client under key in a folded map. An empty key names
// nothing (a client tool spelled only with separators must not be handed every
// other such name), and a key two clients share is marked ambiguous.
func (r *ToolRenames) bindFolded(folded map[string]string, key, client string) {
	if folded == nil || key == "" {
		return
	}
	if existing, ok := folded[key]; ok {
		if existing != client {
			folded[key] = ""
		}
		return
	}
	folded[key] = client
}

// toolNameSeparators are the characters ignored when matching a tool name by
// shape, after case: "todo_write", "todo-write", "todo.write" and "todowrite"
// all fold to one key.
const toolNameSeparators = "_.- \t\r\n"

// foldToolName lowercases name and removes the separators agents spell tool
// names with, so two spellings of one tool fold together.
func foldToolName(name string) string {
	var builder strings.Builder
	for _, char := range strings.ToLower(name) {
		if !strings.ContainsRune(toolNameSeparators, char) {
			builder.WriteRune(char)
		}
	}
	return builder.String()
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

// Resolve maps a tool name the upstream reported back to the name the client
// declared. It is the response-side counterpart of the wire rename: an exact
// wire name restores, a clint_ prefix is stripped as a best-effort recovery
// (for example after a configuration reload mid-stream), and a name that
// differs from exactly one declared client tool only in case, or else only in
// case and separators (todowrite → todo_write), is folded onto that tool. A
// name two tools fold to is never guessed between. It reports whether the
// result differs from the name given.
func (r *ToolRenames) Resolve(wire string) (string, bool) {
	if r == nil || wire == "" {
		return wire, false
	}
	if client, ok := r.Client(wire); ok {
		return client, client != wire
	}
	client := wire
	if rest, ok := strings.CutPrefix(wire, ClientToolPrefix); ok && rest != "" {
		client = rest
	}
	if _, declared := r.toWire[client]; declared {
		return client, client != wire
	}
	lower := strings.ToLower(client)
	if match, ok := r.byCase[lower]; ok {
		// A key present with an empty value is two client tools folded
		// together; the name as written is the only safe answer.
		if match != "" {
			return match, match != wire
		}
		return client, client != wire
	}
	if match, ok := r.byShape[foldToolName(client)]; ok && match != "" {
		return match, match != wire
	}
	return client, client != wire
}

// Restore maps a wire name back to the client name. See Resolve for the rules;
// a name it cannot map is returned as is.
func (r *ToolRenames) Restore(wire string) string {
	client, _ := r.Resolve(wire)
	return client
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
