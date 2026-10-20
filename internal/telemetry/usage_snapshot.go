package telemetry

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// UsageSnapshotFile is the on-disk form of the per-server-key lifetime usage.
// Version lets a later change migrate old files instead of guessing; SavedAt
// tells an operator how stale the numbers are after a restart.
type UsageSnapshotFile struct {
	Version  int                       `json:"version"`
	SavedAt  time.Time                 `json:"saved_at"`
	Lifetime map[string]ServerKeyUsage `json:"lifetime"`
}

const usageSnapshotVersion = 1

// LoadServerKeyUsage reads a snapshot written by SaveServerKeyUsage. A missing
// file is not an error: a first start simply has no usage to restore. A file
// that cannot be parsed is reported so an operator can see that the numbers are
// starting from zero, but the caller is expected to continue serving.
func LoadServerKeyUsage(path string) (map[string]ServerKeyUsage, error) {
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read usage snapshot: %w", err)
	}
	var file UsageSnapshotFile
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("decode usage snapshot: %w", err)
	}
	if file.Version != usageSnapshotVersion {
		return nil, fmt.Errorf("usage snapshot version %d is not supported", file.Version)
	}
	return file.Lifetime, nil
}

// SaveServerKeyUsage writes the per-server-key lifetime usage atomically, so a
// crash mid-write cannot leave a truncated file that fails to load on the next
// start. It creates the parent directory, writes a temporary file in the same
// directory (which is what makes the rename atomic), and replaces the target.
func SaveServerKeyUsage(path string, lifetime map[string]ServerKeyUsage) error {
	if path == "" {
		return nil
	}
	file := UsageSnapshotFile{Version: usageSnapshotVersion, SavedAt: time.Now().UTC(), Lifetime: lifetime}
	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return fmt.Errorf("encode usage snapshot: %w", err)
	}
	data = append(data, '\n')
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create usage snapshot directory: %w", err)
	}
	temp, err := os.CreateTemp(dir, ".usage-*.tmp")
	if err != nil {
		return fmt.Errorf("create usage snapshot: %w", err)
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if _, err = temp.Write(data); err == nil {
		err = temp.Sync()
	}
	closeErr := temp.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("write usage snapshot: %w", err)
	}
	if err := os.Chmod(tempPath, 0o600); err != nil {
		return fmt.Errorf("set usage snapshot permissions: %w", err)
	}
	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("replace usage snapshot: %w", err)
	}
	return nil
}

// RestoreServerKeyUsage seeds the lifetime per-key counters from a snapshot.
// The rolling last-hour window is deliberately not restored: it is a live
// measurement.
func (m *Metrics) RestoreServerKeyUsage(lifetime map[string]ServerKeyUsage) {
	if len(lifetime) == 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for key, value := range lifetime {
		// Merge rather than assign: a request served before the restore ran
		// must not lose its counts.
		addServerKeyUsage(m.lifetimeUsage.ServerKeys, key, value)
	}
}

// usageSaver periodically writes the per-key lifetime counters and restores
// them on start, so a restart no longer resets metering.
type usageSaver struct {
	metrics *Metrics
	path    string
	logger  func(error)
	stop    chan struct{}
	done    chan struct{}
}

// StartUsagePersistence restores path into the lifetime counters and keeps the
// file up to date every interval. onError reports a failed write; the loop
// keeps running, because losing the snapshot must not stop the gateway. With
// an empty path, or an interval of zero or less, persistence is disabled.
func StartUsagePersistence(metrics *Metrics, path string, interval time.Duration, onError func(error)) *usageSaver {
	if metrics == nil || path == "" || interval <= 0 {
		return nil
	}
	lifetime, err := LoadServerKeyUsage(path)
	if err != nil && onError != nil {
		onError(err)
	}
	metrics.RestoreServerKeyUsage(lifetime)
	saver := &usageSaver{metrics: metrics, path: path, logger: onError, stop: make(chan struct{}), done: make(chan struct{})}
	go saver.loop(interval)
	return saver
}

func (s *usageSaver) loop(interval time.Duration) {
	defer close(s.done)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			s.save()
		case <-s.stop:
			return
		}
	}
}

// save writes the current lifetime counters. Lifetime only: the rolling window
// stays in memory.
func (s *usageSaver) save() {
	lifetime, _ := s.metrics.ServerKeyUsage()
	if err := SaveServerKeyUsage(s.path, lifetime); err != nil && s.logger != nil {
		s.logger(err)
	}
}

// Close stops the loop and writes one last snapshot, so a graceful shutdown
// does not lose the interval's tail.
func (s *usageSaver) Close() {
	if s == nil {
		return
	}
	select {
	case <-s.stop:
	default:
		close(s.stop)
	}
	<-s.done
	s.save()
}
