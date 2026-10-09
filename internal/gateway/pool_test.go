package gateway

import (
	"net/http"
	"strconv"
	"testing"
	"time"
)

// Reproduction for: after free-tier quota exhaustion + network change (new IP),
// the gateway keeps answering 401 until the process is restarted, even though
// the network is back and the new IP has fresh quota.
//
// Root causes covered here:
//  1. MarkFailure honors an upstream Retry-After header with no upper bound,
//     so one quota response can cool a lane for hours.
//  2. The anonymous lane skips cooling nodes outright (no earliest-expiry
//     fallback like the key pool), so a long cooldown reads as a permanent
//     outage and traffic falls through to key lanes that always 401.
//  3. Proxy recovery clears key-node cooldowns but never anonymous ones.

func testTransports() *transportPool {
	proxy := &proxyTransport{name: "direct", client: &http.Client{}}
	proxy.healthy.Store(true)
	return &transportPool{items: []*proxyTransport{proxy}}
}

func retryAfterResponse(seconds int) *http.Response {
	resp := &http.Response{StatusCode: http.StatusTooManyRequests, Header: make(http.Header)}
	resp.Header.Set("Retry-After", strconv.Itoa(seconds))
	return resp
}

func nextAnonymous(t *testing.T, pool *anonymousPool) *anonymousNode {
	t.Helper()
	cursor := pool.CursorFor("")
	node := cursor.Next()
	if node == nil {
		t.Fatal("expected an anonymous node")
	}
	return node
}

func remainingCooldown(t *testing.T, untilUnixNano int64) time.Duration {
	t.Helper()
	remaining := time.Until(time.Unix(0, untilUnixNano))
	if remaining < 0 {
		t.Fatalf("expected a future cooldown, got %v", remaining)
	}
	return remaining
}

func TestAnonymousMarkFailureCapsRetryAfter(t *testing.T) {
	pool := newAnonymousPool(true, testTransports(), 15*time.Second)
	node := nextAnonymous(t, pool)
	pool.MarkFailure(node, retryAfterResponse(3600), nil)
	remaining := remainingCooldown(t, node.cooldownUntil.Load())
	// Base 15s with an 8x ceiling: a 1h Retry-After must be capped at ~120s.
	if remaining > 150*time.Second {
		t.Fatalf("Retry-After was honored without a cap: cooldown=%v, want <=150s", remaining)
	}
	if remaining < 100*time.Second {
		t.Fatalf("Retry-After was ignored instead of capped: cooldown=%v, want ~120s", remaining)
	}
}

func TestKeyMarkFailureCapsRetryAfter(t *testing.T) {
	pool, err := newNodePool([]string{"k1"}, testTransports(), 15*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	cursor := pool.Cursor()
	node := cursor.Next()
	if node == nil {
		t.Fatal("expected a key node")
	}
	pool.MarkFailure(node, retryAfterResponse(7200), nil)
	remaining := remainingCooldown(t, node.cooldownUntil.Load())
	if remaining > 150*time.Second {
		t.Fatalf("Retry-After was honored without a cap: cooldown=%v, want <=150s", remaining)
	}
}

func TestAnonymousRestoreProxyClearsCooldown(t *testing.T) {
	transports := testTransports()
	pool := newAnonymousPool(true, transports, 15*time.Second)
	node := nextAnonymous(t, pool)
	pool.MarkFailure(node, retryAfterResponse(3600), nil)
	cursor := pool.CursorFor("")
	if got := cursor.Next(); got != nil {
		t.Fatal("expected the cooling node to be skipped before restore")
	}
	if cleared := pool.RestoreProxy(transports.items[0]); cleared != 1 {
		t.Fatalf("RestoreProxy cleared %d nodes, want 1", cleared)
	}
	after := pool.CursorFor("")
	if got := after.Next(); got == nil {
		t.Fatal("expected the node to be selectable again after proxy restore")
	}
	if failures := node.failures.Load(); failures != 0 {
		t.Fatalf("failures=%d after restore, want 0", failures)
	}
}
