// Package rotation owns sticky, service-wide model selection. No network call
// runs under its lock. A generation prevents late requests undoing a switch.
package rotation

import (
	"encoding/json"
	"fmt"
	"opencode2api/internal/config"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"sync"
	"time"
)

type Attempt struct {
	Time    time.Time `json:"time"`
	Request string    `json:"request"`
	Alias   string    `json:"alias"`
	Model   string    `json:"model"`
	Number  int       `json:"number"`
	Outcome string    `json:"outcome"`
}
type Switch struct {
	Time   time.Time `json:"time"`
	From   string    `json:"from"`
	To     string    `json:"to"`
	Reason string    `json:"reason"`
}
type Group struct {
	ID         string               `json:"id"`
	Alias      string               `json:"alias"`
	Order      []string             `json:"order"`
	Current    string               `json:"current"`
	Generation uint64               `json:"generation"`
	Paused     map[string]time.Time `json:"paused"`
	Available  map[string]bool      `json:"available"`
	Switches   []Switch             `json:"switches"`
	// Dynamic marks a query-driven group. Query, Sort and Pin mirror the
	// configured definition so the WebUI can render the editor from state.
	Dynamic bool                  `json:"dynamic,omitempty"`
	Query   *config.RotationQuery `json:"query,omitempty"`
	Sort    *config.GroupSort     `json:"sort,omitempty"`
	Pin     []string              `json:"pin,omitempty"`
}
type Snapshot struct {
	Groups             []*Group  `json:"groups"`
	Attempts           []Attempt `json:"attempts"`
	PersistenceError   string    `json:"persistence_error,omitempty"`
	FirstOutputSeconds int       `json:"first_output_seconds"`
	// AvailableModels lists every routable model the operator may add to a
	// static custom group. It is filled by the runtime, not persisted.
	AvailableModels []string `json:"available_models"`
	// QueryFields, SortFields and SortDirections describe the query builder
	// vocabulary for the WebUI. Filled by the runtime, not persisted.
	QueryFields    []config.QueryField `json:"query_fields"`
	SortFields     []config.QueryField `json:"sort_fields"`
	SortDirections []string            `json:"sort_directions"`
}
type Token struct {
	Group, Model string
	Generation   uint64
}
type Manager struct {
	mu    sync.Mutex
	path  string
	state Snapshot
	cfg   config.RotationConfig
}

// customID namespaces operator-defined groups so their ID and alias stay in
// lockstep while remaining distinct from the built-in sota/sweet slots.
func customID(alias string) string { return "custom:" + alias }

// dynamicEqual reports whether two configurations describe the same dynamic
// group, so an unchanged query keeps its learned priority order.
func dynamicEqual(a, b config.RotationGroup) bool {
	return reflect.DeepEqual(a.Query, b.Query) && reflect.DeepEqual(a.Sort, b.Sort) && slices.Equal(a.Pin, b.Pin)
}

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
		kind int // 0 built-in, 1 static custom, 2 dynamic custom
	}
	list := []desired{
		{id: "sota", cfg: cfg.SOTA, kind: 0},
		{id: "sweet", cfg: cfg.Sweet, kind: 0},
	}
	for _, g := range cfg.Custom {
		kind := 1
		if g.Dynamic() {
			kind = 2
		}
		list = append(list, desired{id: customID(g.Alias), cfg: g, kind: kind})
	}
	next := make([]*Group, 0, len(list))
	for _, d := range list {
		g := existing[d.id]
		if g == nil {
			g = &Group{ID: d.id}
		}
		clone := config.CloneRotationGroup(d.cfg)
		prev := previous(d.cfg.Alias)
		switch d.kind {
		case 0:
			// The built-in groups keep any models they already learned while
			// adopting a changed configured order.
			if len(g.Order) == 0 || !slices.Equal(prev.Order, d.cfg.Order) {
				order := slices.Clone(d.cfg.Order)
				for _, v := range g.Order {
					if !slices.Contains(order, v) {
						order = append(order, v)
					}
				}
				if !slices.Equal(order, g.Order) {
					g.Generation++
				}
				g.Order = order
			}
		case 1:
			// A static custom group rotates exactly the configured models.
			if !slices.Equal(clone.Order, g.Order) {
				g.Generation++
			}
			g.Order = clone.Order
		case 2:
			// A dynamic group keeps its learned order; Sync recomputes the
			// membership. Only a changed definition invalidates the position.
			if !dynamicEqual(prev, d.cfg) {
				g.Generation++
			}
		}
		if g.Alias != d.cfg.Alias {
			g.Generation++
		}
		g.Alias = d.cfg.Alias
		g.Dynamic = d.kind == 2
		g.Query = clone.Query
		g.Sort = clone.Sort
		g.Pin = clone.Pin
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

// Sync folds the current model catalog into the groups. Custom and dynamic
// groups claim their members first (in config order); sota then takes its
// remaining pool; sweet keeps every leftover non-SOTA model. A dynamic group's
// order is recomputed from its query, keeping previous priority and appending
// new matches last.
func (m *Manager) Sync(facts []config.ModelFacts) {
	m.mu.Lock()
	defer m.mu.Unlock()
	available := make(map[string]bool, len(facts))
	factMap := make(map[string]config.ModelFacts, len(facts))
	for _, fact := range facts {
		factMap[fact.ID] = fact
		if fact.Routable {
			available[fact.ID] = true
		}
	}
	claimed := map[string]bool{}
	changed := false
	for _, c := range m.cfg.Custom {
		g := m.group(customID(c.Alias))
		if g == nil {
			continue
		}
		g.Available = map[string]bool{}
		if c.Dynamic() {
			matched := make([]string, 0, len(facts))
			for _, fact := range facts {
				if c.Query.Evaluate(fact) {
					matched = append(matched, fact.ID)
				}
			}
			order := config.ResolveOrder(matched, factMap, c.Sort, c.Pin, g.Order)
			if !slices.Equal(order, g.Order) {
				g.Order = order
				changed = true
			}
			for _, model := range order {
				if available[model] && !claimed[model] {
					g.Available[model] = true
					claimed[model] = true
				}
			}
			continue
		}
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
		if len(g.Order) == 0 {
			continue
		}
		if g.Current == "" || !slices.Contains(g.Order, g.Current) {
			g.Current = g.Order[0]
			g.Generation++
			changed = true
		}
	}
	if changed {
		m.saveLocked()
	}
}
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
func (m *Manager) Snapshot() Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, _ := json.Marshal(m.state)
	var s Snapshot
	_ = json.Unmarshal(b, &s)
	return s
}
func (m *Manager) group(id string) *Group {
	for _, g := range m.state.Groups {
		if g.ID == id {
			return g
		}
	}
	return nil
}
func usable(g *Group, model string) bool {
	return g.Available[model] && !time.Now().Before(g.Paused[model])
}
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
func (m *Manager) switchLocked(g *Group, to, reason string) {
	from := g.Current
	g.Current = to
	g.Generation++
	g.Switches = append(g.Switches, Switch{time.Now().UTC(), from, to, reason})
	if len(g.Switches) > 30 {
		g.Switches = g.Switches[len(g.Switches)-30:]
	}
}
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
func (m *Manager) saveLocked() {
	if m.path == "" {
		return
	}
	m.state.PersistenceError = ""
	if err := m.writeLocked(); err != nil {
		m.state.PersistenceError = "轮转状态写入失败，请检查磁盘空间和文件权限"
	}
}
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
