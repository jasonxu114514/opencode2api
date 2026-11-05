package gateway

import (
	"time"

	"opencode2api/internal/config"
	"opencode2api/internal/freequota"
)

// FreeQuotaEgress is one configured egress joined with today's tracker state.
// Every proxy and WARP exit appears even when it has carried no traffic, with
// health, block and cooldown state alongside the local estimate.
type FreeQuotaEgress struct {
	Index          int                    `json:"index"`
	Kind           string                 `json:"kind"`
	Label          string                 `json:"label"`
	Pool           bool                   `json:"pool"`
	Healthy        bool                   `json:"healthy"`
	HealthKnown    bool                   `json:"health_known"`
	Checking       bool                   `json:"checking"`
	Blocked        bool                   `json:"blocked"`
	BlockedUntil   *time.Time             `json:"blocked_until"`
	Cooling        bool                   `json:"cooling"`
	CoolingUntil   *time.Time             `json:"cooling_until"`
	ProbeSupported bool                   `json:"probe_supported"`
	Seen           bool                   `json:"seen"`
	Used           int                    `json:"used"`
	Limit          int                    `json:"limit"`
	LimitSource    string                 `json:"limit_source"`
	Remaining      *int                   `json:"remaining"`
	Confidence     string                 `json:"confidence"`
	Exhausted      bool                   `json:"exhausted"`
	ExhaustedUntil *time.Time             `json:"exhausted_until"`
	LastQuotaAt    *time.Time             `json:"last_quota_at"`
	RetryAfter     int                    `json:"retry_after_seconds"`
	LastStatus     int                    `json:"last_status"`
	LastModel      string                 `json:"last_model"`
	LastAttemptAt  *time.Time             `json:"last_attempt_at"`
	ObservedDays   int                    `json:"observed_days"`
	Successes      int                    `json:"successes"`
	LifetimeUsed   int                    `json:"lifetime_used"`
	Exhaustions    int                    `json:"exhaustions"`
	LimitFloor     int                    `json:"limit_floor"`
	LimitCeil      int                    `json:"limit_ceil"`
	History        []freequota.HistoryDay `json:"history"`
}

// FreeQuotaReport is the full free-quota view: today's estimate plus every
// configured egress, including the ones with no traffic.
type FreeQuotaReport struct {
	Enabled                bool              `json:"enabled"`
	AssumedDailyLimit      int               `json:"assumed_daily_limit"`
	Date                   string            `json:"date"`
	ResetsAt               time.Time         `json:"resets_at"`
	GeneratedAt            time.Time         `json:"generated_at"`
	TotalUsed              int               `json:"total_used"`
	Egresses               []FreeQuotaEgress `json:"egresses"`
	Warp                   *WarpStatus       `json:"warp"`
	RemainingKnown         int               `json:"remaining_known"`
	RemainingKnownEgresses int               `json:"remaining_known_egresses"`
	LearnedEgresses        int               `json:"learned_egresses"`
	AssumedEgresses        int               `json:"assumed_egresses"`
	UnknownEgresses        int               `json:"unknown_egresses"`
	UnobservedEgresses     int               `json:"unobserved_egresses"`
	ExhaustedEgresses      int               `json:"exhausted_egresses"`
}

// FreeQuotaReport joins the tracker snapshot with the runtime's configured
// egresses. A scope with no configured transport anymore is still reported as
// an unknown row so its usage does not silently disappear.
func (m *RuntimeController) FreeQuotaReport() FreeQuotaReport {
	snapshot := m.FreeQuotas()
	report := FreeQuotaReport{
		Enabled:           snapshot.Enabled,
		AssumedDailyLimit: snapshot.AssumedDailyLimit,
		Date:              snapshot.Date,
		ResetsAt:          snapshot.ResetsAt,
		GeneratedAt:       time.Now().UTC(),
		TotalUsed:         snapshot.TotalUsed,
		Egresses:          []FreeQuotaEgress{},
	}
	scopes := make(map[string]freequota.ScopeSnapshot, len(snapshot.Scopes))
	for _, scope := range snapshot.Scopes {
		scopes[scope.Egress] = scope
	}
	seen := make(map[string]bool, len(scopes))

	now := time.Now()
	runtime := m.current.Load()
	if runtime != nil && runtime.gateway != nil {
		gw := runtime.gateway
		cooling := make(map[int]time.Time)
		if gw.anonymous != nil {
			for _, node := range gw.anonymous.nodes {
				if node == nil || node.proxy == nil {
					continue
				}
				if until := node.cooldownUntil.Load(); until > now.UnixNano() {
					cooling[node.proxy.index] = time.Unix(0, until).UTC()
				}
			}
		}
		for _, proxy := range gw.transports.items {
			label := config.RedactURL(proxy.name)
			row := FreeQuotaEgress{
				Index: proxy.index, Kind: "proxy", Label: label, Pool: proxy.pool,
				Healthy: proxy.healthy.Load(), HealthKnown: !proxy.pool,
				Checking: proxy.checking.Load(), ProbeSupported: true,
			}
			applyFreeQuotaDefaults(&row, snapshot.AssumedDailyLimit)
			if label == "direct" {
				row.Kind = "direct"
			}
			if proxy.blocked(now) {
				row.Blocked = true
				until := time.Unix(0, proxy.blockedUntil.Load()).UTC()
				row.BlockedUntil = &until
			}
			if until, ok := cooling[proxy.index]; ok {
				row.Cooling = true
				row.CoolingUntil = &until
			}
			if scope, ok := scopes[label]; ok {
				applyFreeQuotaScope(&row, scope)
				seen[label] = true
			}
			report.Egresses = append(report.Egresses, row)
		}
		if gw.warp != nil {
			warpSnapshot := gw.warp.Snapshot()
			report.Warp = &WarpStatus{
				Enabled: true, Stack: warpSnapshot.Stack, Size: warpSnapshot.Size,
				Active: warpSnapshot.Active, Standby: warpSnapshot.Standby,
				Cooling: warpSnapshot.Cooling, Egress: warpSnapshot.Egress,
			}
			for _, key := range warpSnapshot.EgressKeys {
				label := "warp:" + key
				row := FreeQuotaEgress{
					Index: -1, Kind: "warp", Label: label, Healthy: true, HealthKnown: true,
				}
				applyFreeQuotaDefaults(&row, snapshot.AssumedDailyLimit)
				if scope, ok := scopes[label]; ok {
					applyFreeQuotaScope(&row, scope)
					seen[label] = true
				}
				report.Egresses = append(report.Egresses, row)
			}
		}
	}

	// Tracker scopes that no longer match a configured egress (a removed
	// proxy, or a WARP exit the pool retired) stay visible as unknown rows.
	for _, scope := range snapshot.Scopes {
		if seen[scope.Egress] {
			continue
		}
		row := FreeQuotaEgress{Index: -1, Kind: "unknown", Label: scope.Egress}
		applyFreeQuotaDefaults(&row, snapshot.AssumedDailyLimit)
		applyFreeQuotaScope(&row, scope)
		report.Egresses = append(report.Egresses, row)
	}

	for _, row := range report.Egresses {
		if !row.Seen {
			report.UnobservedEgresses++
		}
		if row.Remaining != nil {
			report.RemainingKnown += *row.Remaining
			report.RemainingKnownEgresses++
		}
		switch row.LimitSource {
		case "learned":
			report.LearnedEgresses++
		case "assumed":
			report.AssumedEgresses++
		default:
			report.UnknownEgresses++
		}
		if row.Exhausted {
			report.ExhaustedEgresses++
		}
	}
	return report
}

// applyFreeQuotaDefaults fills a row that has carried no evidence with the
// starting prior. The prior is a bound, not an observation, so the row stays
// unseen and carries no remaining: capacity only counts observed exits.
func applyFreeQuotaDefaults(row *FreeQuotaEgress, assumed int) {
	row.History = []freequota.HistoryDay{}
	if assumed > 0 {
		row.Limit = assumed
		row.LimitSource = "assumed"
		row.Confidence = "low"
		return
	}
	row.LimitSource = "unknown"
	row.Confidence = "none"
}

func applyFreeQuotaScope(row *FreeQuotaEgress, scope freequota.ScopeSnapshot) {
	row.Seen = true
	row.Used = scope.Used
	row.Limit = scope.Limit
	row.LimitSource = scope.LimitSource
	row.Remaining = scope.Remaining
	row.Confidence = scope.Confidence
	row.Exhausted = scope.Exhausted
	row.ExhaustedUntil = scope.ExhaustedUntil
	row.LastQuotaAt = scope.LastQuotaAt
	row.RetryAfter = scope.RetryAfterSeconds
	row.LastStatus = scope.LastStatus
	row.LastModel = scope.LastModel
	row.LastAttemptAt = scope.LastAttemptAt
	row.ObservedDays = scope.ObservedDays
	row.Successes = scope.Successes
	row.LifetimeUsed = scope.LifetimeUsed
	row.Exhaustions = scope.Exhaustions
	row.LimitFloor = scope.LimitFloor
	row.LimitCeil = scope.LimitCeil
	row.History = scope.History
}

// FreeProbeModel returns the free model an active probe should send. When
// preferred is a usable free model it is returned; otherwise the first
// anonymous free route is chosen. It reports false when no free model is
// currently routable.
func (m *RuntimeController) FreeProbeModel(preferred string) (string, bool) {
	runtime := m.current.Load()
	if runtime == nil || runtime.gateway == nil || runtime.gateway.catalog == nil {
		return "", false
	}
	gw := runtime.gateway
	if preferred != "" {
		if gw.catalog.IsFreeModel(preferred) && gw.catalog.Supported(preferred) {
			return preferred, true
		}
		return "", false
	}
	routes, _ := gw.availableModels()
	for _, route := range routes {
		if gw.catalog.IsFreeModel(route.ID) {
			return route.ID, true
		}
	}
	return "", false
}
