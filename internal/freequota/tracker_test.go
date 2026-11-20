package freequota

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

var freeLimitBody = []byte(`{"error":{"message":"Free usage limit reached.","type":"FreeUsageLimitError"}}`)

func at(day int, hour int) time.Time {
	return time.Date(2026, 11, day, hour, 0, 0, 0, time.UTC)
}

func newTracker(t *testing.T, assumed int) *Tracker {
	t.Helper()
	tracker := Open(filepath.Join(t.TempDir(), "state.json"), Options{AssumedDailyLimit: assumed})
	tracker.now = func() time.Time { return at(16, 12) }
	return tracker
}

func ok(tracker *Tracker, egress string, when time.Time) {
	tracker.Record(Event{Time: when, Egress: egress, Status: 200})
}

func quota(tracker *Tracker, egress string, when time.Time) {
	tracker.Record(Event{Time: when, Egress: egress, Status: 429, Body: freeLimitBody,
		RetryAfter: nextReset(when).Sub(when)})
}

func TestOnlyAcceptedRequestsCount(t *testing.T) {
	tracker := newTracker(t, 200)
	ok(tracker, "p1", at(16, 9))
	ok(tracker, "p1", at(16, 9))
	tracker.Record(Event{Time: at(16, 9), Egress: "p1", Status: 400})
	tracker.Record(Event{Time: at(16, 9), Egress: "p1", Status: 403})
	tracker.Record(Event{Time: at(16, 9), Egress: "p1", TransportError: true})
	tracker.Record(Event{Time: at(16, 9), Egress: "p1", Status: 429})

	got := tracker.Snapshot()
	if got.TotalUsed != 2 {
		t.Fatalf("TotalUsed = %d, want 2", got.TotalUsed)
	}
	if len(got.Scopes) != 1 || got.Scopes[0].Used != 2 {
		t.Fatalf("scopes = %+v, want one scope with Used 2", got.Scopes)
	}
	if got.Scopes[0].Limit != 200 || got.Scopes[0].LimitSource != "assumed" {
		t.Fatalf("limit = %d/%s, want 200/assumed", got.Scopes[0].Limit, got.Scopes[0].LimitSource)
	}
}

func TestRefusalWithoutUsageTeachesNothing(t *testing.T) {
	tracker := newTracker(t, 200)
	quota(tracker, "p1", at(16, 9))

	got := tracker.Snapshot()
	if len(got.Scopes) != 0 {
		t.Fatalf("scopes = %+v, want none: a refusal on a fresh egress is not evidence", got.Scopes)
	}
}

func TestRefusalBelowAssumedTeachesNothing(t *testing.T) {
	tracker := newTracker(t, 200)
	for i := 0; i < 7; i++ {
		ok(tracker, "p1", at(16, 9))
	}
	quota(tracker, "p1", at(16, 10))

	got := tracker.Snapshot()
	if len(got.Scopes) != 1 {
		t.Fatalf("scopes = %+v, want one", got.Scopes)
	}
	scope := got.Scopes[0]
	if scope.Exhausted || scope.Limit != 200 || scope.LimitSource != "assumed" {
		t.Fatalf("scope = %+v, want an untouched assumed limit: a refusal under the allowance may be session-scoped", scope)
	}
	if scope.Remaining == nil || *scope.Remaining != 193 {
		t.Fatalf("remaining = %v, want 193", scope.Remaining)
	}
}

func TestRefusalAtAssumedLimitLearns(t *testing.T) {
	tracker := newTracker(t, 200)
	for i := 0; i < 200; i++ {
		ok(tracker, "p1", at(16, 9))
	}
	quota(tracker, "p1", at(16, 10))

	scope := tracker.Snapshot().Scopes[0]
	if scope.Limit != 200 || scope.LimitSource != "learned" {
		t.Fatalf("limit = %d/%s, want 200/learned", scope.Limit, scope.LimitSource)
	}
	if !scope.Exhausted {
		t.Fatal("scope not marked exhausted")
	}
	if scope.Remaining == nil || *scope.Remaining != 0 {
		t.Fatalf("remaining = %v, want 0", scope.Remaining)
	}
}

func TestShortRetryAfterDoesNotTeach(t *testing.T) {
	tracker := newTracker(t, 200)
	ok(tracker, "p1", at(16, 9))
	tracker.Record(Event{Time: at(16, 10), Egress: "p1", Status: 429, Body: freeLimitBody,
		RetryAfter: 30 * time.Second})

	scope := tracker.Snapshot().Scopes[0]
	if scope.Exhausted || scope.Limit != 200 || scope.LimitSource != "assumed" {
		t.Fatalf("scope = %+v, want an untouched assumed limit", scope)
	}
}

func TestOffMidnightRefusalDoesNotTeach(t *testing.T) {
	tracker := newTracker(t, 200)
	ok(tracker, "p1", at(16, 9))
	tracker.Record(Event{Time: at(16, 10), Egress: "p1", Status: 429, Body: freeLimitBody,
		RetryAfter: 8 * time.Hour})

	scope := tracker.Snapshot().Scopes[0]
	if scope.Exhausted || scope.Limit != 200 || scope.LimitSource != "assumed" {
		t.Fatalf("scope = %+v, want an untouched assumed limit", scope)
	}
}

func TestTransientRefusalClearsOnSuccess(t *testing.T) {
	tracker := newTracker(t, 200)
	for i := 0; i < 200; i++ {
		ok(tracker, "p1", at(16, 9))
	}
	quota(tracker, "p1", at(16, 10))
	if !tracker.Snapshot().Scopes[0].Exhausted {
		t.Fatal("expected exhausted after refusal")
	}
	ok(tracker, "p1", at(16, 10))

	scope := tracker.Snapshot().Scopes[0]
	if scope.Exhausted {
		t.Fatal("a later success must clear the exhaustion")
	}
	if scope.Limit != 201 || scope.LimitSource != "learned" {
		t.Fatalf("limit = %d/%s, want the used floor 201", scope.Limit, scope.LimitSource)
	}
	if scope.Used != 201 {
		t.Fatalf("Used = %d, want 201", scope.Used)
	}
}

func TestLearnedLimitStaysOnItsEgress(t *testing.T) {
	tracker := newTracker(t, 200)
	for i := 0; i < 200; i++ {
		ok(tracker, "a", at(16, 9))
	}
	quota(tracker, "a", at(16, 10))
	ok(tracker, "b", at(16, 11))

	got := tracker.Snapshot()
	if got.TotalUsed != 201 {
		t.Fatalf("TotalUsed = %d, want 201", got.TotalUsed)
	}
	for _, scope := range got.Scopes {
		switch scope.Egress {
		case "a":
			if scope.Limit != 200 || scope.LimitSource != "learned" || !scope.Exhausted {
				t.Fatalf("a = %+v, want exhausted with the learned 200", scope)
			}
		case "b":
			if scope.Limit != 200 || scope.LimitSource != "assumed" || scope.Exhausted {
				t.Fatalf("b = %+v, want the configured assumed limit", scope)
			}
			if scope.Remaining == nil || *scope.Remaining != 199 {
				t.Fatalf("b remaining = %v, want 199", scope.Remaining)
			}
		default:
			t.Fatalf("unexpected scope %+v", scope)
		}
	}
}

func TestDayRolloverKeepsLearnedLimit(t *testing.T) {
	tracker := newTracker(t, 200)
	for i := 0; i < 200; i++ {
		ok(tracker, "p1", at(16, 9))
	}
	quota(tracker, "p1", at(16, 10))

	tracker.now = func() time.Time { return at(17, 1) }
	if got := tracker.Snapshot(); len(got.Scopes) != 0 || got.TotalUsed != 0 {
		t.Fatalf("snapshot = %+v, want no rows before the first request of the new day", got)
	}
	ok(tracker, "p1", at(17, 1))

	got := tracker.Snapshot()
	if len(got.Scopes) != 1 {
		t.Fatalf("scopes = %+v, want one", got.Scopes)
	}
	scope := got.Scopes[0]
	if scope.Used != 1 || scope.Exhausted {
		t.Fatalf("scope = %+v, want a fresh day", scope)
	}
	if scope.Limit != 200 || scope.LimitSource != "learned" {
		t.Fatalf("limit = %d/%s, want the learned 200 to survive midnight", scope.Limit, scope.LimitSource)
	}
	if scope.Remaining == nil || *scope.Remaining != 199 {
		t.Fatalf("remaining = %v, want 199", scope.Remaining)
	}
}

func TestBackdatedEventIgnored(t *testing.T) {
	tracker := newTracker(t, 200)
	for i := 0; i < 3; i++ {
		ok(tracker, "p1", at(16, 9))
	}
	ok(tracker, "p1", at(15, 9))
	quota(tracker, "p1", at(15, 10))

	got := tracker.Snapshot()
	if got.TotalUsed != 3 || len(got.Scopes) != 1 {
		t.Fatalf("snapshot = %+v, want yesterday's events ignored", got)
	}
	if got.Scopes[0].Exhausted {
		t.Fatal("a backdated refusal must not exhaust today")
	}
}

func TestUnknownAssumedLimit(t *testing.T) {
	tracker := newTracker(t, 0)
	ok(tracker, "p1", at(16, 9))

	scope := tracker.Snapshot().Scopes[0]
	if scope.Limit != 0 || scope.LimitSource != "unknown" || scope.Remaining != nil {
		t.Fatalf("scope = %+v, want an unknown limit with no remaining", scope)
	}
}

func TestConfigureKeepsCounts(t *testing.T) {
	tracker := newTracker(t, 200)
	for i := 0; i < 3; i++ {
		ok(tracker, "p1", at(16, 9))
	}
	tracker.Configure(Options{AssumedDailyLimit: 50})

	scope := tracker.Snapshot().Scopes[0]
	if scope.Used != 3 || scope.Limit != 50 || scope.LimitSource != "assumed" {
		t.Fatalf("scope = %+v, want today's count with the new prior", scope)
	}
}

func TestPersistenceReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	tracker := Open(path, Options{AssumedDailyLimit: 200})
	tracker.now = func() time.Time { return at(16, 12) }
	for i := 0; i < 200; i++ {
		ok(tracker, "warp", at(16, 9))
	}
	quota(tracker, "warp", at(16, 10))
	tracker.Close()

	reloaded := Open(path, Options{AssumedDailyLimit: 200})
	reloaded.now = func() time.Time { return at(16, 12) }
	scope := reloaded.Snapshot().Scopes[0]
	if scope.Used != 200 || scope.Limit != 200 || !scope.Exhausted {
		t.Fatalf("scope = %+v, want used 200, learned 200, exhausted", scope)
	}
}

func TestConcurrentRecordAndSnapshot(t *testing.T) {
	tracker := newTracker(t, 200)
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok(tracker, "p1", at(16, 9))
		}()
	}
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tracker.Snapshot()
		}()
	}
	wg.Wait()

	if got := tracker.Snapshot(); got.TotalUsed != 100 {
		t.Fatalf("TotalUsed = %d, want 100", got.TotalUsed)
	}
}

func TestCorruptStateIsNotFatal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	tracker := Open(path, Options{AssumedDailyLimit: 200})
	tracker.now = func() time.Time { return at(16, 12) }
	ok(tracker, "p1", at(16, 9))
	if tracker.Snapshot().TotalUsed != 1 {
		t.Fatal("tracker did not start empty after a corrupt file")
	}
}
