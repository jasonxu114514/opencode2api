package warp

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun/netstack"
)

const (
	probeTimeout    = 5 * time.Second
	probeCandidates = 10
)

type Stack string

const (
	StackIPv4 Stack = "ipv4"
	StackIPv6 Stack = "ipv6"
	StackDual Stack = "dual"

	// DefaultStack is IPv6: a WARP IPv4 egress is a public address shared by
	// every user behind Cloudflare's NAT, so the upstream's per-address free
	// quota is usually already spent, while the IPv6 egress belongs to the
	// tunnel. The handshake still falls back to IPv4 endpoints when IPv6 does
	// not answer, so an IPv4-only host keeps working without extra settings.
	DefaultStack = StackIPv6
)

var (
	dnsV4 = netip.MustParseAddr("1.1.1.1")
	dnsV6 = netip.MustParseAddr("2606:4700:4700::1111")
)

func ParseStack(value string) (Stack, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "":
		return DefaultStack, nil
	case "ipv6", "v6":
		return StackIPv6, nil
	case "ipv4", "v4":
		return StackIPv4, nil
	case "dual", "both":
		return StackDual, nil
	default:
		return "", fmt.Errorf("unsupported WARP stack %q; use ipv6, ipv4 or dual", value)
	}
}

type exit struct {
	id        string
	egress    netip.Addr
	stack     Stack
	device    *device.Device
	network   *netstack.Net
	mu        sync.Mutex
	refs      int
	retired   bool
	closed    bool
	penalized bool
	closeOnce sync.Once
}

// newExitWithStack builds one userspace tunnel for reg. A valid source pins
// the outer IPv6 socket and tries IPv6 endpoints first. IPv4 endpoints remain
// available as a fallback when the host cannot reach Cloudflare over IPv6.
func newExitWithStack(ctx context.Context, reg registration, stack Stack, source netip.Addr) (*exit, error) {
	normalized, err := ParseStack(string(stack))
	if err != nil {
		return nil, err
	}
	stack = normalized
	localV6, v6Err := parseAddress(reg.AddressV6)
	localV4, v4Err := parseAddress(reg.AddressV4)
	var localAddresses []netip.Addr
	var dns []netip.Addr
	switch stack {
	case StackIPv6:
		if v6Err != nil || !localV6.Is6() {
			return nil, fmt.Errorf("invalid WARP IPv6 address %q", reg.AddressV6)
		}
		localAddresses = []netip.Addr{localV6}
		dns = []netip.Addr{dnsV6}
	case StackIPv4:
		if v4Err != nil || !localV4.Is4() {
			return nil, fmt.Errorf("invalid WARP IPv4 address %q", reg.AddressV4)
		}
		localAddresses = []netip.Addr{localV4}
		dns = []netip.Addr{dnsV4}
	case StackDual:
		if v6Err == nil && localV6.Is6() {
			localAddresses = append(localAddresses, localV6)
		}
		if v4Err == nil && localV4.Is4() {
			localAddresses = append(localAddresses, localV4)
		}
		if len(localAddresses) == 0 {
			return nil, fmt.Errorf("WARP registration has no usable address: v4=%q v6=%q", reg.AddressV4, reg.AddressV6)
		}
		dns = []netip.Addr{dnsV4, dnsV6}
	}
	tunDevice, network, err := netstack.CreateNetTUN(localAddresses, dns, 1280)
	if err != nil {
		return nil, fmt.Errorf("create WARP network stack: %w", err)
	}
	privateKey, err := uapiKey(reg.PrivateKey)
	if err != nil {
		tunDevice.Close()
		return nil, fmt.Errorf("decode WARP private key: %w", err)
	}
	peerKey, err := uapiKey(reg.PeerPublicKey)
	if err != nil {
		tunDevice.Close()
		return nil, fmt.Errorf("decode WARP peer key: %w", err)
	}
	endpoints, err := resolveEndpoints(ctx, reg.Endpoints, reg.Endpoint)
	if err != nil {
		tunDevice.Close()
		return nil, err
	}
	// Try IPv6 endpoints first when the exit leaves from a rotating IPv6
	// source or the host has its own IPv6 connectivity. The IPv4 ones stay
	// behind them, so an IPv4-only host still gets a working tunnel; an
	// IPv6-capable host avoids the udp4 attempts that otherwise cost a probe
	// timeout each before the tunnel comes up.
	if source.IsValid() || hostHasIPv6() {
		endpoints = preferIPv6Endpoints(endpoints)
	}
	// Start from the peer that answered for a previous exit: every exit talks to
	// the same anycast peers, so re-walking dead candidates costs a probe
	// timeout each, on a path that runs for every registration and replacement.
	endpoints = orderEndpointsByMemory(endpoints)
	wg := device.NewDevice(tunDevice, &simpleBind{source: source}, device.NewLogger(device.LogLevelError, "warp: "))
	uapi := fmt.Sprintf(
		"private_key=%s\npublic_key=%s\nallowed_ip=0.0.0.0/0\nallowed_ip=::/0\nendpoint=%s\npersistent_keepalive_interval=25\n",
		privateKey, peerKey, endpoints[0],
	)
	if err := wg.IpcSet(uapi); err != nil {
		wg.Close()
		<-wg.Wait()
		return nil, fmt.Errorf("configure WARP device: %w", err)
	}
	if err := wg.Up(); err != nil {
		wg.Close()
		<-wg.Wait()
		return nil, fmt.Errorf("start WARP device: %w", err)
	}
	e := &exit{id: reg.ID, stack: stack, device: wg, network: network}
	egress, err := e.probe(ctx, wg, peerKey, endpoints)
	if err != nil {
		e.close()
		return nil, err
	}
	e.egress = egress
	return e, nil
}

var (
	hostIPv6Mu    sync.Mutex
	hostIPv6At    time.Time
	hostIPv6Value bool
)

// hostHasIPv6 reports whether this host currently has a globally routable
// IPv6 address. The result is cached briefly because a laptop can change
// networks while the pool is running, and endpoint order should follow it.
func hostHasIPv6() bool {
	hostIPv6Mu.Lock()
	defer hostIPv6Mu.Unlock()
	if !hostIPv6At.IsZero() && time.Since(hostIPv6At) < time.Minute {
		return hostIPv6Value
	}
	hostIPv6At = time.Now()
	hostIPv6Value = detectHostIPv6()
	return hostIPv6Value
}

func detectHostIPv6() bool {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return false
	}
	for _, addr := range addrs {
		ipNet, ok := addr.(*net.IPNet)
		if !ok || ipNet.IP.IsLoopback() {
			continue
		}
		ip, ok := netip.AddrFromSlice(ipNet.IP)
		if !ok {
			continue
		}
		ip = ip.Unmap()
		if ip.Is6() && ip.IsGlobalUnicast() && !ip.IsPrivate() && !ip.IsLinkLocalUnicast() {
			return true
		}
	}
	return false
}

// preferIPv6Endpoints returns endpoints with the IPv6 ones first and the IPv4
// ones after them, both in their original order. Nothing is dropped: an IPv4
// fallback keeps WARP usable on a host that cannot reach Cloudflare over IPv6.
func preferIPv6Endpoints(endpoints []string) []string {
	ordered := make([]string, 0, len(endpoints))
	ipv4 := make([]string, 0, len(endpoints))
	for _, endpoint := range endpoints {
		host, _, err := net.SplitHostPort(endpoint)
		if err != nil {
			continue
		}
		addr, err := netip.ParseAddr(host)
		if err != nil || addr.Is4In6() {
			continue
		}
		if addr.Is6() {
			ordered = append(ordered, endpoint)
		} else {
			ipv4 = append(ipv4, endpoint)
		}
	}
	return append(ordered, ipv4...)
}

func resolveEndpoints(ctx context.Context, candidates []string, fallback string) ([]string, error) {
	if len(candidates) == 0 && fallback != "" {
		candidates = []string{fallback}
	}
	seen := make(map[string]bool)
	var endpoints []string
	for _, candidate := range candidates {
		normalized, err := normalizeEndpoint(candidate)
		if err != nil {
			continue
		}
		host, port, _ := net.SplitHostPort(normalized)
		if net.ParseIP(host) != nil {
			if !seen[normalized] {
				seen[normalized] = true
				endpoints = append(endpoints, normalized)
			}
		} else {
			addrs, err := lookupHost(ctx, host)
			if err != nil {
				continue
			}
			for _, ip := range addrs {
				value := net.JoinHostPort(ip.IP.String(), port)
				if !seen[value] {
					seen[value] = true
					endpoints = append(endpoints, value)
				}
			}
		}
		if len(endpoints) >= probeCandidates {
			break
		}
	}
	if len(endpoints) == 0 {
		return nil, fmt.Errorf("resolve WARP endpoint candidates %v", candidates)
	}
	return endpoints[:min(len(endpoints), probeCandidates)], nil
}

const dnsCacheTTL = 10 * time.Minute

type dnsCacheEntry struct {
	addrs   []net.IPAddr
	expires time.Time
}

var dnsCache sync.Map // host -> dnsCacheEntry

// lookupHost resolves a Cloudflare endpoint host and caches the answer for a
// short while. Every exit re-resolves the same peer host, so without a cache a
// burst of registrations repeats the same DNS query for no benefit.
func lookupHost(ctx context.Context, host string) ([]net.IPAddr, error) {
	if cached, ok := dnsCache.Load(host); ok {
		entry := cached.(dnsCacheEntry)
		if time.Now().Before(entry.expires) {
			return entry.addrs, nil
		}
	}
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	dnsCache.Store(host, dnsCacheEntry{addrs: addrs, expires: time.Now().Add(dnsCacheTTL)})
	return addrs, nil
}

func normalizeEndpoint(endpoint string) (string, error) {
	endpoint = strings.TrimSpace(endpoint)
	if !strings.Contains(endpoint, ":") {
		endpoint = net.JoinHostPort(endpoint, "2408")
	} else if addr, err := netip.ParseAddr(endpoint); err == nil {
		endpoint = net.JoinHostPort(addr.String(), "2408")
	}
	host, port, err := net.SplitHostPort(endpoint)
	if err != nil {
		return "", fmt.Errorf("invalid WARP endpoint %q", endpoint)
	}
	if port == "" || port == "0" {
		port = "2408"
	}
	return net.JoinHostPort(host, port), nil
}

func parseAddress(value string) (netip.Addr, error) {
	value = strings.TrimSpace(value)
	if addr, err := netip.ParseAddr(value); err == nil {
		return addr.Unmap(), nil
	}
	prefix, err := netip.ParsePrefix(value)
	if err != nil {
		return netip.Addr{}, err
	}
	return prefix.Addr().Unmap(), nil
}

func uapiKey(encoded string) (string, error) {
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil || len(key) != 32 {
		return "", errors.New("invalid WireGuard key")
	}
	return hex.EncodeToString(key), nil
}

type probeTarget struct {
	network string
	url     string
}

func probeTargets(stack Stack) []probeTarget {
	switch stack {
	case StackIPv4:
		return []probeTarget{{network: "tcp4", url: "https://1.1.1.1/cdn-cgi/trace"}}
	case StackDual:
		return []probeTarget{
			{network: "tcp4", url: "https://1.1.1.1/cdn-cgi/trace"},
			{network: "tcp6", url: "https://[2606:4700:4700::1111]/cdn-cgi/trace"},
		}
	default:
		return []probeTarget{{network: "tcp6", url: "https://[2606:4700:4700::1111]/cdn-cgi/trace"}}
	}
}

func (e *exit) probe(ctx context.Context, wg *device.Device, peerKey string, endpoints []string) (netip.Addr, error) {
	// Prefer peers that answered before and skip the ones already known to be
	// unreachable; if that leaves nothing to try, walk the whole list once.
	addr, attempted, err := e.probePass(ctx, wg, peerKey, endpoints, true)
	if err == nil {
		return addr, nil
	}
	if attempted == 0 {
		addr, _, err = e.probePass(ctx, wg, peerKey, endpoints, false)
		if err == nil {
			return addr, nil
		}
	}
	return netip.Addr{}, fmt.Errorf("verify WARP %s egress: %w", e.stack, err)
}

// probePass walks the candidate endpoints once and reports what it learned. A
// peer that answers usually does so on the first handshake, so the sweep is
// ordered by what previous exits saw (orderEndpointsByMemory) and stops at the
// first working peer. Blocks where every candidate timed out are recorded as
// unreachable, which keeps later exits from paying that timeout again.
func (e *exit) probePass(ctx context.Context, wg *device.Device, peerKey string, endpoints []string, skipDead bool) (netip.Addr, int, error) {
	type blockStat struct{ failed, total int }
	stats := make(map[string]*blockStat, 2)
	var (
		lastErr   error
		attempted int
	)
	for _, endpoint := range endpoints {
		block := endpointBlock(endpoint)
		stat := stats[block]
		if stat == nil {
			stat = &blockStat{}
			stats[block] = stat
		}
		stat.total++
		if skipDead && endpointBlockDead(block) {
			continue
		}
		// Point the peer at this candidate before probing it. Re-pointing is a
		// cheap local IPC call, so the device state stays obvious across passes.
		if err := wg.IpcSet(fmt.Sprintf("public_key=%s\nendpoint=%s\n", peerKey, endpoint)); err != nil {
			lastErr = fmt.Errorf("switch WARP endpoint to %s: %w", endpoint, err)
			stat.failed++
			continue
		}
		attempted++
		for _, target := range probeTargets(e.stack) {
			addr, err := e.probeTarget(ctx, target)
			if err == nil {
				rememberEndpoint(endpoint)
				return addr, attempted, nil
			}
			lastErr = err
		}
		stat.failed++
	}
	if lastErr == nil {
		lastErr = errNoReachablePeer
	}
	// Only a block whose every candidate timed out is written off: peers inside
	// one block are published together, but a block is not dead just because one
	// of its addresses was not the one that answered.
	for block, stat := range stats {
		if stat.total > 0 && stat.failed == stat.total {
			forgetBlock(block)
		}
	}
	return netip.Addr{}, attempted, lastErr
}

var errNoReachablePeer = errors.New("no reachable WARP peer")

// endpointMemory remembers which Cloudflare peer block answered last and which
// blocks did not, so a new exit starts its probe where the previous one
// succeeded. Every exit talks to the same anycast peers and only some of them
// are reachable from a given source, so without this each registration pays a
// probe timeout per dead candidate before the tunnel comes up.
var endpointMemory = struct {
	sync.Mutex
	lastGood string
	dead     map[string]time.Time
}{dead: make(map[string]time.Time)}

const (
	// endpointDeadTTL bounds how long an unreachable peer block is skipped. It is
	// short because a peer can come back, and a skipped block is still tried when
	// nothing else is left.
	endpointDeadTTL = 10 * time.Minute
	// maxDeadEndpointBlocks keeps the map from growing without bound on a host
	// that keeps resolving new candidate addresses.
	maxDeadEndpointBlocks = 64
)

// endpointBlock groups candidate endpoints the way Cloudflare publishes them:
// peers inside one IPv6 /64 or one IPv4 /24 are either all usable or all dead
// from a given source, so one sweep of a block says something about the rest.
func endpointBlock(endpoint string) string {
	host, _, err := net.SplitHostPort(endpoint)
	if err != nil {
		return endpoint
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return endpoint
	}
	if addr.Is4In6() {
		addr = addr.Unmap()
	}
	bits := 64
	if addr.Is4() {
		bits = 24
	}
	prefix, err := addr.Prefix(bits)
	if err != nil {
		return endpoint
	}
	return prefix.String()
}

func rememberEndpoint(endpoint string) {
	block := endpointBlock(endpoint)
	endpointMemory.Lock()
	endpointMemory.lastGood = block
	delete(endpointMemory.dead, block)
	endpointMemory.Unlock()
}

func forgetBlock(block string) {
	endpointMemory.Lock()
	defer endpointMemory.Unlock()
	if len(endpointMemory.dead) >= maxDeadEndpointBlocks {
		now := time.Now()
		for candidate, until := range endpointMemory.dead {
			if !until.After(now) {
				delete(endpointMemory.dead, candidate)
			}
		}
	}
	if endpointMemory.lastGood == block {
		endpointMemory.lastGood = ""
	}
	endpointMemory.dead[block] = time.Now().Add(endpointDeadTTL)
}

// endpointBlockDead reports whether a recent sweep gave up on this block.
func endpointBlockDead(block string) bool {
	endpointMemory.Lock()
	until, ok := endpointMemory.dead[block]
	endpointMemory.Unlock()
	return ok && until.After(time.Now())
}

// orderEndpointsByMemory moves candidates from the block that answered last to
// the front, then the blocks still considered reachable, and the skipped blocks
// last. Interleaving the tail tries one candidate per block before the second
// candidate of any block, so a live block is reached before a dead one is
// walked end to end.
func orderEndpointsByMemory(endpoints []string) []string {
	if len(endpoints) < 2 {
		return endpoints
	}
	endpointMemory.Lock()
	lastGood := endpointMemory.lastGood
	dead := make(map[string]bool, len(endpointMemory.dead))
	now := time.Now()
	for block, until := range endpointMemory.dead {
		if until.After(now) {
			dead[block] = true
		}
	}
	endpointMemory.Unlock()

	preferred := make([]string, 0, len(endpoints))
	var alive, unreachable []string
	for _, endpoint := range endpoints {
		block := endpointBlock(endpoint)
		switch {
		case lastGood != "" && block == lastGood:
			preferred = append(preferred, endpoint)
		case dead[block]:
			unreachable = append(unreachable, endpoint)
		default:
			alive = append(alive, endpoint)
		}
	}
	return append(append(preferred, interleaveByBlock(alive)...), interleaveByBlock(unreachable)...)
}

// interleaveByBlock returns the endpoints in rounds: the first candidate of
// every block, then the second of every block, and so on. Candidate lists come
// grouped by host, so a plain walk spends several timeouts on one unreachable
// block before it ever tries a block that answers.
func interleaveByBlock(endpoints []string) []string {
	if len(endpoints) < 2 {
		return endpoints
	}
	var order []string
	groups := make(map[string][]string, 2)
	for _, endpoint := range endpoints {
		block := endpointBlock(endpoint)
		if _, ok := groups[block]; !ok {
			order = append(order, block)
		}
		groups[block] = append(groups[block], endpoint)
	}
	if len(order) < 2 {
		return endpoints
	}
	interleaved := make([]string, 0, len(endpoints))
	for round := 0; ; round++ {
		added := false
		for _, block := range order {
			if round < len(groups[block]) {
				interleaved = append(interleaved, groups[block][round])
				added = true
			}
		}
		if !added {
			return interleaved
		}
	}
}

func (e *exit) probeTarget(ctx context.Context, target probeTarget) (netip.Addr, error) {
	probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	transport := &http.Transport{
		Proxy:               nil,
		ForceAttemptHTTP2:   false,
		DisableKeepAlives:   true,
		TLSHandshakeTimeout: 10 * time.Second,
		DialContext: func(ctx context.Context, _, address string) (net.Conn, error) {
			return e.network.DialContext(ctx, target.network, address)
		},
	}
	defer transport.CloseIdleConnections()
	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, target.url, nil)
	if err != nil {
		return netip.Addr{}, err
	}
	resp, err := (&http.Client{Transport: transport}).Do(req)
	if err != nil {
		return netip.Addr{}, err
	}
	defer resp.Body.Close()
	egress, err := parseTrace(resp.Body)
	if err != nil {
		return netip.Addr{}, err
	}
	if target.network == "tcp4" && !egress.Is4() {
		return netip.Addr{}, fmt.Errorf("WARP trace over IPv4 reported %s", egress)
	}
	if target.network == "tcp6" && !egress.Is6() {
		return netip.Addr{}, fmt.Errorf("WARP trace over IPv6 reported %s", egress)
	}
	return egress, nil
}

func parseTrace(body io.Reader) (netip.Addr, error) {
	scanner := bufio.NewScanner(io.LimitReader(body, 64<<10))
	var rawIP string
	warpEnabled := false
	for scanner.Scan() {
		key, value, ok := strings.Cut(scanner.Text(), "=")
		if !ok {
			continue
		}
		switch strings.TrimSpace(key) {
		case "ip":
			rawIP = strings.TrimSpace(value)
		case "warp":
			warpEnabled = strings.TrimSpace(value) == "on"
		}
	}
	if err := scanner.Err(); err != nil {
		return netip.Addr{}, err
	}
	if !warpEnabled {
		return netip.Addr{}, errors.New("Cloudflare trace reports warp=off")
	}
	addr, err := netip.ParseAddr(rawIP)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("invalid trace IP %q", rawIP)
	}
	addr = addr.Unmap()
	if (!addr.Is4() && !addr.Is6()) || !addr.IsGlobalUnicast() || addr.IsPrivate() {
		return netip.Addr{}, fmt.Errorf("trace returned non-public address %s", addr)
	}
	return addr, nil
}

func (e *exit) addRef() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.retired || e.closed {
		return false
	}
	e.refs++
	return true
}

func (e *exit) release() {
	e.mu.Lock()
	if e.refs > 0 {
		e.refs--
	}
	closeNow := e.refs == 0 && e.retired && !e.closed
	e.mu.Unlock()
	if closeNow {
		e.close()
	}
}

func (e *exit) retire() {
	e.mu.Lock()
	e.retired = true
	closeNow := e.refs == 0 && !e.closed
	e.mu.Unlock()
	if closeNow {
		e.close()
	}
}

func (e *exit) close() {
	e.closeOnce.Do(func() {
		e.mu.Lock()
		e.closed = true
		e.mu.Unlock()
		if e.device != nil {
			e.device.Close()
			<-e.device.Wait()
		}
	})
}

func (e *exit) observe(status int, body []byte) (bool, string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.penalized {
		return false, ""
	}
	if status == http.StatusForbidden {
		reason := "HTTP 403"
		if networkAnomaly(body) {
			reason += " network anomaly"
		}
		return true, reason
	}
	// A free-tier 429 is scoped to the egress address, not to the request, the
	// key or the session: retrying the same exit repeats it. Replace the exit
	// on the first one, as the instant-rotate deployment does, so the retry
	// draws a fresh outer source and a new public egress.
	if status == http.StatusTooManyRequests {
		return true, "HTTP 429"
	}
	return false, ""
}

func (e *exit) isPenalized() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.penalized
}

func (e *exit) markPenalized() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.penalized {
		return false
	}
	e.penalized = true
	return true
}

func (e *exit) clearPenalty() {
	e.mu.Lock()
	e.penalized = false
	e.mu.Unlock()
}

func networkAnomaly(body []byte) bool {
	return strings.Contains(strings.ToLower(string(body)), "network")
}

func (e *exit) dialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if e.network == nil {
		return nil, errors.New("WARP network is closed")
	}
	network = strings.ToLower(strings.TrimSpace(network))
	switch network {
	case "tcp":
		network = e.dialNetwork()
	case "tcp4", "tcp6":
	default:
		return nil, fmt.Errorf("WARP supports TCP only, got %q", network)
	}
	switch e.stack {
	case StackIPv4:
		if network != "tcp4" {
			return nil, fmt.Errorf("WARP IPv4 stack cannot dial %q", network)
		}
	case StackIPv6:
		if network != "tcp6" {
			return nil, fmt.Errorf("WARP IPv6 stack cannot dial %q", network)
		}
	case StackDual:
		// Both address families are present in the netstack.
	default:
		return nil, fmt.Errorf("unsupported WARP stack %q", e.stack)
	}
	return e.network.DialContext(ctx, network, address)
}

func (e *exit) dialNetwork() string {
	switch e.stack {
	case StackIPv4:
		return "tcp4"
	case StackDual:
		if e.egress.Is4() {
			return "tcp4"
		}
		return "tcp6"
	default:
		return "tcp6"
	}
}

func (e *exit) egressKey() string {
	if e.egress.Is4() {
		return netip.PrefixFrom(e.egress, 32).Masked().String()
	}
	if !e.egress.Is6() {
		return ""
	}
	// The free tier treats a Cloudflare /64 as one quota scope: rotating inside
	// the same /64 yields a new /128 but the same exhausted quota. Keying on
	// the /64 keeps every active and cooling exit on a distinct scope, so a
	// replacement is a real rotation rather than a cosmetic address change.
	return netip.PrefixFrom(e.egress, 64).Masked().String()
}

// idle reports whether no lease currently holds this exit.
func (e *exit) idle() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.refs == 0 && !e.retired && !e.closed
}
