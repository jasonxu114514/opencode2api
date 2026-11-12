package gateway

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"opencode2api/internal/identity"
)

func resetSessionEscapes() {
	sessionEscapes.Lock()
	defer sessionEscapes.Unlock()
	sessionEscapes.entries = map[sessionEscapeKey]sessionEscapeKey{}
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
	freeLimit := func() *http.Response {
		return &http.Response{
			StatusCode: http.StatusTooManyRequests,
			Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"Free usage limit reached"}}`)),
		}
	}
	derived := identity.RequestIDs{Session: "ses_x", SessionDerived: true}
	if !stopBurnedWalk(context.Background(), derived, freeLimit(), nil) {
		t.Fatal("a derived session's free-limit 429 must stop the original walk")
	}
	if stopBurnedWalk(withSessionRemint(context.Background()), derived, freeLimit(), nil) {
		t.Fatal("the re-minted retry must keep walking exits")
	}
	official := identity.RequestIDs{Session: "ses_y"}
	if stopBurnedWalk(context.Background(), official, freeLimit(), nil) {
		t.Fatal("an official session must not stop the walk")
	}
	if stopBurnedWalk(context.Background(), derived, &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("ok"))}, nil) {
		t.Fatal("a 200 must not stop the walk")
	}
}
