package gateway

import (
	"context"

	"errors"

	"strings"

	"time"

	"opencode2api/internal/config"
	"opencode2api/internal/models"
)

func (m *RuntimeManager) AvailabilitySnapshot() []models.Availability {
	items := []models.Availability{}
	hiddenItems := []models.Availability{}
	r := m.current.Load()
	if r == nil {
		return items
	}
	for _, model := range r.gateway.catalog.ListAll() {
		hidden := r.gateway.catalog.IsHidden(model)
		// The page probes free models, but it is also the only place to unhide
		// a model, so hidden ones are listed even when they are not free.
		if !hidden && !r.gateway.catalog.IsFreeModel(model) {
			continue
		}
		item := r.availability.Get(model)
		item.Hidden = hidden
		// Hidden models are not probed and cannot be routed to, but this page is
		// the only place to unhide them, so they stay listed at the tail.
		if hidden {
			hiddenItems = append(hiddenItems, item)
			continue
		}
		items = append(items, item)
	}
	return append(items, hiddenItems...)
}

func (m *RuntimeManager) RestoreModel(model string) error {
	m.updateMu.Lock()
	defer m.updateMu.Unlock()
	r := m.current.Load()
	if r == nil {
		return errors.New("gateway runtime is unavailable")
	}
	for _, item := range m.AvailabilitySnapshot() {
		if item.Model == model {
			return r.availability.Restore(model, time.Now().UTC())
		}
	}
	return errors.New("unknown free model")
}

// SetManualModel switches a free model between manual and automatic control.
// Manual models stay enabled and are excluded from automatic probing.
func (m *RuntimeManager) SetManualModel(model string, manual bool) error {
	m.updateMu.Lock()
	defer m.updateMu.Unlock()
	r := m.current.Load()
	if r == nil {
		return errors.New("gateway runtime is unavailable")
	}
	for _, item := range m.AvailabilitySnapshot() {
		if item.Model == model {
			return r.availability.SetManual(model, manual, time.Now().UTC())
		}
	}
	return errors.New("unknown free model")
}

// SetHiddenModel adds or removes a model from the operator blocklist. The model
// does not have to exist yet: pre-hiding an upcoming model is allowed, which is
// why the ID is not validated against the catalog. Update() re-reads the live
// configuration and persists the change, so a console save racing this call
// cannot drop the edit.
func (m *RuntimeManager) SetHiddenModel(model string, hidden bool) error {
	model = strings.TrimSpace(model)
	if model == "" {
		return errors.New("model is required")
	}
	_, err := m.Update(func(candidate *config.Config) error {
		next := make([]string, 0, len(candidate.Models.Hidden)+1)
		found := false
		for _, item := range candidate.Models.Hidden {
			if item == model {
				found = true
				if !hidden {
					continue
				}
			}
			next = append(next, item)
		}
		if hidden && !found {
			next = append(next, model)
		}
		candidate.Models.Hidden = next
		return nil
	})
	return err
}

func (m *RuntimeManager) startAvailabilityChecks() {
	ctx, cancel := context.WithCancel(m.root)
	m.availabilityCancel = cancel
	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			m.checkFreeModels(ctx)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

func (m *RuntimeManager) checkFreeModels(ctx context.Context) {
	for _, item := range m.AvailabilitySnapshot() {
		if ctx.Err() != nil {
			return
		}
		// The operator blocklist is a true blocklist: a hidden model is not
		// exposed, not routed, and not probed. Skipping it here keeps a model
		// nobody can call from generating upstream traffic every minute.
		if item.Hidden {
			continue
		}
		r := m.current.Load()
		if r == nil {
			return
		}
		item = r.availability.Get(item.Model)
		if item.Manual {
			continue
		}
		if time.Now().Before(item.NextCheck) {
			continue
		}
		result := r.gateway.probeFreeModel(ctx, item.Model)
		if !result.attempted || ctx.Err() != nil {
			continue
		}
		// Config changes may replace credentials or endpoints during a probe.
		m.updateMu.Lock()
		if m.current.Load() == r {
			if err := r.availability.Record(item.Model, item.Generation, result.success, result.reason, result.channel, time.Now().UTC(), result.effort); err != nil {
				m.logger.Warn("model availability state save failed", "error", err)
			}
			m.logger.Info("free model availability checked", "component", "models", "event", "availability_checked", "model", item.Model, "available", result.success, "reason", result.reason, "channel", result.channel)
		}
		m.updateMu.Unlock()
	}
}
