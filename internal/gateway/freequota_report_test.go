package gateway

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"opencode2api/internal/freequota"
)

func TestApplyFreeQuotaDefaults(t *testing.T) {
	assumed := FreeQuotaEgress{}
	applyFreeQuotaDefaults(&assumed, 200, 0)
	if assumed.Limit != 200 || assumed.LimitSource != "assumed" || assumed.Confidence != "low" {
		t.Fatalf("assumed defaults = %+v", assumed)
	}
	if assumed.History == nil {
		t.Fatal("assumed row history is nil")
	}

	shared := FreeQuotaEgress{}
	applyFreeQuotaDefaults(&shared, 200, 150)
	if shared.Limit != 150 || shared.LimitSource != "assumed" || shared.Confidence != "low" {
		t.Fatalf("shared defaults = %+v, want 150 assumed", shared)
	}
	if shared.Remaining != nil {
		t.Fatalf("shared row remaining = %v, want nil", shared.Remaining)
	}

	unknown := FreeQuotaEgress{}
	applyFreeQuotaDefaults(&unknown, 0, 0)
	if unknown.Limit != 0 || unknown.LimitSource != "unknown" || unknown.Confidence != "none" {
		t.Fatalf("unknown defaults = %+v", unknown)
	}
}

func TestFreeQuotaReportJoinsUnknownScope(t *testing.T) {
	tracker := freequota.Open("", freequota.Options{AssumedDailyLimit: 200})
	t.Cleanup(tracker.Close)
	tracker.Record(freequota.Event{Time: time.Now().UTC(), Egress: "gone", Model: "beam-free", Status: 200})

	proxy := &proxyTransport{index: 0, name: "direct"}
	proxy.healthy.Store(true)
	gw := &Gateway{transports: &proxyTransports{items: []*proxyTransport{proxy}}}
	controller := &RuntimeController{freeQuota: tracker}
	controller.current.Store(&gatewayRuntime{gateway: gw})

	report := controller.FreeQuotaReport()
	if len(report.Egresses) != 2 {
		t.Fatalf("egresses = %d, want 2", len(report.Egresses))
	}
	var gone, direct *FreeQuotaEgress
	for index := range report.Egresses {
		switch report.Egresses[index].Label {
		case "gone":
			gone = &report.Egresses[index]
		case "direct":
			direct = &report.Egresses[index]
		}
	}
	if gone == nil || gone.Kind != "unknown" || !gone.Seen || gone.Used != 1 {
		t.Fatalf("unknown scope row = %+v", gone)
	}
	if direct == nil || direct.Seen || direct.LimitSource != "assumed" || direct.Limit != 200 {
		t.Fatalf("unobserved direct row = %+v", direct)
	}
	if report.UnobservedEgresses != 1 || report.AssumedEgresses != 1 || report.RemainingKnownEgresses != 1 || report.RemainingKnown != 199 {
		t.Fatalf("counters = %+v", report)
	}
}

func TestFreeQuotaReportSharesLearnedCeilingWithUnseenEgress(t *testing.T) {
	path := filepath.Join(t.TempDir(), "freequota.json")
	now := time.Now().UTC()
	state := map[string]any{
		"version": 2,
		"days": map[string]any{
			now.Format("2006-01-02"): map[string]any{
				"scopes": map[string]any{
					"gone": map[string]any{"used": 5, "ceiling": 150},
				},
			},
		},
	}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	tracker := freequota.Open(path, freequota.Options{AssumedDailyLimit: 200})
	t.Cleanup(tracker.Close)

	proxy := &proxyTransport{index: 0, name: "direct"}
	proxy.healthy.Store(true)
	gw := &Gateway{transports: &proxyTransports{items: []*proxyTransport{proxy}}}
	controller := &RuntimeController{freeQuota: tracker}
	controller.current.Store(&gatewayRuntime{gateway: gw})

	report := controller.FreeQuotaReport()
	var direct *FreeQuotaEgress
	for index := range report.Egresses {
		if report.Egresses[index].Label == "direct" {
			direct = &report.Egresses[index]
		}
	}
	if direct == nil {
		t.Fatal("direct row missing")
	}
	if direct.Seen || direct.Limit != 150 || direct.LimitSource != "assumed" || direct.Confidence != "low" {
		t.Fatalf("direct row = %+v, want unseen shared 150 assumed", direct)
	}
	if direct.Remaining != nil {
		t.Fatalf("direct remaining = %v, want nil", direct.Remaining)
	}
	if report.UnobservedEgresses != 1 || report.AssumedEgresses != 0 || report.LearnedEgresses != 1 {
		t.Fatalf("counters = %+v", report)
	}
	if report.RemainingKnown != 145 || report.RemainingKnownEgresses != 1 {
		t.Fatalf("remaining = %d over %d, want 145 over 1", report.RemainingKnown, report.RemainingKnownEgresses)
	}
}
