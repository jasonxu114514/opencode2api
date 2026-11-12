package gateway

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"sync"

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

// sessionScopedFreeLimit reports whether the attempt's free-tier allowance
// rejection can belong to the session rather than to the exit. The gateway
// re-mints a session it minted itself and retries once, so such a response must
// not cool the exit: a healthy relay would be parked and the retry could find
// no exit left, which would make the escape impossible on a small pool.
func sessionScopedFreeLimit(ids identity.RequestIDs, resp *http.Response, err error) bool {
	return err == nil && ids.SessionDerived && isFreeUsageLimitResponse(resp)
}

// sessionEscapeLimit bounds the memo of burned-session replacements so callers
// that keep opening fresh conversations cannot grow it without bound.
const sessionEscapeLimit = 4096

type sessionEscapeKey struct{ session, child, parent string }

type sessionEscapeValue struct{ session, child, parent string }

var sessionEscapes = struct {
	sync.Mutex
	entries map[sessionEscapeKey]sessionEscapeValue
}{entries: map[sessionEscapeKey]sessionEscapeValue{}}

func sessionEscapeKeyOf(ids identity.RequestIDs) sessionEscapeKey {
	return sessionEscapeKey{ids.Session, ids.ChildSession, ids.ParentSession}
}

// escapedSession returns ids with every remembered replacement applied. One
// conversation can burn more than one session over its life, so the chain is
// followed until it ends. The second result reports whether anything changed.
func escapedSession(ids identity.RequestIDs) (identity.RequestIDs, bool) {
	changed := false
	for range 4 {
		sessionEscapes.Lock()
		value, ok := sessionEscapes.entries[sessionEscapeKeyOf(ids)]
		sessionEscapes.Unlock()
		if !ok {
			break
		}
		ids.Session, ids.ChildSession, ids.ParentSession = value.session, value.child, value.parent
		changed = true
	}
	return ids, changed
}

// rememberSessionEscape records that the free tier rejected original's session
// and that escaped works. Once the memo is full it is dropped whole, which only
// costs the oldest conversations one fresh escape.
func rememberSessionEscape(original, escaped identity.RequestIDs) {
	key := sessionEscapeKeyOf(original)
	sessionEscapes.Lock()
	defer sessionEscapes.Unlock()
	if _, exists := sessionEscapes.entries[key]; !exists && len(sessionEscapes.entries) >= sessionEscapeLimit {
		sessionEscapes.entries = map[sessionEscapeKey]sessionEscapeValue{}
	}
	sessionEscapes.entries[key] = sessionEscapeValue{escaped.Session, escaped.ChildSession, escaped.ParentSession}
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
	if !ids.SessionDerived || isDiagnosticRequest(ctx) || ctx.Err() != nil {
		return resp, effectiveRoute, attempts, nil
	}
	if !isFreeUsageLimitResponse(resp) {
		return resp, effectiveRoute, attempts, nil
	}
	if isTitleGenerationBody(bodies[config.TierZen]) || isTitleGenerationBody(bodies[route.Tier]) {
		return resp, effectiveRoute, attempts, nil
	}
	original := *resp
	originalBody, readErr := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	httpx.DrainAndClose(resp.Body)
	escaped := ids.WithFreshSession()
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
