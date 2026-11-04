package gateway

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"opencode2api/internal/config"
)

func TestNodePool_PerKeyBinding(t *testing.T) {
	transports := &proxyTransports{
		items: []*proxyTransport{
			{index: 0, name: "http://127.0.0.1:18090"},
			{index: 1, name: "http://127.0.0.1:18091"},
			{index: 2, name: "http://127.0.0.1:18092"},
		},
	}
	for _, tr := range transports.items {
		tr.healthy.Store(true)
	}

	keys := []string{"key-alpha-12345", "key-beta-67890", "key-gamma-99999"}
	alphaFP := config.Fingerprint(keys[0])

	// Explicitly bind alpha to proxy 1 (index 1) and gamma to proxy 0 (index 0).
	// beta is left unbound.
	bindings := map[string]string{
		alphaFP: "http://127.0.0.1:18091",
		keys[2]: "http://127.0.0.1:18090", // raw key lookup works too
	}

	pool, err := nodePool(keys, transports, 15*time.Second, bindings)
	if err != nil {
		t.Fatalf("nodePool failed: %v", err)
	}

	nodeAlpha := pool.nodes[0]
	nodeBeta := pool.nodes[1]
	nodeGamma := pool.nodes[2]

	if !nodeAlpha.bound || nodeAlpha.proxyIndex.Load() != 1 {
		t.Errorf("nodeAlpha: want bound=true proxyIndex=1, got bound=%v proxyIndex=%d", nodeAlpha.bound, nodeAlpha.proxyIndex.Load())
	}
	if nodeBeta.bound || nodeBeta.proxyIndex.Load() != 1 { // 1 % 3 = 1
		t.Errorf("nodeBeta: want bound=false proxyIndex=1, got bound=%v proxyIndex=%d", nodeBeta.bound, nodeBeta.proxyIndex.Load())
	}
	if !nodeGamma.bound || nodeGamma.proxyIndex.Load() != 0 {
		t.Errorf("nodeGamma: want bound=true proxyIndex=0, got bound=%v proxyIndex=%d", nodeGamma.bound, nodeGamma.proxyIndex.Load())
	}

	// Now fail proxy 1.
	// nodeAlpha is bound to proxy 1 -> MUST NOT MOVE (fail-closed).
	// nodeBeta is unbound on proxy 1 -> MUST MOVE to another healthy proxy (e.g. index 0 or 2).
	moved := pool.RebindProxy(1)
	if moved != 1 {
		t.Errorf("RebindProxy(1): want moved=1 (only unbound node), got %d", moved)
	}

	if nodeAlpha.proxyIndex.Load() != 1 {
		t.Errorf("nodeAlpha moved during proxy failure: got %d, want 1 (fail-closed)", nodeAlpha.proxyIndex.Load())
	}
	if nodeBeta.proxyIndex.Load() == 1 {
		t.Errorf("nodeBeta was not moved away from failed proxy 1")
	}

	// Simulate nodeAlpha failure & cooldown while proxy 1 is down.
	nodeAlpha.failures.Store(3)
	nodeAlpha.cooldownUntil.Store(time.Now().Add(10 * time.Minute).UnixNano())

	// Now restore proxy 1.
	// nodeAlpha should have failures and cooldown reset, remaining on proxy 1.
	// nodeBeta should move back to proxy 1.
	restoredMoved := pool.RestoreProxy(1)
	if restoredMoved != 1 {
		t.Errorf("RestoreProxy(1): want moved=1 (unbound node restored), got %d", restoredMoved)
	}

	if nodeAlpha.proxyIndex.Load() != 1 {
		t.Errorf("nodeAlpha: got proxyIndex=%d, want 1", nodeAlpha.proxyIndex.Load())
	}
	if nodeAlpha.failures.Load() != 0 {
		t.Errorf("nodeAlpha: failures not cleared upon proxy recovery: got %d", nodeAlpha.failures.Load())
	}
	if nodeAlpha.cooldownUntil.Load() != 0 {
		t.Errorf("nodeAlpha: cooldown not cleared upon proxy recovery")
	}
	if nodeBeta.proxyIndex.Load() != 1 {
		t.Errorf("nodeBeta: failed to return to preferred proxy 1")
	}
}

func TestNodePool_ReorderingKeysPreservesBindings(t *testing.T) {
	transports := &proxyTransports{
		items: []*proxyTransport{
			{index: 0, name: "http://127.0.0.1:18090"},
			{index: 1, name: "http://127.0.0.1:18091"},
		},
	}
	for _, tr := range transports.items {
		tr.healthy.Store(true)
	}

	key1 := "zen-key-number-one"
	key2 := "zen-key-number-two"
	bindings := map[string]string{
		config.Fingerprint(key1): "http://127.0.0.1:18091",
	}

	// Order A: [key1, key2]
	poolA, err := nodePool([]string{key1, key2}, transports, 10*time.Second, bindings)
	if err != nil {
		t.Fatalf("poolA creation failed: %v", err)
	}

	// Order B: [key2, key1] (reordered!)
	poolB, err := nodePool([]string{key2, key1}, transports, 10*time.Second, bindings)
	if err != nil {
		t.Fatalf("poolB creation failed: %v", err)
	}

	nodeAKey1 := poolA.NodeByID(config.Fingerprint(key1))
	nodeBKey1 := poolB.NodeByID(config.Fingerprint(key1))

	if nodeAKey1.proxyIndex.Load() != 1 || nodeBKey1.proxyIndex.Load() != 1 {
		t.Errorf("key1 binding differed across key reordering: A=%d B=%d", nodeAKey1.proxyIndex.Load(), nodeBKey1.proxyIndex.Load())
	}
}

func TestKeyStatuses_ReportsBoundFieldWithoutSecrets(t *testing.T) {
	transports := &proxyTransports{
		items: []*proxyTransport{
			{index: 0, name: "http://127.0.0.1:18090"},
		},
	}
	transports.items[0].healthy.Store(true)

	secretKey := "super-secret-zen-key-xyz99"
	bindings := map[string]string{
		config.Fingerprint(secretKey): "http://127.0.0.1:18090",
	}

	pool, err := nodePool([]string{secretKey}, transports, 10*time.Second, bindings)
	if err != nil {
		t.Fatalf("nodePool failed: %v", err)
	}

	statuses := keyStatuses("zen", pool)
	if len(statuses) != 1 {
		t.Fatalf("expected 1 status, got %d", len(statuses))
	}
	st := statuses[0]
	if !st.Bound {
		t.Errorf("expected Bound=true")
	}
	if st.ID != "xyz99" {
		t.Errorf("expected masked ID 'xyz99', got %q", st.ID)
	}
	// Raw key must not appear anywhere in ID
	if st.ID == secretKey {
		t.Errorf("raw key leaked into status ID")
	}
}

func TestGateway_BoundKeyFailsClosed(t *testing.T) {
	// Setup 2 mock upstreams / proxies
	var proxy1Calls, proxy2Calls atomic.Int64

	server1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxy1Calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"response from proxy 1"}}]}`))
	}))
	defer server1.Close()

	server2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxy2Calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"response from proxy 2"}}]}`))
	}))
	defer server2.Close()

	cfg := config.Config{
		Listen:     "127.0.0.1:8080",
		ServerKeys: []string{"sk-local-test"},
		ZenKeys:    []string{"zen-key-alpha", "zen-key-beta"},
		Proxies:    []string{server1.URL, server2.URL},
		ProxyBindings: map[string]string{
			config.Fingerprint("zen-key-alpha"): server1.URL,
			config.Fingerprint("zen-key-beta"):  server2.URL,
		},
		Upstream: config.UpstreamConfig{
			Zen:       server1.URL,
			Go:        server1.URL,
			Anonymous: server1.URL,
		},
		Retry: config.RetryConfig{
			MaxAttempts:    2,
			TimeoutSeconds: 5,
		},
		Models: config.ModelsConfig{
			RefreshSeconds: 300,
		},
		Performance: config.PerformanceConfig{
			MaxIdleConns:           10,
			MaxIdleConnsPerHost:    5,
			IdleConnTimeoutSeconds: 30,
			ConnectTimeoutSeconds:  5,
			FailureCooldownSeconds: 15,
		},
		Logging: config.LoggingConfig{
			Level:    "error",
			RingSize: 1000,
		},
		Prefer: config.TierZen,
	}

	norm, err := config.Normalize("", cfg)
	if err != nil {
		t.Fatalf("Normalize failed: %v", err)
	}

	gw, err := New(norm, nil, nil, nil)
	if err != nil {
		t.Fatalf("New gateway failed: %v", err)
	}

	nodeAlpha := gw.zenNodes.NodeByID(config.Fingerprint("zen-key-alpha"))
	if nodeAlpha == nil {
		t.Fatalf("nodeAlpha not found")
	}

	// Verify that nodeAlpha routes through server1.URL
	p := gw.zenNodes.Proxy(nodeAlpha)
	if p == nil || p.name != server1.URL {
		t.Fatalf("nodeAlpha proxy want %s, got %v", server1.URL, p)
	}

	// Simulate failure of server1 proxy
	gw.transports.items[0].healthy.Store(false)
	gw.rebindUnavailableProxy(gw.transports.items[0], true)

	// Verify nodeAlpha was NOT rebound to server2
	if gw.zenNodes.Proxy(nodeAlpha).name != server1.URL {
		t.Errorf("fail-closed violation: nodeAlpha was moved to %s", gw.zenNodes.Proxy(nodeAlpha).name)
	}

	// Restore server1 proxy
	gw.transports.items[0].healthy.Store(true)
	gw.restoreProxy(gw.transports.items[0])

	if gw.zenNodes.Proxy(nodeAlpha).name != server1.URL {
		t.Errorf("nodeAlpha did not stay on server1 after restore")
	}
}

func TestGateway_AnonymousUnaffectedByBindings(t *testing.T) {
	transports := &proxyTransports{
		items: []*proxyTransport{
			{index: 0, name: "http://127.0.0.1:18090"},
			{index: 1, name: "http://127.0.0.1:18091"},
		},
	}
	for _, tr := range transports.items {
		tr.healthy.Store(true)
	}

	anon := anonymousPool(true, transports, 10*time.Second, anonymousBackoff{})
	if anon.Len() != 2 {
		t.Fatalf("expected 2 anonymous nodes, got %d", anon.Len())
	}
	// Anonymous cursor visits all proxies round-robin regardless of key bindings
	cursor := anon.CursorFor("")
	n1 := cursor.Next()
	n2 := cursor.Next()
	if n1 == nil || n2 == nil {
		t.Fatalf("anonymous cursor returned nil")
	}
	if n1.proxy.name == n2.proxy.name {
		t.Errorf("anonymous cursor did not cycle proxies: got same proxy %s", n1.proxy.name)
	}
}
