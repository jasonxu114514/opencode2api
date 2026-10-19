package models

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// FreeModelCheckInterval is the delay before a free model is probed again
// after a success or an inconclusive failure.
const FreeModelCheckInterval = time.Hour

// FailedModelCheckInterval is the longer delay once a probe has found the model
// unavailable, and after an inconclusive recheck of a disabled model.
const FailedModelCheckInterval = 24 * time.Hour

// EffortProbe is the result of the automatic reasoning-effort probe: the
// highest Effort the model accepted ("none" when only the baseline request
// succeeded) and the Protocol it was probed on. Reset is never persisted; it
// asks Record to clear a previously stored result.
type EffortProbe struct {
	Effort   string `json:"effort,omitempty"`
	Protocol string `json:"protocol,omitempty"`
	Reset    bool   `json:"-"`
}

// Availability is the probe state of one free model. Generation lives only in
// memory: every manual/automatic switch bumps it, so Record can drop the result
// of a probe that started before the switch. ProbeVersion 0 marks a record
// written by the old probe, which AvailabilityStore migrates on load.
type Availability struct {
	Model           string      `json:"model"`
	Disabled        bool        `json:"disabled"`
	CheckedAt       time.Time   `json:"checked_at,omitempty"`
	NextCheck       time.Time   `json:"next_check"`
	Reason          string      `json:"reason,omitempty"`
	Channel         string      `json:"channel,omitempty"`
	Manual          bool        `json:"manual,omitempty"`
	Generation      uint64      `json:"-"`
	ProbeVersion    int         `json:"probe_version,omitempty"`
	AutoEffort      EffortProbe `json:"auto_effort,omitempty"`
	EffortCheckedAt time.Time   `json:"effort_checked_at,omitempty"`
}

// AvailabilityRegistry tracks which free models the background probes found
// available and which ones an operator put under manual control. It is
// persisted as JSON at path, or kept in memory only when path is empty.
type AvailabilityRegistry struct {
	mu    sync.RWMutex
	path  string
	items map[string]Availability
}

// AvailabilityStore loads the registry from path. A missing file starts an
// empty registry; an unreadable or malformed one is an error.
func AvailabilityStore(path string) (*AvailabilityRegistry, error) {
	s := &AvailabilityRegistry{path: path, items: map[string]Availability{}}
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

// Get returns the record of model, or an enabled, never-checked record when the
// registry has none.
func (s *AvailabilityRegistry) Get(model string) Availability {
	s.mu.RLock()
	defer s.mu.RUnlock()
	item := s.items[model]
	item.Model = model
	return item
}

func (s *AvailabilityRegistry) Disabled(model string) bool { return s.Get(model).Disabled }

// SetManual switches a model between manual and automatic control. Manual
// mode keeps the model enabled and excludes it from automatic probing;
// switching back to automatic schedules a probe on the next check. A call that
// does not change the mode is a no-op. A switch bumps Generation, and the
// previous record is restored when it cannot be persisted.
func (s *AvailabilityRegistry) SetManual(model string, manual bool, now time.Time) error {
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

// Restore takes the model under manual control, which re-enables it and, by
// bumping Generation, invalidates results from probes that were already
// running. A model already under manual control is left untouched and its
// Generation is not bumped; Record ignores manual records anyway.
func (s *AvailabilityRegistry) Restore(model string, now time.Time) error {
	return s.SetManual(model, true, now)
}

// Record stores the result of an automatic probe that started when the record
// was at generation. The result is dropped when Generation has moved on since
// then or the model is under manual control. Only the reason
// "model_unavailable" disables a model and moves it to the daily schedule; any
// other failure of an enabled model is recorded with an hourly retry. A
// successful probe stores effort when it names a level and clears the stored
// level when effort.Reset is set.
func (s *AvailabilityRegistry) Record(model string, generation uint64, success bool, reason, channel string, now time.Time, effort EffortProbe) error {
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

// saveLocked rewrites the registry file through a temporary file and a rename.
// It is a no-op without a path. The caller holds s.mu for writing.
func (s *AvailabilityRegistry) saveLocked() error {
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
