package gateway

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"runtime"
	"strings"
	"syscall"
	"time"

	"opencode2api/internal/config"
	"opencode2api/internal/httpx"
	"opencode2api/internal/models"
)

type ProxyCheckResult struct {
	OK        bool   `json:"ok"`
	State     string `json:"state"`
	Reason    string `json:"reason"`
	LatencyMS int64  `json:"latency_ms"`
	Status    int    `json:"status,omitempty"`
}

func (m *RuntimeController) CheckProxy(parent context.Context, value string) (result ProxyCheckResult) {
	started := time.Now()
	result = ProxyCheckResult{State: "inconclusive", Reason: "transport_error"}
	defer func() { result.LatencyMS = time.Since(started).Milliseconds() }()
	runtime := m.current.Load()
	if runtime == nil {
		result.Reason = "runtime_unavailable"
		return result
	}
	if err := config.ValidateProxyEntry(value); err != nil {
		result.Reason = "invalid_proxy"
		return result
	}
	ctx, cancel := context.WithTimeout(parent, 20*time.Second)
	defer cancel()
	performance := runtime.config.Performance
	performance.ConnectTimeoutSeconds = min(max(performance.ConnectTimeoutSeconds, 1), 10)
	performance.MaxIdleConns = 1
	performance.MaxIdleConnsPerHost = 1
	performance.MaxConnsPerHost = 1
	pool, err := transportPool([]string{value}, performance, 20*time.Second)
	if err != nil {
		return result
	}
	proxy := pool.items[0]
	defer proxy.client.CloseIdleConnections()
	defer proxy.bun.CloseIdleConnections()
	client := proxy.bun
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	target, err := url.Parse(runtime.config.Upstream.Zen)
	if err != nil || target.Host == "" {
		result.Reason = "runtime_unavailable"
		return result
	}
	target.User = nil
	target.Path = strings.TrimRight(target.Path, "/") + "/v1/models"
	target.RawPath, target.RawQuery, target.Fragment = "", "", ""
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return result
	}
	req.Close = true
	req.Header.Set("Accept", "application/json")
	userAgent := httpx.V2UserAgent()
	if models.IsV1Upstream(runtime.config.Upstream.Zen) {
		userAgent = httpx.V1UserAgent()
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("x-opencode-client", "cli")
	resp, err := client.Do(req)
	if err != nil {
		return proxyCheckFailure(ctx, value, err)
	}
	resp.Body.Close()
	return proxyCheckStatus(resp.StatusCode)
}

func proxyCheckStatus(status int) ProxyCheckResult {
	result := ProxyCheckResult{State: "rejected", Reason: "upstream_responded", Status: status}
	switch {
	case status == http.StatusTooManyRequests:
		result.State, result.Reason = "limited", "rate_limited"
	case status == http.StatusUnauthorized:
		result.OK, result.State, result.Reason = true, "available", "authentication_required"
	case status >= 200 && status < 400:
		result.OK, result.State, result.Reason = true, "available", "ok"
	}
	return result
}

func proxyCheckFailure(ctx context.Context, value string, err error) ProxyCheckResult {
	result := ProxyCheckResult{State: "inconclusive", Reason: "transport_error"}
	if errors.Is(ctx.Err(), context.Canceled) {
		result.Reason = "canceled"
		return result
	}
	var timeout net.Error
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &timeout) && timeout.Timeout()) {
		result.Reason = "check_timeout"
		return result
	}
	var connectError *httpx.ProxyConnectError
	if errors.As(err, &connectError) {
		if connectError.StatusCode == http.StatusProxyAuthRequired {
			result.State, result.Reason, result.Status = "unavailable", "proxy_authentication_required", connectError.StatusCode
			return result
		}
		if connectError.StatusCode == http.StatusTooManyRequests {
			return proxyCheckStatus(connectError.StatusCode)
		}
		result.State, result.Reason, result.Status = "rejected", "proxy_tunnel_rejected", connectError.StatusCode
		return result
	}
	clean, _ := config.ParseProxyEntry(value)
	if clean == "direct" {
		return result
	}
	var routeError *net.OpError
	if !errors.As(err, &routeError) || (routeError.Op != "proxyconnect" && routeError.Op != "socks connect") {
		return result
	}
	var dialError *net.OpError
	if !errors.As(routeError.Err, &dialError) || dialError.Op != "dial" {
		return result
	}
	var dnsError *net.DNSError
	refused := errors.Is(dialError, syscall.ECONNREFUSED)
	if runtime.GOOS == "windows" {
		const windowsConnectionRefused = syscall.Errno(10061)
		refused = refused || errors.Is(dialError, windowsConnectionRefused)
	}
	if refused || (errors.As(dialError, &dnsError) && dnsError.IsNotFound) {
		result.State, result.Reason = "unavailable", "proxy_unreachable"
	}
	return result
}
