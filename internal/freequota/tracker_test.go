package freequota

import (
	"path/filepath"
	"testing"
	"time"
)

func newTestTracker(t *testing.T, assumed int) *Tracker {
	t.Helper()
	tracker := Open(filepath.Join(t.TempDir(), "freequota.json"), Options{AssumedDailyLimit: assumed})
	t.Cleanup(tracker.Close)
	return tracker
}

func recordSuccess(tracker *Tracker, when time.Time, egress, model string) {
	tracker.Record(Event{Time: when, Egress: egress, Model: model, Status: 200})
}

func recordQuota(tracker *Tracker, when time.Time, egress, model string) {
	tracker.Record(Event{
		Time:       when,
		Egress:     egress,
		Model:      model,
		Status:     429,
		RetryAfter: 10 * time.Minute,
		Body:       []byte(`{"error":{"type":"FreeUsageLimitError","message":"free usage limit reached"}}`),
	})
}

func findScope(t *testing.T, snapshot Snapshot, egress string) ScopeSnapshot {
	t.Helper()
	for _, scope := range snapshot.Scopes {
		if scope.Egress == egress {
			return scope
		}
	}
	t.Fatalf("scope %q not found", egress)
	return ScopeSnapshot{}
}

func TestGenuineQuota(t *testing.T) {
	when := time.Date(2026, 11, 5, 23, 50, 0, 0, time.UTC)
	body := []byte(`{"error":{"type":"FreeUsageLimitError","message":"free usage limit reached"}}`)
	cases := []struct {
		name       string
		status     int
		body       []byte
		retryAfter time.Duration
		want       bool
	}{
		{"daily reset hint", 429, body, 10 * time.Minute, true},
		{"short throttle", 429, body, 5 * time.Second, false},
		{"hint misses reset", 429, body, 90 * time.Second, false},
		{"plain 429", 429, []byte(`{"error":"rate limited"}`), 10 * time.Minute, false},
		{"success", 200, nil, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := GenuineQuota(tc.status, tc.body, tc.retryAfter, when); got != tc.want {
				t.Fatalf("GenuineQuota(%d, %q, %v) = %v, want %v", tc.status, tc.body, tc.retryAfter, got, tc.want)
			}
		})
	}
}

func TestClassifyAttempt(t *testing.T) {
	when := time.Date(2026, 11, 5, 23, 50, 0, 0, time.UTC)
	quotaBody := []byte(`FreeUsageLimitError`)
	cases := []struct {
		name         string
		status       int
		body         []byte
		retryAfter   time.Duration
		transportErr bool
		want         string
	}{
		{"success", 200, []byte(`{}`), 0, false, "ok"},
		{"transport failure", 0, nil, 0, true, "transport_error"},
		{"bad gateway", 502, nil, 0, false, "transport_error"},
		{"service unavailable", 503, nil, 0, false, "transport_error"},
		{"gateway timeout", 504, nil, 0, false, "transport_error"},
		{"unauthorized", 401, nil, 0, false, "auth"},
		{"forbidden", 403, nil, 0, false, "blocked"},
		{"genuine quota", 429, quotaBody, 10 * time.Minute, false, "quota"},
		{"same body wrong hint", 429, quotaBody, 90 * time.Second, false, "session_risk"},
		{"plain 429", 429, []byte(`{"error":"rate limited"}`), 0, false, "rate_limited"},
		{"bad request", 400, nil, 0, false, "error"},
		{"server error", 500, nil, 0, false, "error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyAttempt(tc.status, tc.body, tc.retryAfter, when, tc.transportErr); got != tc.want {
				t.Fatalf("ClassifyAttempt(%d, %q, %v, transport=%v) = %q, want %q",
					tc.status, tc.body, tc.retryAfter, tc.transportErr, got, tc.want)
			}
		})
	}
}

func TestAssumedPriorSnapshot(t *testing.T) {
	tracker := newTestTracker(t, 200)
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	tracker.now = func() time.Time { return now }

	for i := 0; i < 30; i++ {
		recordSuccess(tracker, now.Add(-time.Duration(i+1)*time.Minute), "e", "m")
	}

	scope := findScope(t, tracker.Snapshot(), "e")
	if scope.Limit != 200 || scope.LimitSource != "assumed" {
		t.Fatalf("limit = %d/%q, want 200/assumed", scope.Limit, scope.LimitSource)
	}
	if scope.Confidence != "low" {
		t.Fatalf("confidence = %q, want low", scope.Confidence)
	}
	if scope.Remaining == nil || *scope.Remaining != 170 {
		t.Fatalf("remaining = %v, want 170", scope.Remaining)
	}
	if scope.LimitFloor != 30 || scope.LimitCeil != 0 || scope.Exhaustions != 0 {
		t.Fatalf("floor=%d ceil=%d exhaustions=%d, want 30/0/0", scope.LimitFloor, scope.LimitCeil, scope.Exhaustions)
	}
}

func TestFloorRaisesAssumedPrior(t *testing.T) {
	tracker := newTestTracker(t, 200)
	now := time.Date(2026, 11, 5, 23, 0, 0, 0, time.UTC)
	tracker.now = func() time.Time { return now }

	for i := 0; i < 260; i++ {
		recordSuccess(tracker, now.Add(-time.Duration(i+1)*time.Minute), "e", "m")
	}

	scope := findScope(t, tracker.Snapshot(), "e")
	if scope.Limit != 260 || scope.LimitSource != "learned" {
		t.Fatalf("limit = %d/%q, want 260/learned", scope.Limit, scope.LimitSource)
	}
	if scope.Confidence != "low" {
		t.Fatalf("confidence = %q, want low", scope.Confidence)
	}
	if scope.LimitFloor != 260 || scope.LimitCeil != 0 || scope.Exhaustions != 0 {
		t.Fatalf("floor=%d ceil=%d exhaustions=%d, want 260/0/0", scope.LimitFloor, scope.LimitCeil, scope.Exhaustions)
	}
}

func TestCeilingReplacesAssumedPrior(t *testing.T) {
	tracker := newTestTracker(t, 200)
	quotaAt := time.Date(2026, 11, 5, 23, 50, 0, 0, time.UTC)
	now := quotaAt.Add(5 * time.Minute)
	tracker.now = func() time.Time { return now }

	for i := 0; i < 180; i++ {
		recordSuccess(tracker, quotaAt.Add(-time.Duration(i+1)*time.Minute), "e", "m")
	}
	recordQuota(tracker, quotaAt, "e", "m")

	scope := findScope(t, tracker.Snapshot(), "e")
	if scope.Limit != 180 || scope.LimitSource != "learned" {
		t.Fatalf("limit = %d/%q, want 180/learned", scope.Limit, scope.LimitSource)
	}
	if scope.LimitCeil != 180 || scope.LimitFloor != 180 || scope.Exhaustions != 1 {
		t.Fatalf("ceil=%d floor=%d exhaustions=%d, want 180/180/1", scope.LimitCeil, scope.LimitFloor, scope.Exhaustions)
	}
	if scope.Confidence != "medium" {
		t.Fatalf("confidence = %q, want medium", scope.Confidence)
	}
	if !scope.Exhausted || scope.ExhaustedUntil == nil {
		t.Fatalf("exhausted=%v until=%v, want true/non-nil", scope.Exhausted, scope.ExhaustedUntil)
	}
}

func TestZeroPriorOnlySuccessesStaysUnknown(t *testing.T) {
	tracker := newTestTracker(t, 0)
	now := time.Date(2026, 11, 5, 23, 0, 0, 0, time.UTC)
	tracker.now = func() time.Time { return now }

	for i := 0; i < 300; i++ {
		recordSuccess(tracker, now.Add(-time.Duration(i+1)*time.Minute), "e", "m")
	}

	scope := findScope(t, tracker.Snapshot(), "e")
	if scope.Limit != 0 || scope.LimitSource != "unknown" {
		t.Fatalf("limit = %d/%q, want 0/unknown", scope.Limit, scope.LimitSource)
	}
	if scope.Confidence != "none" {
		t.Fatalf("confidence = %q, want none", scope.Confidence)
	}
	if scope.Remaining != nil {
		t.Fatalf("remaining = %v, want nil", *scope.Remaining)
	}
	if scope.LimitFloor != 300 || scope.LimitCeil != 0 {
		t.Fatalf("floor=%d ceil=%d, want 300/0", scope.LimitFloor, scope.LimitCeil)
	}
}

func TestWideBoundsStayMedium(t *testing.T) {
	tracker := newTestTracker(t, 200)
	day1 := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	day2 := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	tracker.now = func() time.Time { return day2.Add(11*time.Hour + 55*time.Minute) }

	for i := 0; i < 70; i++ {
		recordSuccess(tracker, day1, "e", "m")
	}
	recordQuota(tracker, time.Date(2026, 11, 4, 23, 50, 0, 0, time.UTC), "e", "m")
	for i := 0; i < 90; i++ {
		recordSuccess(tracker, day2, "e", "m")
	}
	recordQuota(tracker, time.Date(2026, 11, 5, 23, 50, 0, 0, time.UTC), "e", "m")

	scope := findScope(t, tracker.Snapshot(), "e")
	if scope.LimitCeil != 70 || scope.LimitFloor != 90 || scope.Limit != 90 {
		t.Fatalf("ceil/floor/limit = %d/%d/%d, want 70/90/90", scope.LimitCeil, scope.LimitFloor, scope.Limit)
	}
	if scope.Exhaustions != 2 || scope.Confidence != "medium" {
		t.Fatalf("exhaustions=%d confidence=%q, want 2/medium", scope.Exhaustions, scope.Confidence)
	}
}

func TestMultipleDaysAggregatesAndHistory(t *testing.T) {
	tracker := newTestTracker(t, 0)
	day1 := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	day2 := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	day3 := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	tracker.now = func() time.Time { return day3.Add(11*time.Hour + 55*time.Minute) }

	for i := 0; i < 6; i++ {
		recordSuccess(tracker, day1, "e", "m")
	}
	recordQuota(tracker, time.Date(2026, 11, 1, 23, 50, 0, 0, time.UTC), "e", "m")
	for i := 0; i < 5; i++ {
		recordSuccess(tracker, day2, "e", "m")
	}
	recordQuota(tracker, time.Date(2026, 11, 2, 23, 50, 0, 0, time.UTC), "e", "m")
	for i := 0; i < 7; i++ {
		recordSuccess(tracker, day3, "e", "m")
	}

	scope := findScope(t, tracker.Snapshot(), "e")
	if scope.LimitCeil != 5 || scope.LimitFloor != 7 || scope.Limit != 7 {
		t.Fatalf("ceil/floor/limit = %d/%d/%d, want 5/7/7", scope.LimitCeil, scope.LimitFloor, scope.Limit)
	}
	if scope.ObservedDays != 3 || scope.Exhaustions != 2 || scope.Successes != 18 || scope.LifetimeUsed != 18 {
		t.Fatalf("observed=%d exhaustions=%d successes=%d lifetime=%d, want 3/2/18/18",
			scope.ObservedDays, scope.Exhaustions, scope.Successes, scope.LifetimeUsed)
	}
	if scope.Confidence != "high" {
		t.Fatalf("confidence = %q, want high", scope.Confidence)
	}
	if len(scope.History) != 3 || scope.History[0].Date != "2026-11-01" ||
		scope.History[1].Date != "2026-11-02" || scope.History[2].Date != "2026-11-03" {
		t.Fatalf("history = %+v, want three ascending days", scope.History)
	}
	if scope.History[0].Quota429 != 1 || scope.History[1].Quota429 != 1 || scope.History[2].Quota429 != 0 {
		t.Fatalf("history quotas = %+v, want 1/1/0", scope.History)
	}
}

func TestPersistenceReloadRetainsCounts(t *testing.T) {
	now := time.Now().UTC()
	path := filepath.Join(t.TempDir(), "freequota.json")
	first := Open(path, Options{AssumedDailyLimit: 200})
	first.now = func() time.Time { return now }
	for i := 0; i < 5; i++ {
		recordSuccess(first, now, "e", "m")
	}
	first.Close()

	second := Open(path, Options{AssumedDailyLimit: 200})
	t.Cleanup(second.Close)
	second.now = func() time.Time { return now }

	scope := findScope(t, second.Snapshot(), "e")
	if scope.Used != 5 || scope.Successes != 5 || scope.LifetimeUsed != 5 {
		t.Fatalf("used=%d successes=%d lifetime=%d, want 5/5/5", scope.Used, scope.Successes, scope.LifetimeUsed)
	}
	if scope.ObservedDays != 1 {
		t.Fatalf("observed_days = %d, want 1", scope.ObservedDays)
	}
}

func TestRecordBillableClassification(t *testing.T) {
	tracker := newTestTracker(t, 0)
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	tracker.now = func() time.Time { return now }
	quotaBody := []byte(`FreeUsageLimitError`)

	tracker.Record(Event{Time: now, Egress: "transport", Model: "m", TransportError: true})
	tracker.Record(Event{Time: now, Egress: "forbidden", Model: "m", Status: 403})
	tracker.Record(Event{Time: now, Egress: "session", Model: "m", Status: 429, RetryAfter: 90 * time.Second, Body: quotaBody})
	tracker.Record(Event{Time: now, Egress: "throttle", Model: "m", Status: 429})
	for _, status := range []int{400, 404, 529} {
		tracker.Record(Event{Time: now, Egress: "billable", Model: "m", Status: status})
	}
	recordSuccess(tracker, now, "billable", "m")

	snapshot := tracker.Snapshot()
	if scope := findScope(t, snapshot, "transport"); scope.Used != 0 || scope.LastStatus != 0 || scope.LastAttemptAt == nil {
		t.Fatalf("transport scope = %+v, want used 0, status 0, attempt set", scope)
	}
	if scope := findScope(t, snapshot, "forbidden"); scope.Used != 0 || scope.LastStatus != 403 {
		t.Fatalf("forbidden scope = %+v, want used 0, status 403", scope)
	}
	if scope := findScope(t, snapshot, "session"); scope.Used != 0 || scope.Exhaustions != 0 {
		t.Fatalf("session scope = %+v, want used 0, no exhaustion", scope)
	}
	if scope := findScope(t, snapshot, "throttle"); scope.Used != 1 || scope.Successes != 0 {
		t.Fatalf("throttle scope used=%d successes=%d, want 1/0", scope.Used, scope.Successes)
	}
	if scope := findScope(t, snapshot, "billable"); scope.Used != 4 || scope.Successes != 1 {
		t.Fatalf("billable scope used=%d successes=%d, want 4/1", scope.Used, scope.Successes)
	}
}

func TestSameModelSuccessClearsExhaustion(t *testing.T) {
	tracker := newTestTracker(t, 200)
	quotaAt := time.Date(2026, 11, 5, 23, 50, 0, 0, time.UTC)
	now := quotaAt.Add(5 * time.Minute)
	tracker.now = func() time.Time { return now }

	for i := 0; i < 3; i++ {
		recordSuccess(tracker, quotaAt.Add(-time.Duration(i+1)*time.Minute), "e", "m")
	}
	recordQuota(tracker, quotaAt, "e", "m")
	if scope := findScope(t, tracker.Snapshot(), "e"); !scope.Exhausted || scope.LimitCeil != 3 {
		t.Fatalf("before heal exhausted=%v ceil=%d, want true/3", scope.Exhausted, scope.LimitCeil)
	}

	recordSuccess(tracker, quotaAt.Add(2*time.Minute), "e", "m")
	scope := findScope(t, tracker.Snapshot(), "e")
	if scope.Exhausted || scope.ExhaustedUntil != nil || scope.LimitCeil != 0 {
		t.Fatalf("after heal exhausted=%v until=%v ceil=%d, want false/nil/0",
			scope.Exhausted, scope.ExhaustedUntil, scope.LimitCeil)
	}
	if scope.RetryAfterSeconds != 0 || scope.LastQuotaAt != nil {
		t.Fatalf("after heal retry=%d last_quota=%v, want 0/nil", scope.RetryAfterSeconds, scope.LastQuotaAt)
	}
}
