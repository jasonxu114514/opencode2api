package gateway

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	rand "math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"opencode2api/internal/config"
	"opencode2api/internal/httpx"
)

// proxyTransport is one configured proxy route, or "direct", with its own HTTP
// clients: client for regular traffic and bun for the free-model traffic that
// goes out disguised as the Bun runtime (see Gateway.inferenceClient). pool
// marks a rotating proxy pool entry (#pool or pool+), whose health is never
// tracked. healthy is cleared by proxy-route failures and set again by
// upstream HTTP responses (see syncProxyResult) or a passing health check;
// checking claims the single health check allowed in flight.
type proxyTransport struct {
	index    int
	name     string
	pool     bool
	client   *http.Client
	bun      *http.Client
	healthy  atomic.Bool
	checking atomic.Bool
}

// proxyTransports holds every configured proxy route, indexed by
// proxyTransport.index, shared by the key and anonymous node pools.
type proxyTransports struct {
	items []*proxyTransport
}

// anonymousNodes gives the shared OpenCode "public" credential an independent
// cooldown per proxy. Unlike key nodes, anonymous nodes are never rebound:
// changing proxy is the failover mechanism because Zen rate-limits them by IP.
type anonymousNodes struct {
	nodes    []*anonymousNode
	next     atomic.Uint64
	cooldown time.Duration
	hasPool  bool
}

type anonymousNode struct {
	proxy         *proxyTransport
	failures      atomic.Uint32
	cooldownUntil atomic.Int64
}

// anonymousCursor walks the anonymous nodes for one request from start;
// offset counts the positions visited so far and used marks the nodes the
// first pass or the fallback of Next has already returned.
type anonymousCursor struct {
	pool   *anonymousNodes
	start  int
	offset int
	used   []bool
}

// anonymousPool builds one anonymous node per proxy transport, or an empty
// pool when the anonymous tier is disabled.
func anonymousPool(enabled bool, transports *proxyTransports, cooldown time.Duration) *anonymousNodes {
	pool := &anonymousNodes{cooldown: cooldown}
	if !enabled || transports == nil {
		return pool
	}
	pool.nodes = make([]*anonymousNode, 0, len(transports.items))
	for _, proxy := range transports.items {
		if proxy.pool {
			pool.hasPool = true
		}
		pool.nodes = append(pool.nodes, &anonymousNode{proxy: proxy})
	}
	return pool
}

func (p *anonymousNodes) Len() int {
	if p == nil {
		return 0
	}
	return len(p.nodes)
}

// HasPool reports whether any node uses a rotating proxy pool (#pool), which
// lets callers make more attempts than there are nodes.
func (p *anonymousNodes) HasPool() bool {
	return p != nil && p.hasPool
}

// CursorFor returns a cursor whose first node is stable for a non-empty
// affinity key, keeping a conversation on one exit; an empty affinity rotates
// the starting node round-robin.
func (p *anonymousNodes) CursorFor(affinity string) anonymousCursor {
	if p == nil || len(p.nodes) == 0 {
		return anonymousCursor{pool: p}
	}
	start := 0
	if affinity == "" {
		start = int((p.next.Add(1) - 1) % uint64(len(p.nodes)))
	} else {
		hash := fnv.New64a()
		_, _ = hash.Write([]byte(affinity))
		start = int(hash.Sum64() % uint64(len(p.nodes)))
	}
	return anonymousCursor{pool: p, start: start}
}

// Next returns the next node to try. Its first pass visits every node once
// from the cursor's start and returns each healthy, non-cooling static proxy
// and each rotating proxy-pool transport (#pool) it reaches, so healthy nodes
// are tried first. When the pool contains #pool transports, Next then keeps
// cycling through only those, so retry.max_attempts can draw a fresh upstream
// connection (and new exit IP) on every attempt. Otherwise it falls back to
// the nodes this cursor has not returned yet, healthy ones first and, among
// those, the one whose cooldown expires earliest, and returns nil only once
// every node has been returned.
func (c *anonymousCursor) Next() *anonymousNode {
	if c.pool == nil || len(c.pool.nodes) == 0 {
		return nil
	}
	n := len(c.pool.nodes)
	if c.used == nil {
		c.used = make([]bool, n)
	}
	now := time.Now().UnixNano()
	for c.offset < n {
		idx := (c.start + c.offset) % n
		c.offset++
		node := c.pool.nodes[idx]
		if node.proxy.pool || (!c.used[idx] && node.proxy.healthy.Load() && node.cooldownUntil.Load() <= now) {
			c.used[idx] = true
			return node
		}
	}
	if c.pool.hasPool {
		for range n {
			idx := (c.start + c.offset) % n
			c.offset++
			node := c.pool.nodes[idx]
			if node.proxy.pool {
				return node
			}
		}
	}
	choice := -1
	var earliest int64
	choiceHealthy := false
	for step := range n {
		idx := (c.start + step) % n
		if c.used[idx] {
			continue
		}
		node := c.pool.nodes[idx]
		healthy := node.proxy.healthy.Load()
		until := node.cooldownUntil.Load()
		if choice < 0 || (healthy && !choiceHealthy) || (healthy == choiceHealthy && until < earliest) {
			choice, earliest, choiceHealthy = idx, until, healthy
		}
	}
	if choice >= 0 {
		c.used[choice] = true
		return c.pool.nodes[choice]
	}
	return nil
}

func (p *anonymousNodes) MarkSuccess(node *anonymousNode) {
	if node == nil {
		return
	}
	node.failures.Store(0)
	node.cooldownUntil.Store(0)
}

// RestoreProxy clears cooldowns for anonymous nodes bound to a recovered
// proxy, mirroring upstreamNodes.RestoreProxy for key nodes. Without this, an
// outage-era cooldown survives the proxy's recovery and the anonymous lane
// stays skipped even though the route is healthy again. It returns how many
// nodes it cleared.
func (p *anonymousNodes) RestoreProxy(proxy *proxyTransport) int {
	if p == nil || proxy == nil {
		return 0
	}
	cleared := 0
	for _, node := range p.nodes {
		if node.proxy != proxy {
			continue
		}
		if node.cooldownUntil.Load() == 0 && node.failures.Load() == 0 {
			continue
		}
		node.failures.Store(0)
		node.cooldownUntil.Store(0)
		cleared++
	}
	return cleared
}

// maxAnonymousRetryAfterMultiplier bounds an upstream Retry-After on the
// anonymous lane to the same 8x ceiling as its exponential backoff. The lane
// is rate limited per exit IP, and a cooling node is reached only through the
// cursor's fallback: after every eligible node, and not at all while #pool
// transports are present, since the cursor then keeps cycling those. A quota
// response asking for hours would keep the exit demoted long after a network
// change gave it a fresh allowance. Key lanes keep honoring Retry-After in
// full.
const maxAnonymousRetryAfterMultiplier = 8

// MarkFailure cools node down after an error or a 401, 403, 429 or 5xx
// response; any other response leaves it untouched, and #pool nodes are never
// cooled. The cooldown doubles with each consecutive failure, from the
// base cooldown up to 8x, and a longer Retry-After extends it up to that same
// 8x ceiling.
func (p *anonymousNodes) MarkFailure(node *anonymousNode, resp *http.Response, err error) {
	if node == nil || (node.proxy != nil && node.proxy.pool) {
		return
	}
	if err == nil && resp != nil && resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode < 500 {
		return
	}
	failures := node.failures.Add(1)
	delay := p.cooldown * time.Duration(1<<min(failures-1, 3))
	if resp != nil {
		if retryAfter := parseRetryAfter(resp.Header.Get("Retry-After")); retryAfter > delay {
			delay = min(retryAfter, p.cooldown*maxAnonymousRetryAfterMultiplier)
		}
	}
	node.cooldownUntil.Store(time.Now().Add(delay).UnixNano())
}

func (p *proxyTransports) hasHealthy() bool {
	for _, proxy := range p.items {
		if proxy.healthy.Load() {
			return true
		}
	}
	return false
}

func (p *proxyTransports) healthCounts() (total, healthy int) {
	if p == nil {
		return 0, 0
	}
	for _, proxy := range p.items {
		if proxy.healthy.Load() {
			healthy++
		}
	}
	return len(p.items), healthy
}

// transportPool builds the HTTP clients of every configured proxy entry, each
// entry starting healthy: a regular client and a Bun-disguised one
// (httpx.BunTransport) sharing the proxy, connect timeout, idle settings and
// responseHeaderTimeout as their header wait. A #pool entry gets HTTP/1.1
// transports without keep-alives, so every attempt opens a new connection
// through the pool; "direct" dials the upstream without any proxy, ignoring
// the proxy environment variables.
func transportPool(proxies []string, cfg config.PerformanceConfig, responseHeaderTimeout time.Duration) (*proxyTransports, error) {
	p := &proxyTransports{items: make([]*proxyTransport, 0, len(proxies))}
	for _, raw := range proxies {
		clean, isPool := config.ParseProxyEntry(raw)
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.MaxIdleConns = cfg.MaxIdleConns
		transport.MaxIdleConnsPerHost = cfg.MaxIdleConnsPerHost
		transport.MaxConnsPerHost = cfg.MaxConnsPerHost
		transport.IdleConnTimeout = time.Duration(cfg.IdleConnTimeoutSeconds) * time.Second
		transport.ResponseHeaderTimeout = responseHeaderTimeout
		transport.ForceAttemptHTTP2 = !isPool
		transport.DisableKeepAlives = isPool
		if isPool {
			// Cloning DefaultTransport after its first dial inherits the
			// already-configured HTTP/2 ALPN and TLSNextProto map, which ignores
			// DisableKeepAlives and req.Close and multiplexes every request over
			// one CONNECT tunnel. An explicit empty TLSNextProto map and HTTP/1.1
			// ALPN force a brand-new TCP + CONNECT tunnel per attempt.
			transport.TLSNextProto = make(map[string]func(authority string, c *tls.Conn) http.RoundTripper)
			if transport.TLSClientConfig == nil {
				transport.TLSClientConfig = &tls.Config{NextProtos: []string{"http/1.1"}}
			} else {
				tlsCfg := transport.TLSClientConfig.Clone()
				tlsCfg.NextProtos = []string{"http/1.1"}
				transport.TLSClientConfig = tlsCfg
			}
		}
		transport.DialContext = (&net.Dialer{
			Timeout:   time.Duration(cfg.ConnectTimeoutSeconds) * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext
		var proxyURL *url.URL
		if clean == "direct" {
			transport.Proxy = nil
		} else {
			u, err := url.Parse(clean)
			if err != nil {
				return nil, fmt.Errorf("parse proxy %s: %w", config.RedactURL(raw), err)
			}
			transport.Proxy = http.ProxyURL(u)
			proxyURL = u
		}
		bun, err := httpx.NewBunTransport(httpx.BunOptions{
			Proxy:                 proxyURL,
			ConnectTimeout:        time.Duration(cfg.ConnectTimeoutSeconds) * time.Second,
			ResponseHeaderTimeout: responseHeaderTimeout,
			IdleConnTimeout:       transport.IdleConnTimeout,
			MaxIdleConnsPerHost:   cfg.MaxIdleConnsPerHost,
			DisableKeepAlives:     isPool,
		})
		if err != nil {
			return nil, fmt.Errorf("proxy %s: %w", config.RedactURL(raw), err)
		}
		proxy := &proxyTransport{index: len(p.items), name: raw, pool: isPool, client: &http.Client{Transport: transport}, bun: &http.Client{Transport: bun}}
		proxy.healthy.Store(true)
		p.items = append(p.items, proxy)
	}
	return p, nil
}

// proxyHealthResult is the outcome of one health check. failed is set only
// for a proxy-route failure (see isProxyFailure), and wasHealthy is the
// healthy flag as it stood before the check updated it.
type proxyHealthResult struct {
	proxy      *proxyTransport
	err        error
	failed     bool
	wasHealthy bool
}

// CheckHealth concurrently rechecks only proxies already marked unhealthy and
// waits for every check to finish. Healthy proxies, #pool transports and
// proxies whose check is already in flight are skipped. Any HTTP response
// from the test URL proves that the route is reachable and restores the
// proxy; any error keeps it unhealthy.
func (p *proxyTransports) CheckHealth(ctx context.Context, target string, timeout time.Duration) []proxyHealthResult {
	results := make(chan proxyHealthResult, len(p.items))
	checks := 0
	for _, proxy := range p.items {
		if proxy.pool || proxy.healthy.Load() || !proxy.checking.CompareAndSwap(false, true) {
			continue
		}
		// A real request may have restored the proxy between the first health
		// read and claiming this check.
		if proxy.healthy.Load() {
			proxy.checking.Store(false)
			continue
		}
		checks++
		go func() {
			results <- p.checkClaimedProxy(ctx, proxy, target, timeout)
		}()
	}
	out := make([]proxyHealthResult, 0, checks)
	for range checks {
		out = append(out, <-results)
	}
	return out
}

// checkClaimedProxy sends one GET to target through proxy after the caller
// has acquired checking, and releases that claim when done. Any HTTP response
// marks the proxy healthy, a proxy-route failure marks it unhealthy, and any
// other error leaves the flag as it is.
func (p *proxyTransports) checkClaimedProxy(ctx context.Context, proxy *proxyTransport, target string, timeout time.Duration) proxyHealthResult {
	defer proxy.checking.Store(false)
	checkCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(checkCtx, http.MethodGet, target, nil)
	if err == nil {
		req.Header.Set("User-Agent", httpx.V2UserAgent())
		resp, requestErr := proxy.client.Do(req)
		err = requestErr
		if resp != nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
			_ = resp.Body.Close()
		}
	}
	result := proxyHealthResult{proxy: proxy, err: err, wasHealthy: proxy.healthy.Load()}
	if err == nil {
		result.wasHealthy = proxy.healthy.Swap(true)
	} else if isProxyFailure(err) {
		result.failed = true
		result.wasHealthy = proxy.healthy.Swap(false)
	}
	return result
}

// isProxyFailure deliberately recognizes only failures that say the proxy
// route is unavailable: a timeout or a connection refusal. HTTP responses,
// timeouts in the first-event wait (the upstream already answered with
// headers) and unrelated transport/protocol errors must not evict a proxy.
func isProxyFailure(err error) bool {
	if errors.Is(err, errFirstEventTimeout) || upstreamAccepted(err) {
		return false // The upstream accepted HTTP; slow generation is not a broken proxy.
	}
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, syscall.ECONNREFUSED) {
		return true
	}
	var timeout interface{ Timeout() bool }
	return errors.As(err, &timeout) && timeout.Timeout()
}

// upstreamNode keeps a key stable while allowing its proxy binding to change
// atomically when a proxy becomes unavailable.
type upstreamNode struct {
	key            string
	index          int
	preferredProxy int
	proxyIndex     atomic.Int64
	failures       atomic.Uint32
	cooldownUntil  atomic.Int64
}

// upstreamNodes is one tier's key pool. bindingsMu serializes rebinding and
// guards bindingCount, the number of keys bound to each proxy index; a node's
// proxyIndex is atomic, so reading a binding needs no lock.
type upstreamNodes struct {
	nodes        []*upstreamNode
	transports   *proxyTransports
	next         atomic.Uint64
	cooldown     time.Duration
	bindingsMu   sync.Mutex
	bindingCount []int
}

// nodePool distributes keys over proxies in round-robin order. When there
// are fewer keys than proxies, the remaining proxies are intentionally idle and
// can take over a key immediately if an active proxy fails. It needs at least
// one proxy transport.
func nodePool(keys []string, transports *proxyTransports, cooldown time.Duration) (*upstreamNodes, error) {
	if transports == nil || len(transports.items) == 0 {
		return nil, fmt.Errorf("at least one proxy transport is required")
	}
	pool := &upstreamNodes{
		nodes:        make([]*upstreamNode, 0, len(keys)),
		transports:   transports,
		cooldown:     cooldown,
		bindingCount: make([]int, len(transports.items)),
	}
	if len(keys) == 0 {
		return pool, nil
	}
	for i, key := range keys {
		proxyIndex := i % len(transports.items)
		node := &upstreamNode{key: key, index: i, preferredProxy: proxyIndex}
		node.proxyIndex.Store(int64(proxyIndex))
		pool.nodes = append(pool.nodes, node)
		pool.bindingCount[proxyIndex]++
	}
	return pool, nil
}

// RestoreProxy moves keys that were originally assigned to a recovered proxy
// back to it, clearing their failure counts and cooldowns, and returns how
// many keys moved. Proxies without an original key simply become healthy
// failover candidates again.
func (p *upstreamNodes) RestoreProxy(recoveredProxy int) int {
	if p == nil || p.transports == nil || recoveredProxy < 0 || recoveredProxy >= len(p.transports.items) {
		return 0
	}
	p.bindingsMu.Lock()
	defer p.bindingsMu.Unlock()

	moved := 0
	for _, node := range p.nodes {
		current := int(node.proxyIndex.Load())
		if node.preferredProxy != recoveredProxy || current == recoveredProxy {
			continue
		}
		if current >= 0 && current < len(p.bindingCount) {
			p.bindingCount[current]--
		}
		p.bindingCount[recoveredProxy]++
		node.proxyIndex.Store(int64(recoveredProxy))
		node.failures.Store(0)
		node.cooldownUntil.Store(0)
		moved++
	}
	return moved
}

func (p *upstreamNodes) Len() int { return len(p.nodes) }

// NodeByID resolves a node by its stable fingerprint. Callers only ever hold
// the fingerprint, never the key value, so the lookup cannot leak a secret.
func (p *upstreamNodes) NodeByID(id string) *upstreamNode {
	if p == nil || id == "" {
		return nil
	}
	for _, node := range p.nodes {
		if config.Fingerprint(node.key) == id {
			return node
		}
	}
	return nil
}

func (p *upstreamNodes) Proxy(node *upstreamNode) *proxyTransport {
	if p == nil || node == nil || p.transports == nil {
		return nil
	}
	index := int(node.proxyIndex.Load())
	if index < 0 || index >= len(p.transports.items) {
		return nil
	}
	return p.transports.items[index]
}

// RebindProxy moves every key currently using failedProxy to the least-loaded
// healthy proxy. Empty proxies are selected in configuration order; otherwise
// one of the least-loaded proxies is chosen at random. If every alternative is
// currently unhealthy, it still moves the key to the least-loaded alternative.
// Moved keys start with a clean failure count and cooldown. It returns how
// many keys moved; with a single proxy there is nowhere to move them.
func (p *upstreamNodes) RebindProxy(failedProxy int) int {
	if p == nil || p.transports == nil || len(p.transports.items) < 2 || failedProxy < 0 || failedProxy >= len(p.transports.items) {
		return 0
	}
	p.bindingsMu.Lock()
	defer p.bindingsMu.Unlock()

	moved := 0
	for _, node := range p.nodes {
		if int(node.proxyIndex.Load()) != failedProxy {
			continue
		}
		target := p.replacementLocked(failedProxy, true)
		if target < 0 {
			target = p.replacementLocked(failedProxy, false)
		}
		if target < 0 {
			continue
		}
		p.bindingCount[failedProxy]--
		p.bindingCount[target]++
		node.proxyIndex.Store(int64(target))
		node.failures.Store(0)
		node.cooldownUntil.Store(0)
		moved++
	}
	return moved
}

// replacementLocked picks the proxy a key leaving failedProxy moves to: the
// first proxy in configuration order with no keys bound, otherwise a random
// one of the least-loaded, considering only healthy proxies when healthyOnly
// is set. It returns -1 when no proxy qualifies. The caller must hold
// bindingsMu.
func (p *upstreamNodes) replacementLocked(failedProxy int, healthyOnly bool) int {
	minimum := int(^uint(0) >> 1)
	candidates := make([]int, 0, len(p.transports.items)-1)
	for i := range p.transports.items {
		if i == failedProxy || healthyOnly && !p.transports.items[i].healthy.Load() {
			continue
		}
		count := p.bindingCount[i]
		if count == 0 {
			return i
		}
		if count < minimum {
			minimum = count
			candidates = candidates[:0]
			candidates = append(candidates, i)
		} else if count == minimum {
			candidates = append(candidates, i)
		}
	}
	if len(candidates) == 0 {
		return -1
	}
	return candidates[rand.IntN(len(candidates))]
}

type nodeCursor struct {
	pool *upstreamNodes
	next int
}

// Cursor reserves a different starting node for each concurrent request.
// Selection is delayed until Next, so a node marked failed by the preceding
// attempt is immediately skipped. Both Cursor and Next allocate no memory.
func (p *upstreamNodes) Cursor() nodeCursor {
	if len(p.nodes) == 0 {
		return nodeCursor{pool: p}
	}
	return nodeCursor{pool: p, next: int((p.next.Add(1) - 1) % uint64(len(p.nodes)))}
}

// CursorFor returns a cursor whose first choice is stable for the supplied
// affinity key. This keeps every turn in a conversation on the same upstream
// key while retaining Next's cooldown-aware failover behavior. Empty affinity
// keys keep the round-robin behavior used by background tasks.
func (p *upstreamNodes) CursorFor(affinity string) nodeCursor {
	if affinity == "" || len(p.nodes) == 0 {
		return p.Cursor()
	}
	hash := fnv.New64a()
	_, _ = hash.Write([]byte(affinity))
	return nodeCursor{pool: p, next: int(hash.Sum64() % uint64(len(p.nodes)))}
}

// Next chooses an uncooled key on a healthy proxy first, then the earliest
// cooling key on any healthy proxy. Only when no healthy proxy has a key does
// it fall back to an uncooled key on an unhealthy proxy, then the earliest
// cooldown overall. It moves the cursor past the selected node. Unlike
// anonymousCursor.Next it never returns nil for a non-empty pool and may revisit
// a node, so callers bound attempts themselves.
func (c *nodeCursor) Next() *upstreamNode {
	if c.pool == nil || len(c.pool.nodes) == 0 {
		return nil
	}
	now := time.Now().UnixNano()
	ready, healthyReady := -1, -1
	var earliest, healthyEarliest int64
	choice, healthyChoice := -1, -1
	for offset := range len(c.pool.nodes) {
		i := (c.next + offset) % len(c.pool.nodes)
		node := c.pool.nodes[i]
		proxy := c.pool.Proxy(node)
		healthy := proxy != nil && proxy.healthy.Load()
		until := node.cooldownUntil.Load()
		if until <= now {
			if healthy {
				healthyReady = i
				break
			}
			if ready < 0 {
				ready = i
			}
			continue
		}
		if healthy && (healthyChoice < 0 || until < healthyEarliest) {
			healthyChoice, healthyEarliest = i, until
		}
		if choice < 0 || until < earliest {
			choice, earliest = i, until
		}
	}
	if healthyReady >= 0 {
		choice = healthyReady
	} else if healthyChoice >= 0 {
		choice = healthyChoice
	} else if ready >= 0 {
		choice = ready
	} else if choice < 0 {
		return nil
	}
	c.next = (choice + 1) % len(c.pool.nodes)
	return c.pool.nodes[choice]
}

func (p *upstreamNodes) MarkSuccess(node *upstreamNode) {
	node.failures.Store(0)
	node.cooldownUntil.Store(0)
}

// MarkFailure cools node down after an error or a 401, 403, 429 or 5xx
// response; any other response leaves it untouched. The cooldown doubles with
// each consecutive failure, from the base cooldown up to 8x, and an upstream
// Retry-After longer than that is honored in full.
func (p *upstreamNodes) MarkFailure(node *upstreamNode, resp *http.Response, err error) {
	if err == nil && resp != nil && resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode < 500 {
		return
	}
	failures := node.failures.Add(1)
	multiplier := time.Duration(1 << min(failures-1, 3))
	delay := p.cooldown * multiplier
	if resp != nil {
		if retryAfter := parseRetryAfter(resp.Header.Get("Retry-After")); retryAfter > delay {
			delay = retryAfter
		}
	}
	node.cooldownUntil.Store(time.Now().Add(delay).UnixNano())
}

// parseRetryAfter reads a Retry-After header in delta-seconds or HTTP-date
// form. It returns 0 for an empty, non-positive or unparsable value and for a
// date already in the past.
func parseRetryAfter(value string) time.Duration {
	if seconds, err := strconv.Atoi(strings.TrimSpace(value)); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	if when, err := http.ParseTime(value); err == nil {
		return max(time.Until(when), 0)
	}
	return 0
}
