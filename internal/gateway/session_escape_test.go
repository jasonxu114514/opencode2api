package gateway

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"opencode2api/internal/identity"
)

func resetSessionEscapes() {
	sessionEscapes.Lock()
	defer sessionEscapes.Unlock()
	sessionEscapes.entries = map[sessionEscapeKey]sessionEscapeKey{}
	sessionEscapes.spent = map[sessionEscapeKey]bool{}
}

func TestIsFreeUsageLimitResponse(t *testing.T) {
	const freeLimitBody = `{"error":{"code":null,"message":"Free usage limit reached. Free models work in OpenCode","param":null,"type":"upstream_error"}}`
	resp := &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Body:       io.NopCloser(strings.NewReader(freeLimitBody)),
	}
	if !isFreeUsageLimitResponse(resp) {
		t.Fatal("expected the free usage limit body to be recognized")
	}
	rest, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading restored body: %v", err)
	}
	if string(rest) != freeLimitBody {
		t.Fatalf("body was not restored: got %q", rest)
	}

	rateLimit := &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"rate limit"}}`)),
	}
	if isFreeUsageLimitResponse(rateLimit) {
		t.Fatal("a generic rate limit must not be recognized as the free usage limit")
	}

	ok := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(freeLimitBody)),
	}
	if isFreeUsageLimitResponse(ok) {
		t.Fatal("a 200 must not be recognized as the free usage limit")
	}

	if isFreeUsageLimitResponse(nil) {
		t.Fatal("a nil response must not be recognized as the free usage limit")
	}
}

func TestSessionEscapeMemoChains(t *testing.T) {
	resetSessionEscapes()
	first := identity.RequestIDs{Session: "ses_first"}
	second := first.WithFreshSession()
	third := second.WithFreshSession()
	rememberSessionEscape(first, second)
	rememberSessionEscape(second, third)

	got, ok := escapedSession(first)
	if !ok {
		t.Fatal("expected the first session to have a remembered replacement")
	}
	if got.Session != third.Session {
		t.Fatalf("chain did not reach the last replacement: got %q want %q", got.Session, third.Session)
	}

	unknown := identity.RequestIDs{Session: "ses_unknown"}
	if _, ok := escapedSession(unknown); ok {
		t.Fatal("an unknown session must not report a replacement")
	}
}

func TestSessionEscapeRotatesSpentReplacement(t *testing.T) {
	resetSessionEscapes()
	first := identity.RequestIDs{Session: "ses_first"}
	second := first.WithFreshSession()
	rememberSessionEscape(first, second)
	markSessionEscapeSpent(second)

	// The replacement already served a request, so the chain must advance
	// instead of handing back a bucket that is certain to 429.
	got, ok := escapedSession(first)
	if !ok {
		t.Fatal("expected a remembered replacement")
	}
	third := second.WithFreshSession()
	if got.Session != third.Session {
		t.Fatalf("spent replacement was not rotated: got %q want %q", got.Session, third.Session)
	}

	// The rotation is recorded, so the next request reuses the same link
	// rather than minting another one.
	again, _ := escapedSession(first)
	if again.Session != third.Session {
		t.Fatalf("rotation was not memoized: got %q want %q", again.Session, third.Session)
	}

	// Once that link has served a request, the next call rotates again.
	markSessionEscapeSpent(third)
	fourth := third.WithFreshSession()
	next, _ := escapedSession(first)
	if next.Session != fourth.Session {
		t.Fatalf("second rotation failed: got %q want %q", next.Session, fourth.Session)
	}
}

func TestSessionEscapeAdvancesPastUsedLinks(t *testing.T) {
	resetSessionEscapes()
	first := identity.RequestIDs{Session: "ses_advance"}
	second := first.WithFreshSession()
	rememberSessionEscape(first, second)
	markSessionEscapeSpent(second)

	// Every turn must reach a link that has not served a request yet. With a
	// deterministic mint from the same key, a naive rotation cycles between
	// two spent ids and replays the 429 forever.
	seen := map[string]bool{second.Session: true}
	for turn := 0; turn < 8; turn++ {
		got, ok := escapedSession(first)
		if !ok {
			t.Fatal("expected a remembered replacement")
		}
		if seen[got.Session] {
			t.Fatalf("turn %d reused spent link %q", turn, got.Session)
		}
		seen[got.Session] = true
		markSessionEscapeSpent(got)
	}
}

func TestAdvanceSessionAfterSuccess(t *testing.T) {
	resetSessionEscapes()
	first := identity.RequestIDs{Session: "ses_conv"}

	// The first turn against the client's own id must leave a successor, or
	// the next turn reuses a spent bucket and pays a 429.
	advanceSessionAfterSuccess(first)
	got, ok := escapedSession(first)
	if !ok {
		t.Fatal("a served session must have a successor")
	}
	if got.Session == first.Session {
		t.Fatal("the next turn must not reuse the served session")
	}

	// The successor is fresh, so it is used as-is; after it serves a request
	// the chain must advance again.
	advanceSessionAfterSuccess(got)
	next, _ := escapedSession(first)
	if next.Session == got.Session {
		t.Fatalf("chain did not advance after the successor served a request: %q", next.Session)
	}
}

func TestRememberSessionEscapeKeepsSpentTarget(t *testing.T) {
	resetSessionEscapes()
	first := identity.RequestIDs{Session: "ses_seq"}
	second := first.WithFreshSession()
	markSessionEscapeSpent(second)
	// The escape path remembers the mapping after the retry already
	// succeeded, so remembering must not unmask a target the success path
	// just marked spent.
	rememberSessionEscape(first, second)
	got, _ := escapedSession(first)
	if got.Session == second.Session {
		t.Fatalf("spent target %q was reused after being remembered", second.Session)
	}
}

func TestSessionEscapeMemoCaps(t *testing.T) {
	resetSessionEscapes()
	for i := 0; i < sessionEscapeLimit+8; i++ {
		original := identity.RequestIDs{Session: "ses_" + strconv.Itoa(i)}
		rememberSessionEscape(original, original.WithFreshSession())
	}
	sessionEscapes.Lock()
	defer sessionEscapes.Unlock()
	if len(sessionEscapes.entries) > sessionEscapeLimit {
		t.Fatalf("memo grew past its limit: %d > %d", len(sessionEscapes.entries), sessionEscapeLimit)
	}
}

func TestSessionEscapeMemoFollowsLongChains(t *testing.T) {
	resetSessionEscapes()
	first := identity.RequestIDs{Session: "ses_chain"}
	current := first
	for i := 0; i < 8; i++ {
		next := current.WithFreshSession()
		rememberSessionEscape(current, next)
		current = next
	}
	got, ok := escapedSession(first)
	if !ok {
		t.Fatal("expected a remembered replacement")
	}
	if got.Session != current.Session {
		t.Fatalf("chain stopped early: got %q want %q", got.Session, current.Session)
	}
}

func TestStopBurnedWalk(t *testing.T) {
	now := time.Date(2026, 11, 12, 18, 0, 0, 0, time.UTC)
	freeLimit := func(retryAfter string) *http.Response {
		resp := &http.Response{
			StatusCode: http.StatusTooManyRequests,
			Header:     http.Header{},
			Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"Free usage limit reached"}}`)),
		}
		if retryAfter != "" {
			resp.Header.Set("Retry-After", retryAfter)
		}
		return resp
	}
	derived := identity.RequestIDs{Session: "ses_x", SessionDerived: true}
	if !stopBurnedWalk(context.Background(), derived, freeLimit(""), nil, now) {
		t.Fatal("a derived session's free-limit 429 must stop the original walk")
	}
	if stopBurnedWalk(withSessionRemint(context.Background()), derived, freeLimit(""), nil, now) {
		t.Fatal("the re-minted retry must keep walking exits")
	}
	// A daily reset hint still describes the session's own bucket: every exit
	// shares one egress, so walking cannot help and the re-mint is the only way
	// past it. The original walk must stop too.
	if !stopBurnedWalk(context.Background(), derived, freeLimit("21600"), nil, now) {
		t.Fatal("a daily-reset 429 must stop the original walk")
	}
	// Client-supplied sessions are re-minted as well for the same reason: one
	// egress means the same answer on every exit, and a burned bucket has no
	// prompt cache left to preserve.
	official := identity.RequestIDs{Session: "ses_y"}
	if !stopBurnedWalk(context.Background(), official, freeLimit(""), nil, now) {
		t.Fatal("an official session's free-limit 429 must stop the walk too")
	}
	if stopBurnedWalk(context.Background(), derived, &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("ok"))}, nil, now) {
		t.Fatal("a 200 must not stop the walk")
	}
}

func TestDailyQuotaReset(t *testing.T) {
	now := time.Date(2026, 11, 12, 18, 0, 0, 0, time.UTC)
	nextMidnight := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	resp := func(status int, retryAfter, body string) *http.Response {
		r := &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}
		if retryAfter != "" {
			r.Header.Set("Retry-After", retryAfter)
		}
		return r
	}
	const freeLimitBody = `{"error":{"type":"upstream_error","message":"Free usage limit reached. Free models work in OpenCode"}}`

	reset, ok := dailyQuotaReset(resp(http.StatusTooManyRequests, "21600", freeLimitBody), now)
	if !ok {
		t.Fatal("a free-limit 429 with a midnight Retry-After must be a daily-quota exhaustion")
	}
	if !reset.Equal(nextMidnight) {
		t.Fatalf("reset = %s, want %s", reset, nextMidnight)
	}
	if _, ok := dailyQuotaReset(resp(http.StatusTooManyRequests, "30", freeLimitBody), now); ok {
		t.Fatal("a short Retry-After must not park the exit until midnight")
	}
	if _, ok := dailyQuotaReset(resp(http.StatusTooManyRequests, "", freeLimitBody), now); ok {
		t.Fatal("a free-limit 429 without Retry-After must stay session-scoped")
	}
	if _, ok := dailyQuotaReset(resp(http.StatusTooManyRequests, "21600", `{"error":{"message":"rate limit"}}`), now); ok {
		t.Fatal("a non-free-limit 429 must not be treated as quota")
	}
	// The observed free-tier reset lands a few minutes past midnight, so the
	// window must cover it while still excluding the later session throttles.
	if _, ok := dailyQuotaReset(resp(http.StatusTooManyRequests, "20700", freeLimitBody), now); !ok {
		t.Fatal("a Retry-After five minutes past midnight must be a daily reset")
	}
	if _, ok := dailyQuotaReset(resp(http.StatusTooManyRequests, "19800", freeLimitBody), now); ok {
		t.Fatal("a Retry-After half an hour before midnight must not be a daily reset")
	}
}

func TestExitFreeLimitReset(t *testing.T) {
	now := time.Date(2026, 11, 12, 18, 0, 0, 0, time.UTC)
	resp := func(retry string) *http.Response {
		r := &http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"error":{"message":"Free usage limit reached"}}`))}
		if retry != "" {
			r.Header.Set("Retry-After", retry)
		}
		return r
	}
	proxy := &proxyTransport{index: 0, name: "a"}
	node := &anonymousNode{proxy: proxy}
	// A single session's rejection may be that session's own burn, which the
	// escape retry clears by re-minting, so the exit stays usable.
	if _, ok := exitFreeLimitReset(node, resp("20000"), identity.RequestIDs{Session: "ses_a", SessionDerived: true}, now); ok {
		t.Fatal("a single session's free-limit 429 must not park the exit")
	}
	// A rejection for a different session proves the allowance is the egress's.
	reset, ok := exitFreeLimitReset(node, resp("20000"), identity.RequestIDs{Session: "ses_b", SessionDerived: true}, now.Add(time.Minute))
	if !ok {
		t.Fatal("a second session's free-limit 429 must park the exit")
	}
	if !reset.After(now) {
		t.Fatalf("reset = %s, want after %s", reset, now)
	}
	// The daily hint still parks on the first rejection.
	other := &anonymousNode{proxy: &proxyTransport{index: 1, name: "b"}}
	if _, ok := exitFreeLimitReset(other, resp("20700"), identity.RequestIDs{Session: "ses_c"}, now); !ok {
		t.Fatal("a Retry-After past midnight must park the exit immediately")
	}
}

func TestCursorSkipsDrainedExits(t *testing.T) {
	first := &proxyTransport{index: 0, name: "a"}
	second := &proxyTransport{index: 1, name: "b"}
	first.healthy.Store(true)
	second.healthy.Store(true)
	pool := &anonymousNodes{nodes: []*anonymousNode{{proxy: first}, {proxy: second}}}
	first.markDrained(time.Now().Add(time.Hour))
	cursor := &anonymousCursor{pool: pool}
	if node := cursor.Next(); node == nil || node.proxy != second {
		t.Fatal("a drained exit must be skipped by the anonymous cursor")
	}
	if node := cursor.Next(); node != nil {
		t.Fatalf("the cursor must skip the drained exit every time, got %q", node.proxy.name)
	}
}

func TestCursorProbesFullyDrainedPoolOnce(t *testing.T) {
	first := &proxyTransport{index: 0, name: "a"}
	second := &proxyTransport{index: 1, name: "b"}
	first.healthy.Store(true)
	second.healthy.Store(true)
	pool := &anonymousNodes{nodes: []*anonymousNode{{proxy: first}, {proxy: second}}}
	reset := time.Now().Add(time.Hour)
	first.markDrained(reset)
	second.markDrained(reset)
	cursor := &anonymousCursor{pool: pool}
	// A fully drained pool must still answer with one real upstream attempt so
	// the client receives the quota 429 instead of a gateway error.
	if node := cursor.Next(); node == nil {
		t.Fatal("a fully drained pool must probe one exit instead of returning nil")
	}
	if node := cursor.Next(); node != nil {
		t.Fatalf("the drain probe must happen once per request, got %q", node.proxy.name)
	}
}
