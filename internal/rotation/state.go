// Package rotation owns sticky, service-wide model selection. No network call
// runs under its lock. A generation prevents late requests from undoing a
// switch.
package rotation

import (
	"encoding/json"
	"fmt"
	"opencode2api/internal/config"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"sync"
	"time"
)

// Attempt is one upstream attempt of a rotation request, kept for the WebUI.
// Number counts the attempts of that request across models; Record sets Time.
type Attempt struct {
	Time    time.Time `json:"time"`
	Request string    `json:"request"`
	Alias   string    `json:"alias"`
	Model   string    `json:"model"`
	Number  int       `json:"number"`
	Outcome string    `json:"outcome"`
}

// Switch records one change of a group's current model.
type Switch struct {
	Time   time.Time `json:"time"`
	From   string    `json:"from"`
	To     string    `json:"to"`
	Reason string    `json:"reason"`
}

// Group is one rotation group ("sota" or "sweet"), served under Alias. Order
// is the rotation order and Current the model requests start on. Generation
// grows whenever Current is set and whenever Configure changes Order or Alias.
// Paused maps a model to the end of its pause, Available holds the models of
// the last Sync, and Switches keeps the latest 30 switches.
type Group struct {
	ID         string               `json:"id"`
	Alias      string               `json:"alias"`
	Order      []string             `json:"order"`
	Current    string               `json:"current"`
	Generation uint64               `json:"generation"`
	Paused     map[string]time.Time `json:"paused"`
	Available  map[string]bool      `json:"available"`
	Switches   []Switch             `json:"switches"`
}

// Snapshot is the rotation state: both groups, the latest 100 attempts, the
// configured first-output timeout, and the error of the last write when it
// failed.
type Snapshot struct {
	Groups             []*Group  `json:"groups"`
	Attempts           []Attempt `json:"attempts"`
	PersistenceError   string    `json:"persistence_error,omitempty"`
	FirstOutputSeconds int       `json:"first_output_seconds"`
	// AvailableModels lists every Go-routable model the operator may add to a
	// custom group. It is filled by the runtime, not persisted.
	AvailableModels []string `json:"available_models"`
}

// Token is the model Select handed to a request, with the group generation it
// was selected at. Fail only switches away from the model while that
// generation is still current.
type Token struct {
	Group, Model string
	Generation   uint64
}

// Manager owns the rotation state and persists it as JSON at path, or keeps it
// in memory only when path is empty. mu guards all state; methods ending in
// Locked expect the caller to hold it.
type Manager struct {
	mu    sync.Mutex
	path  string
	state Snapshot
	cfg   config.RotationConfig
}

// customID namespaces operator-defined groups so their ID and alias stay in
// lockstep while remaining distinct from the built-in sota/sweet slots.
func customID(alias string) string { return "custom:" + alias }

// New loads the state from path (a missing file starts empty), applies cfg and
// persists the result. It fails when the file cannot be read or parsed, when
// its group IDs are empty or repeated, or when the state cannot be written.
func New(path string, cfg config.RotationConfig) (*Manager, error) {
	m := &Manager{path: path}
	if path != "" {
		b, e := os.ReadFile(path)
		if e == nil {
			if e = json.Unmarshal(b, &m.state); e != nil {
				return nil, fmt.Errorf("invalid rotation state: %w", e)
			}
		} else if !os.IsNotExist(e) {
			return nil, e
		}
	}
	seen := map[string]bool{}
	for _, g := range m.state.Groups {
		if g == nil || g.ID == "" || seen[g.ID] {
			return nil, fmt.Errorf("invalid rotation groups")
		}
		seen[g.ID] = true
	}
	m.Configure(cfg)
	if m.state.PersistenceError != "" {
		return nil, fmt.Errorf("cannot persist rotation state")
	}
	return m, nil
}

// Configure applies the configured aliases and orders and persists the state.
// The built-in groups keep any models they already learned while adopting a
// changed configured order; a custom group rotates exactly its configured
// models. Groups that disappeared from the config are dropped, and a Current
// that left the order moves to its first model.
func (m *Manager) Configure(cfg config.RotationConfig) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cfg.Defaults()
	previous := func(alias string) config.RotationGroup {
		for _, p := range m.cfg.All() {
			if p.Alias == alias {
				return p
			}
		}
		return config.RotationGroup{}
	}
	existing := map[string]*Group{}
	for _, g := range m.state.Groups {
		if g != nil {
			existing[g.ID] = g
		}
	}
	type desired struct {
		id   string
		cfg  config.RotationGroup
		auto bool
	}
	list := []desired{
		{id: "sota", cfg: cfg.SOTA, auto: true},
		{id: "sweet", cfg: cfg.Sweet, auto: true},
	}
	for _, g := range cfg.Custom {
		list = append(list, desired{id: customID(g.Alias), cfg: g})
	}
	next := make([]*Group, 0, len(list))
	for _, d := range list {
		g := existing[d.id]
		if g == nil {
			g = &Group{ID: d.id}
		}
		var order []string
		if d.auto {
			// The built-in groups keep any models they already learned while
			// adopting a changed configured order.
			prev := previous(d.cfg.Alias)
			if len(g.Order) == 0 || !slices.Equal(prev.Order, d.cfg.Order) {
				order = slices.Clone(d.cfg.Order)
				for _, v := range g.Order {
					if !slices.Contains(order, v) {
						order = append(order, v)
					}
				}
			} else {
				order = slices.Clone(g.Order)
			}
		} else {
			// A custom group rotates exactly the configured models.
			order = slices.Clone(d.cfg.Order)
		}
		if !slices.Equal(order, g.Order) || g.Alias != d.cfg.Alias {
			g.Generation++
		}
		g.Order = order
		g.Alias = d.cfg.Alias
		if g.Paused == nil {
			g.Paused = map[string]time.Time{}
		}
		if !slices.Contains(g.Order, g.Current) && len(g.Order) > 0 {
			g.Current = g.Order[0]
			g.Generation++
		}
		next = append(next, g)
	}
	m.state.Groups = next
	m.cfg = cfg
	m.state.FirstOutputSeconds = cfg.FirstOutputSeconds
	m.saveLocked()
}

// Sync folds the current model catalog into the groups. Custom groups claim
// their explicit members first; sota then takes its remaining pool; sweet keeps
// every leftover non-SOTA model, appending new models to its order in sorted
// order. A group without a Current starts on its first model. The state is
// persisted only when an order or Current changed.
func (m *Manager) Sync(available map[string]bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	claimed := map[string]bool{}
	changed := false
	for _, c := range m.cfg.Custom {
		g := m.group(customID(c.Alias))
		if g == nil {
			continue
		}
		g.Available = map[string]bool{}
		for _, model := range c.Order {
			if available[model] && !claimed[model] {
				g.Available[model] = true
				claimed[model] = true
			}
		}
	}
	if g := m.group("sota"); g != nil {
		g.Available = map[string]bool{}
		for model := range available {
			if config.IsSOTA(model) && !claimed[model] {
				g.Available[model] = true
				claimed[model] = true
			}
		}
	}
	if g := m.group("sweet"); g != nil {
		g.Available = map[string]bool{}
		added := []string{}
		for model := range available {
			if config.IsSOTA(model) || claimed[model] {
				continue
			}
			g.Available[model] = true
			claimed[model] = true
			if !slices.Contains(g.Order, model) {
				added = append(added, model)
			}
		}
		sort.Strings(added)
		if len(added) > 0 {
			g.Order = append(g.Order, added...)
			changed = true
		}
	}
	for _, g := range m.state.Groups {
		if g.Current == "" && len(g.Order) > 0 {
			g.Current = g.Order[0]
			g.Generation++
			changed = true
		}
	}
	if changed {
		m.saveLocked()
	}
}

// GroupID returns the ID of the group served under alias, or "" when alias
// names no group.
func (m *Manager) GroupID(alias string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, g := range m.state.Groups {
		if g.Alias == alias {
			return g.ID
		}
	}
	return ""
}

// Snapshot returns a deep copy of the state.
func (m *Manager) Snapshot() Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, _ := json.Marshal(m.state)
	var s Snapshot
	_ = json.Unmarshal(b, &s)
	return s
}

// group returns the group with id, or nil. The caller holds m.mu.
func (m *Manager) group(id string) *Group {
	for _, g := range m.state.Groups {
		if g.ID == id {
			return g
		}
	}
	return nil
}

// usable reports whether model is available in g and not paused.
func usable(g *Group, model string) bool {
	return g.Available[model] && !time.Now().Before(g.Paused[model])
}

// Select returns the first usable model of group id that the request has not
// visited, starting at Current and following the order. When Current itself
// is unusable, the selection becomes the new global Current; skipping a model
// the request already visited does not. It reports false when no model is
// left.
func (m *Manager) Select(id string, visited map[string]bool) (Token, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	g := m.group(id)
	if g == nil || len(g.Order) == 0 {
		return Token{}, false
	}
	start := max(slices.Index(g.Order, g.Current), 0)
	for n := 0; n < len(g.Order); n++ {
		model := g.Order[(start+n)%len(g.Order)]
		if visited[model] || !usable(g, model) {
			continue
		}
		// A request-local skip must not change a newer, healthy global selection.
		if !usable(g, g.Current) {
			m.switchLocked(g, model, "当前模型暂停或已下架")
			m.saveLocked()
		}
		return Token{g.ID, model, g.Generation}, true
	}
	return Token{}, false
}

// switchLocked sets the Current of g to to, bumps its Generation and records
// the switch only when the model changes, keeping the latest 30. The caller persists the state.
func (m *Manager) switchLocked(g *Group, to, reason string) {
	from := g.Current
	g.Current = to
	g.Generation++
	if from == to {
		return
	}
	g.Switches = append(g.Switches, Switch{time.Now().UTC(), from, to, reason})
	if len(g.Switches) > 30 {
		g.Switches = g.Switches[len(g.Switches)-30:]
	}
}

// Fail pauses the model of t for pause, never shortening a longer pause. If
// t is still the group's Current at the same generation, Current moves to the
// next usable model in the order; when there is none, it remains the same model
// and Generation still bumps without recording a switch.
func (m *Manager) Fail(t Token, pause time.Duration, reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	g := m.group(t.Group)
	if g == nil {
		return
	}
	until := time.Now().Add(pause).UTC()
	if until.After(g.Paused[t.Model]) {
		g.Paused[t.Model] = until
	}
	if g.Current == t.Model && g.Generation == t.Generation {
		start := slices.Index(g.Order, t.Model)
		next := t.Model
		for n := 1; n < len(g.Order); n++ {
			v := g.Order[(start+n)%len(g.Order)]
			if usable(g, v) {
				next = v
				break
			}
		}
		m.switchLocked(g, next, reason)
	}
	m.saveLocked()
}

// Manual makes model the Current of group id at an operator's request and
// lifts its pause. The model must be in the group's order and available. It
// fails when the new state cannot be persisted.
func (m *Manager) Manual(id, model string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	g := m.group(id)
	if g == nil || !slices.Contains(g.Order, model) || !g.Available[model] {
		return fmt.Errorf("请选择该组当前可用的 Go 模型")
	}
	delete(g.Paused, model)
	m.switchLocked(g, model, "管理员手动切换")
	m.saveLocked()
	if m.state.PersistenceError != "" {
		return fmt.Errorf("轮转状态保存失败")
	}
	return nil
}

// Record stamps a with the current time and keeps it among the latest 100
// attempts.
func (m *Manager) Record(a Attempt) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a.Time = time.Now().UTC()
	m.state.Attempts = append(m.state.Attempts, a)
	if len(m.state.Attempts) > 100 {
		m.state.Attempts = m.state.Attempts[len(m.state.Attempts)-100:]
	}
	m.saveLocked()
}

// saveLocked persists the state when there is a path. A failed write is not
// returned: it is kept in PersistenceError until the next successful write.
func (m *Manager) saveLocked() {
	if m.path == "" {
		return
	}
	m.state.PersistenceError = ""
	if err := m.writeLocked(); err != nil {
		m.state.PersistenceError = "轮转状态写入失败，请检查磁盘空间和文件权限"
	}
}

// writeLocked writes the state through a temporary file and a rename.
func (m *Manager) writeLocked() error {
	b, e := json.MarshalIndent(m.state, "", "  ")
	if e != nil {
		return e
	}
	f, e := os.CreateTemp(filepath.Dir(m.path), ".rotation-*.tmp")
	if e != nil {
		return e
	}
	defer os.Remove(f.Name())
	if _, e = f.Write(b); e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e == nil {
		e = ce
	}
	if e != nil {
		return e
	}
	return os.Rename(f.Name(), m.path)
}
