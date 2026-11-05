package gateway

import (
	"testing"
	"time"

	"opencode2api/internal/freequota"
)

func TestApplyFreeQuotaDefaults(t *testing.T) {
	assumed := FreeQuotaEgress{}
	applyFreeQuotaDefaults(&assumed, 200)
	if assumed.Limit != 200 || assumed.LimitSource != "assumed" || assumed.Confidence != "low" {
		t.Fatalf("assumed defaults = %+v", assumed)
	}
	if assumed.History == nil {
		t.Fatal("assumed row history is nil")
	}

	unknown := FreeQuotaEgress{}
	applyFreeQuotaDefaults(&unknown, 0)
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
	if report.UnobservedEgresses != 1 || report.AssumedEgresses != 2 || report.RemainingKnownEgresses != 1 || report.RemainingKnown != 199 {
		t.Fatalf("counters = %+v", report)
	}
}
