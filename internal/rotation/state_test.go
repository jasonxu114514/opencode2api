package rotation

import (
	"opencode2api/internal/config"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"
)

func TestStickyPersistenceMembershipAndGeneration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	cfg := config.RotationConfig{}
	cfg.Defaults()
	m, err := New(path, cfg)
	if err != nil {
		t.Fatal(err)
	}
	available := map[string]bool{"deepseek-v4.1-flash": true, "glm-5.3": true, "glm-5.3-flash": true, "z-model": true, "a-model": true}
	m.Sync(available)
	s := m.Snapshot()
	if !slices.Equal(s.Groups[1].Order, []string{"glm-5.3-flash", "a-model", "z-model"}) {
		t.Fatal(s.Groups[1].Order)
	}
	first, _ := m.Select("sota", nil)
	same, _ := m.Select("sota", nil)
	if first != same {
		t.Fatal("success should not rotate")
	}
	m.Fail(first, 15*time.Second, "error")
	next, _ := m.Select("sota", nil)
	if next.Model != "glm-5.3" {
		t.Fatal(next)
	}
	// Old inflight failures cannot undo a manual selection (nor can successes).
	if err := m.Manual("sota", "deepseek-v4.1-flash"); err != nil {
		t.Fatal(err)
	}
	before := m.Snapshot().Groups[0].Generation
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); m.Fail(first, time.Second, "stale"); m.Snapshot() }()
	}
	wg.Wait()
	s = m.Snapshot()
	if s.Groups[0].Current != "deepseek-v4.1-flash" || s.Groups[0].Generation != before {
		t.Fatal("stale completion moved cursor")
	}
	// Reordering config and key-only reloads keep operational state.
	cfg.Sweet.Order = []string{"z-model", "a-model", "glm-5.3-flash"}
	m.Configure(cfg)
	available["0-new-model"] = true
	delete(available, "a-model")
	m.Sync(available)
	m2, err := New(path, cfg)
	if err != nil {
		t.Fatal(err)
	}
	m2.Sync(available)
	s = m2.Snapshot()
	if !slices.Equal(s.Groups[1].Order, []string{"z-model", "a-model", "glm-5.3-flash", "0-new-model"}) {
		t.Fatal(s.Groups[1].Order)
	}
	if s.Groups[1].Available["a-model"] {
		t.Fatal("delisted model available")
	}
	if s.Groups[0].Paused[first.Model].IsZero() {
		t.Fatal("pause not persisted")
	}
	if s.Groups[0].Current != "deepseek-v4.1-flash" {
		t.Fatal("current not persisted")
	}
}
func TestNoWrapDuplicatesAndUnavailable(t *testing.T) {
	m, _ := New("", config.RotationConfig{})
	m.Sync(map[string]bool{"glm-5.3": true, "kimi-k3": true})
	visited := map[string]bool{}
	for i := 0; i < 2; i++ {
		token, ok := m.Select("sota", visited)
		if !ok {
			t.Fatal(i)
		}
		visited[token.Model] = true
		m.Fail(token, time.Minute, "quota")
	}
	if _, ok := m.Select("sota", visited); ok {
		t.Fatal("revisited model")
	}
	if _, ok := m.Select("sota", nil); ok {
		t.Fatal("paused model selected")
	}
}
