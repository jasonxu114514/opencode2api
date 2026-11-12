package gateway

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"sync"
	"time"

	"opencode2api/internal/config"
	"opencode2api/internal/httpx"
	"opencode2api/internal/identity"
	"opencode2api/internal/models"
	"opencode2api/internal/telemetry"
)

// freeUsageLimitMarkers are the error bodies the free tier returns when a
// session or egress has spent its free-model allowance. The type name is stable
// across the wordings observed so far; the message is the human form.
var freeUsageLimitMarkers = []string{"freeusagelimiterror", "free usage limit"}

// isFreeUsageLimitResponse reports whether resp is the free tier's free-model
// allowance rejection. The body is peeked and restored, so the caller can still
// forward the response.
func isFreeUsageLimitResponse(resp *http.Response) bool {
	if resp == nil || resp.StatusCode != http.StatusTooManyRequests {
		return false
	}
	body := peekResponseBody(resp)
	if len(body) == 0 {
		return false
	}
	lower := bytes.ToLower(body)
	for _, marker := range freeUsageLimitMarkers {
		if bytes.Contains(lower, []byte(marker)) {
			return true
		}
	}
	return false
}

// sessionRemintKey marks a routing pass that already uses a re-minted session.
// Such a pass must not stop at the first free-limit 429, because the answer may
// still be exit-specific: the fresh session has to walk exits like any normal
// request, while a burned original session does not.
type sessionRemintKey struct{}

func withSessionRemint(ctx context.Context) context.Context {
	return context.WithValue(ctx, sessionRemintKey{}, true)
}

func sessionReminted(ctx context.Context) bool {
	reminted, _ := ctx.Value(sessionRemintKey{}).(bool)
	return reminted
}

// dailyQuotaSkew is how late a 429's Retry-After may land after the next UTC
// midnight for the rejection to be the exit's daily free-model allowance
// rather than a per-session throttle. The observed free-tier reset is a few
// minutes past midnight (5-7 min), so the window has to be wider than the
// tracker's tight 5-minute rule; the per-session throttles seen so far land
// 18+ minutes past, leaving a gap this window stays inside.
const dailyQuotaSkew = 15 * time.Minute

// freeLimitRepeatWindow is how long a second free-limit rejection on the same
// exit, for a different session, marks that exit as spent. A per-session
// throttle cannot answer two sessions, so a repeat proves the allowance
// belongs to the egress IP and survives a session re-mint.
const freeLimitRepeatWindow = 30 * time.Minute

// dailyQuotaReset reports whether resp is the free tier's daily allowance
// rejection for this egress, and when that allowance resets. The free-model
// quota is per egress IP and shared by every session leaving through it: the
// body is the same "Free usage limit reached" seen for a burned session, but
// its Retry-After points at the next UTC midnight. Re-minting the session
// cannot restore an egress's allowance, so this case must park the exit
// instead of triggering the session escape.
func dailyQuotaReset(resp *http.Response, now time.Time) (time.Time, bool) {
	if !isFreeUsageLimitResponse(resp) {
		return time.Time{}, false
	}
	retry := parseRetryAfter(resp.Header.Get("Retry-After"))
	// A hint under a minute is a transient throttle, not a daily reset;
	// treating it as exhaustion would park a healthy exit until midnight.
	if retry < time.Minute {
		return time.Time{}, false
	}
	utc := now.UTC()
	reset := time.Date(utc.Year(), utc.Month(), utc.Day()+1, 0, 0, 0, 0, time.UTC)
	delta := reset.Sub(now.Add(retry))
	if delta < 0 {
		delta = -delta
	}
	if delta > dailyQuotaSkew {
		return time.Time{}, false
	}
	return reset, true
}

// exitFreeLimitReset reports whether the free-limit 429 belongs to the exit
// and when it can serve again. The Retry-After naming the daily reset is the
// fast path. Otherwise only a repeat rejection for a different session proves
// the allowance is per egress: a single rejection may be the session's own
// burn, which the escape retry can clear by re-minting, so parking on it would
// sacrifice a healthy exit.
func exitFreeLimitReset(node *anonymousNode, resp *http.Response, ids identity.RequestIDs, now time.Time) (time.Time, bool) {
	if node == nil || node.proxy == nil || !isFreeUsageLimitResponse(resp) {
		return time.Time{}, false
	}
	if reset, daily := dailyQuotaReset(resp, now); daily {
		node.proxy.noteFreeLimit(ids.Session, now)
		return reset, true
	}
	if !node.proxy.noteFreeLimit(ids.Session, now) {
		return time.Time{}, false
	}
	retry := parseRetryAfter(resp.Header.Get("Retry-After"))
	if retry < time.Minute {
		// A repeat with no usable hint still means the exit is spent; bound
		// the park so it recovers on its own if the hint was wrong.
		retry = time.Hour
	}
	return now.Add(retry), true
}

// sessionScopedFreeLimit reports whether the attempt's free-tier allowance
// rejection belongs to the session rather than to the exit. The gateway
// re-mints a session it minted itself and retries once, so such a response must
// not cool the exit: a healthy relay would be parked and the retry could find
// no exit left, which would make the escape impossible on a small pool. A
// daily-quota 429 is excluded: that one belongs to the egress IP and is parked
// until its reset, not answered by a new session.
func sessionScopedFreeLimit(ids identity.RequestIDs, resp *http.Response, err error, now time.Time) bool {
	// The free tier ledges the free-model allowance per session id, and every
	// exit leaves from the same egress IP (the pool's tunnels differ, the
	// observed egress does not), so a free-limit 429 is the same answer on
	// every exit. A re-minted session is the only way past it, including for a
	// client-supplied session: a burned bucket has no prompt cache to keep.
	// The daily reset hint describes that session bucket, not the exit.
	return err == nil && isFreeUsageLimitResponse(resp)
}

// stopBurnedWalk reports whether the anonymous exit walk should stop on this
// response instead of trying the next exit. A free-limit rejection of the
// original session is identical on every exit, so walking cannot help; the
// re-minted retry pass keeps walking, because one particular exit may still
// reject the fresh session.
func stopBurnedWalk(ctx context.Context, ids identity.RequestIDs, resp *http.Response, err error, now time.Time) bool {
	return !sessionReminted(ctx) && sessionScopedFreeLimit(ids, resp, err, now)
}

// sessionEscapeLimit bounds the memo of burned-session replacements so callers
// that keep opening fresh conversations cannot grow it without bound.
const sessionEscapeLimit = 4096

type sessionEscapeKey struct{ session, child, parent string }

var sessionEscapes = struct {
	sync.Mutex
	entries map[sessionEscapeKey]sessionEscapeKey
	// spent marks a replacement that has already served a request. The free
	// tier meters its free-model bucket per session, and one request at the
	// observed sizes consumes it: replaying a spent session only earns a
	// guaranteed 429, so the chain advances before the request is sent.
	spent map[sessionEscapeKey]bool
}{entries: map[sessionEscapeKey]sessionEscapeKey{}, spent: map[sessionEscapeKey]bool{}}

func sessionEscapeKeyOf(ids identity.RequestIDs) sessionEscapeKey {
	return sessionEscapeKey{ids.Session, ids.ChildSession, ids.ParentSession}
}

// nextUnspentSession returns the next deterministic link after ids that has
// not already served a request. WithFreshSession is deterministic, so minting
// from the same link twice returns the same id; a chain that has advanced
// before must skip the links it already used or it would cycle between two
// spent ids and replay the 429 forever.
func nextUnspentSession(ids identity.RequestIDs) identity.RequestIDs {
	next := ids.WithFreshSession()
	for range sessionEscapeLimit {
		sessionEscapes.Lock()
		spent := sessionEscapes.spent[sessionEscapeKeyOf(next)]
		sessionEscapes.Unlock()
		if !spent {
			break
		}
		next = next.WithFreshSession()
	}
	return next
}

// escapedSession returns ids with every remembered replacement applied. One
// conversation can burn more than one session over its life, so the chain is
// followed until it ends. The second result reports whether anything changed.
//
// If the chain ends on a replacement that has already served a request, the
// next unused link is minted here and recorded, so the request is sent on a
// fresh bucket instead of replaying a 429 the upstream is certain to answer.
func escapedSession(ids identity.RequestIDs) (identity.RequestIDs, bool) {
	changed := false
	// Follow the whole chain: a long conversation burns one bucket per link,
	// so stopping early would hand back a session that is already spent.
	for range sessionEscapeLimit {
		key := sessionEscapeKeyOf(ids)
		sessionEscapes.Lock()
		value, ok := sessionEscapes.entries[key]
		spent := ok && sessionEscapes.spent[value]
		sessionEscapes.Unlock()
		if !ok {
			break
		}
		ids.Session, ids.ChildSession, ids.ParentSession = value.session, value.child, value.parent
		changed = true
		if !spent {
			continue
		}
		// The replacement answered one request already; a second request on
		// it is a guaranteed 429. Advance the chain now and reuse the new
		// link for the rest of the conversation.
		next := nextUnspentSession(ids)
		ids = next
		sessionEscapes.Lock()
		sessionEscapes.entries[key] = sessionEscapeKeyOf(next)
		delete(sessionEscapes.spent, value)
		sessionEscapes.Unlock()
		break
	}
	return ids, changed
}

// rememberSessionEscape records that the free tier rejected original's session
// and that escaped works. Once the memo is full it is dropped whole, which only
// costs the oldest conversations one fresh escape.
func rememberSessionEscape(original, escaped identity.RequestIDs) {
	key := sessionEscapeKeyOf(original)
	target := sessionEscapeKeyOf(escaped)
	sessionEscapes.Lock()
	defer sessionEscapes.Unlock()
	if _, exists := sessionEscapes.entries[key]; !exists && len(sessionEscapes.entries) >= sessionEscapeLimit {
		sessionEscapes.entries = map[sessionEscapeKey]sessionEscapeKey{}
		sessionEscapes.spent = map[sessionEscapeKey]bool{}
	}
	sessionEscapes.entries[key] = target
}

// advanceSessionAfterSuccess records that used has served its one request and
// that the next turn should start on its successor. The free tier meters per
// session, so without the recorded successor a conversation reusing the
// client's id would pay a guaranteed 429 on every turn after the first.
func advanceSessionAfterSuccess(used identity.RequestIDs) {
	next := nextUnspentSession(used)
	rememberSessionEscape(used, next)
	markSessionEscapeSpent(used)
}

// markSessionEscapeSpent records that a replacement already served a request,
// so escapedSession rotates it instead of replaying the burn.
func markSessionEscapeSpent(escaped identity.RequestIDs) {
	sessionEscapes.Lock()
	defer sessionEscapes.Unlock()
	if len(sessionEscapes.spent) >= sessionEscapeLimit {
		// The marks are only an optimization: dropping them costs at most one
		// rediscovered 429 per chain, so the map never grows without bound.
		sessionEscapes.spent = map[sessionEscapeKey]bool{}
	}
	sessionEscapes.spent[sessionEscapeKeyOf(escaped)] = true
}

// escapeBurnedSession retries one routing pass with a re-minted session when
// the last response is the free tier's usage rejection and the session was one
// the gateway minted itself. The burn follows the session id, not the egress:
// every exit answers the same 429, so rotating exits cannot help, while a fresh
// session id is a fresh bucket. Re-minting is skipped for diagnostics (a probe
// measures the request it was given) and for title requests (answered
// locally). The retry's attempt count is added to the total so telemetry keeps
// numbering one request's attempts in sequence.
func (g *Gateway) escapeBurnedSession(ctx context.Context, route models.Route, bodies map[config.Tier][]byte, ids identity.RequestIDs, resp *http.Response, effectiveRoute models.Route, attempts int) (*http.Response, models.Route, int, error) {
	if isDiagnosticRequest(ctx) || ctx.Err() != nil {
		return resp, effectiveRoute, attempts, nil
	}
	if !sessionScopedFreeLimit(ids, resp, nil, time.Now()) {
		return resp, effectiveRoute, attempts, nil
	}
	if isTitleGenerationBody(bodies[config.TierZen]) || isTitleGenerationBody(bodies[route.Tier]) {
		return resp, effectiveRoute, attempts, nil
	}
	original := *resp
	originalBody, readErr := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	httpx.DrainAndClose(resp.Body)
	escaped := nextUnspentSession(ids)
	if meta := telemetry.MetaFromContext(ctx); meta != nil {
		meta.Session = escaped.Session
	}
	g.logger.Info("free tier burned this session; retrying once with a fresh session",
		"component", "upstream", "event", "free_limit_session_retry", "request_id", ids.Request,
		"model", route.ID, "session", ids.Session, "fresh_session", escaped.Session,
		"retry_after", resp.Header.Get("Retry-After"))
	retryResp, retryRoute, used, retryErr := g.doUpstreamTiers(withSessionRemint(ctx), route, bodies, escaped, attempts)
	attempts += used
	if retryErr == nil && retryResp != nil && retryResp.StatusCode/100 == 2 {
		rememberSessionEscape(ids, escaped)
		// The free tier gave this bucket one request. Marking it spent makes
		// the next request in the conversation rotate before sending instead
		// of spending an attempt on a guaranteed 429.
		markSessionEscapeSpent(escaped)
		return retryResp, retryRoute, attempts, nil
	}
	if retryErr != nil && ctx.Err() != nil {
		if retryResp != nil {
			httpx.DrainAndClose(retryResp.Body)
		}
		return nil, effectiveRoute, attempts, retryErr
	}
	if retryResp == nil || retryErr != nil {
		// The retry produced no usable response and the original body was
		// consumed to free its connection. Hand the client the real upstream
		// cause instead of a gateway error.
		if retryResp != nil {
			httpx.DrainAndClose(retryResp.Body)
		}
		if readErr == nil {
			original.Body = io.NopCloser(bytes.NewReader(originalBody))
		} else {
			original.Body = http.NoBody
		}
		return &original, effectiveRoute, attempts, nil
	}
	return retryResp, retryRoute, attempts, retryErr
}
