package models

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const FreeModelCheckInterval = time.Hour
const FailedModelCheckInterval = 24 * time.Hour

type EffortProbe struct {
	Effort   string `json:"effort,omitempty"`
	Protocol string `json:"protocol,omitempty"`
	Reset    bool   `json:"-"`
}

type Availability struct {
	Model     string    `json:"model"`
	Disabled  bool      `json:"disabled"`
	CheckedAt time.Time `json:"checked_at,omitempty"`
	NextCheck time.Time `json:"next_check"`
	Reason    string    `json:"reason,omitempty"`
	Channel   string    `json:"channel,omitempty"`
	Manual    bool      `json:"manual,omitempty"`
	// Hidden mirrors models.hidden for the availability page. It is display
	// state only: a hidden model keeps its real probe result rather than being
	// reported as healthy.
	Hidden          bool        `json:"hidden,omitempty"`
	Generation      uint64      `json:"-"`
	ProbeVersion    int         `json:"probe_version,omitempty"`
	AutoEffort      EffortProbe `json:"auto_effort,omitempty"`
	EffortCheckedAt time.Time   `json:"effort_checked_at,omitempty"`
}

type AvailabilityStore struct {
	mu    sync.RWMutex
	path  string
	items map[string]Availability
}

func NewAvailabilityStore(path string) (*AvailabilityStore, error) {
	s := &AvailabilityStore{path: path, items: map[string]Availability{}}
	if path != "" {
		b, err := os.ReadFile(path)
		if err == nil {
			if err = json.Unmarshal(b, &s.items); err != nil {
				return nil, err
			}
		} else if !os.IsNotExist(err) {
			return nil, err
		}
	}
	if s.items == nil {
		s.items = map[string]Availability{}
	}
	// Legacy probes used invalid session IDs and treated temporary errors as
	// permanent failures. These records cannot establish model unavailability.
	// The migration stays in memory and is re-derived on every load; the next
	// explicit write persists it.
	for model, item := range s.items {
		if item.ProbeVersion == 0 && item.Disabled {
			item.Disabled = false
			item.NextCheck = time.Time{}
			item.Reason = "legacy_probe_recheck"
			s.items[model] = item
		}
		// Manual restores predate the manual lock flag. Preserve the
		// operator's intent: a model left as manually_enabled stays under
		// manual control instead of falling back under automatic probing.
		if !item.Disabled && !item.Manual && item.Reason == "manually_enabled" {
			item.Manual = true
			s.items[model] = item
		}
	}
	return s, nil
}

func (s *AvailabilityStore) Get(model string) Availability {
	s.mu.RLock()
	defer s.mu.RUnlock()
	item := s.items[model]
	item.Model = model
	return item
}

func (s *AvailabilityStore) Disabled(model string) bool { return s.Get(model).Disabled }

// SetManual switches a model between manual and automatic control. Manual
// mode keeps the model enabled and excludes it from automatic probing;
// switching back to automatic schedules a probe on the next check.
func (s *AvailabilityStore) SetManual(model string, manual bool, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	item, existed := s.items[model]
	// Retried requests must not erase disable evidence or reset the schedule.
	if item.Manual == manual {
		return nil
	}
	previous := item
	item.Model = model
	item.Manual = manual
	item.Generation++
	item.ProbeVersion = 1
	if manual {
		item.Disabled = false
		item.Reason = "manually_enabled"
		item.NextCheck = now.Add(FreeModelCheckInterval)
	} else {
		item.Reason = "auto_probe"
		item.NextCheck = time.Time{}
	}
	s.items[model] = item
	if err := s.saveLocked(); err != nil {
		if existed {
			s.items[model] = previous
		} else {
			delete(s.items, model)
		}
		return err
	}
	return nil
}

// A manual restore takes the model under manual control and invalidates
// results from probes that were already running.
func (s *AvailabilityStore) Restore(model string, now time.Time) error {
	return s.SetManual(model, true, now)
}

func (s *AvailabilityStore) Record(model string, generation uint64, success bool, reason, channel string, now time.Time, effort EffortProbe) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	item := s.items[model]
	if item.Generation != generation {
		return nil
	}
	// Manual control freezes the record: automatic probes must never
	// overwrite a manually_enabled state (neither Disabled nor Reason).
	if item.Manual {
		return nil
	}
	item.Model = model
	if success {
		item.Disabled = false
	} else if reason == "model_unavailable" {
		item.Disabled = true
	} else if item.Disabled {
		// An inconclusive recheck of a still-disabled model carries no new
		// evidence. Keep the previous disable reason and backoff instead of
		// resetting to an hourly retry with the transient error.
		item.CheckedAt = now
		item.ProbeVersion = 1
		// A scheduled recheck has already reached NextCheck. Advance it to
		// avoid probing on every scheduler tick after an inconclusive result.
		if !item.NextCheck.After(now) {
			item.NextCheck = now.Add(FailedModelCheckInterval)
		}
		s.items[model] = item
		return s.saveLocked()
	}
	item.CheckedAt, item.Reason, item.Channel = now, reason, channel
	item.ProbeVersion = 1
	if success && effort.Effort != "" {
		item.AutoEffort, item.EffortCheckedAt = effort, now
	} else if success && effort.Reset {
		item.AutoEffort, item.EffortCheckedAt = EffortProbe{}, time.Time{}
	}
	interval := FreeModelCheckInterval
	if !success && reason == "model_unavailable" {
		interval = FailedModelCheckInterval
	}
	item.NextCheck = now.Add(interval)
	s.items[model] = item
	return s.saveLocked()
}

func (s *AvailabilityStore) saveLocked() error {
	if s.path == "" {
		return nil
	}
	b, err := json.MarshalIndent(s.items, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(s.path), ".availability-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(f.Name(), s.path)
}
