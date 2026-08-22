package main

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

type proxyTransport struct {
	index    int
	name     string
	client   *http.Client
	healthy  atomic.Bool
	checking atomic.Bool
}

type transportPool struct {
	items []*proxyTransport
}

// anonymousPool gives the shared OpenCode "public" credential an independent
// cooldown per proxy. Unlike key nodes, anonymous nodes are never rebound:
// changing proxy is the failover mechanism because Zen rate-limits them by IP.
type anonymousPool struct {
	nodes    []*anonymousNode
	next     atomic.Uint64
	cooldown time.Duration
}

type anonymousNode struct {
	proxy         *proxyTransport
	failures      atomic.Uint32
	cooldownUntil atomic.Int64
}

type anonymousCursor struct {
	pool   *anonymousPool
	start  int
	offset int
}

func newAnonymousPool(enabled bool, transports *transportPool, cooldown time.Duration) *anonymousPool {
	pool := &anonymousPool{cooldown: cooldown}
	if !enabled || transports == nil {
		return pool
	}
	pool.nodes = make([]*anonymousNode, 0, len(transports.items))
	for _, proxy := range transports.items {
		pool.nodes = append(pool.nodes, &anonymousNode{proxy: proxy})
	}
	return pool
}

func (p *anonymousPool) Len() int {
	if p == nil {
		return 0
	}
	return len(p.nodes)
}

func (p *anonymousPool) CursorFor(affinity string) anonymousCursor {
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

// Next visits each healthy, non-cooling proxy at most once per cursor.
func (c *anonymousCursor) Next() *anonymousNode {
	if c.pool == nil || len(c.pool.nodes) == 0 {
		return nil
	}
	now := time.Now().UnixNano()
	for c.offset < len(c.pool.nodes) {
		node := c.pool.nodes[(c.start+c.offset)%len(c.pool.nodes)]
		c.offset++
		if node.proxy.healthy.Load() && node.cooldownUntil.Load() <= now {
			return node
		}
	}
	return nil
}

func (p *anonymousPool) MarkSuccess(node *anonymousNode) {
	if node == nil {
		return
	}
	node.failures.Store(0)
	node.cooldownUntil.Store(0)
}

func (p *anonymousPool) MarkFailure(node *anonymousNode, resp *http.Response, err error) {
	if node == nil {
		return
	}
	if err == nil && resp != nil && resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode < 500 {
		return
	}
	failures := node.failures.Add(1)
	delay := p.cooldown * time.Duration(1<<min(failures-1, 3))
	if resp != nil {
		if retryAfter := parseRetryAfter(resp.Header.Get("Retry-After")); retryAfter > delay {
			delay = retryAfter
		}
	}
	node.cooldownUntil.Store(time.Now().Add(delay).UnixNano())
}

func (p *transportPool) hasHealthy() bool {
	for _, proxy := range p.items {
		if proxy.healthy.Load() {
			return true
		}
	}
	return false
}

func (p *transportPool) healthCounts() (total, healthy int) {
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

func newTransportPool(proxies []string, cfg PerformanceConfig, responseHeaderTimeout time.Duration) (*transportPool, error) {
	p := &transportPool{items: make([]*proxyTransport, 0, len(proxies))}
	for _, raw := range proxies {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.MaxIdleConns = cfg.MaxIdleConns
		transport.MaxIdleConnsPerHost = cfg.MaxIdleConnsPerHost
		transport.MaxConnsPerHost = cfg.MaxConnsPerHost
		transport.IdleConnTimeout = time.Duration(cfg.IdleConnTimeoutSeconds) * time.Second
		transport.ResponseHeaderTimeout = responseHeaderTimeout
		transport.ForceAttemptHTTP2 = true
		transport.DialContext = (&net.Dialer{
			Timeout:   time.Duration(cfg.ConnectTimeoutSeconds) * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext
		if raw == "direct" {
			transport.Proxy = nil
		} else {
			u, err := url.Parse(raw)
			if err != nil {
				return nil, fmt.Errorf("parse proxy %s: %w", redactURL(raw), err)
			}
			transport.Proxy = http.ProxyURL(u)
		}
		proxy := &proxyTransport{index: len(p.items), name: raw, client: &http.Client{Transport: transport}}
		proxy.healthy.Store(true)
		p.items = append(p.items, proxy)
	}
	return p, nil
}

type proxyHealthResult struct {
	proxy      *proxyTransport
	err        error
	failed     bool
	wasHealthy bool
}

// CheckHealth concurrently rechecks only proxies already marked unhealthy.
// Healthy proxies are skipped before a check is claimed. Any HTTP response
// from the test URL proves that the route is reachable; only a timeout or
// connection refusal keeps the proxy unhealthy.
func (p *transportPool) CheckHealth(ctx context.Context, target string, timeout time.Duration) []proxyHealthResult {
	results := make(chan proxyHealthResult, len(p.items))
	checks := 0
	for _, proxy := range p.items {
		if proxy.healthy.Load() || !proxy.checking.CompareAndSwap(false, true) {
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

// checkClaimedProxy performs a check after the caller has acquired checking.
func (p *transportPool) checkClaimedProxy(ctx context.Context, proxy *proxyTransport, target string, timeout time.Duration) proxyHealthResult {
	defer proxy.checking.Store(false)
	checkCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(checkCtx, http.MethodGet, target, nil)
	if err == nil {
		req.Header.Set("User-Agent", opencodeUserAgent())
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
// route is unavailable. HTTP responses and unrelated transport/protocol errors
// must not evict a proxy.
func isProxyFailure(err error) bool {
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

type nodePool struct {
	nodes        []*upstreamNode
	transports   *transportPool
	next         atomic.Uint64
	cooldown     time.Duration
	bindingsMu   sync.Mutex
	bindingCount []int
}

// newNodePool distributes keys over proxies in round-robin order. When there
// are fewer keys than proxies, the remaining proxies are intentionally idle and
// can take over a key immediately if an active proxy fails.
func newNodePool(keys []string, transports *transportPool, cooldown time.Duration) (*nodePool, error) {
	if transports == nil || len(transports.items) == 0 {
		return nil, fmt.Errorf("at least one proxy transport is required")
	}
	pool := &nodePool{
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
// back to it. Proxies without an original key simply become healthy failover
// candidates again.
func (p *nodePool) RestoreProxy(recoveredProxy int) int {
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

func (p *nodePool) Len() int { return len(p.nodes) }

func (p *nodePool) NodeByID(id string) *upstreamNode {
	if p == nil || id == "" {
		return nil
	}
	for _, node := range p.nodes {
		if secretFingerprint(node.key) == id {
			return node
		}
	}
	return nil
}

func (p *nodePool) Proxy(node *upstreamNode) *proxyTransport {
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
// currently unhealthy, it still attempts the least-loaded alternative.
func (p *nodePool) RebindProxy(failedProxy int) int {
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

func (p *nodePool) replacementLocked(failedProxy int, healthyOnly bool) int {
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
	pool *nodePool
	next int
}

// Cursor reserves a different starting node for each concurrent request.
// Selection is delayed until Next, so a node marked failed by the preceding
// attempt is immediately skipped. Both Cursor and Next allocate no memory.
func (p *nodePool) Cursor() nodeCursor {
	if len(p.nodes) == 0 {
		return nodeCursor{pool: p}
	}
	return nodeCursor{pool: p, next: int((p.next.Add(1) - 1) % uint64(len(p.nodes)))}
}

// CursorFor returns a cursor whose first choice is stable for the supplied
// affinity key. This keeps every turn in a conversation on the same upstream
// key while retaining Next's cooldown-aware failover behavior. Empty affinity
// keys keep the round-robin behavior used by background tasks.
func (p *nodePool) CursorFor(affinity string) nodeCursor {
	if affinity == "" || len(p.nodes) == 0 {
		return p.Cursor()
	}
	hash := fnv.New64a()
	_, _ = hash.Write([]byte(affinity))
	return nodeCursor{pool: p, next: int(hash.Sum64() % uint64(len(p.nodes)))}
}

func (c *nodeCursor) Next() *upstreamNode {
	if c.pool == nil || len(c.pool.nodes) == 0 {
		return nil
	}
	now := time.Now().UnixNano()
	choice := -1
	var earliest int64
	for offset := 0; offset < len(c.pool.nodes); offset++ {
		i := (c.next + offset) % len(c.pool.nodes)
		until := c.pool.nodes[i].cooldownUntil.Load()
		if until <= now {
			choice = i
			break
		}
		if choice == -1 || until < earliest {
			choice, earliest = i, until
		}
	}
	if choice < 0 {
		return nil
	}
	c.next = (choice + 1) % len(c.pool.nodes)
	return c.pool.nodes[choice]
}

func (p *nodePool) MarkSuccess(node *upstreamNode) {
	node.failures.Store(0)
	node.cooldownUntil.Store(0)
}

func (p *nodePool) MarkFailure(node *upstreamNode, resp *http.Response, err error) {
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

func parseRetryAfter(value string) time.Duration {
	if seconds, err := strconv.Atoi(strings.TrimSpace(value)); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	if when, err := http.ParseTime(value); err == nil {
		return max(time.Until(when), 0)
	}
	return 0
}

func drainAndClose(body io.ReadCloser) {
	if body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(body, 64<<10))
	_ = body.Close()
}
