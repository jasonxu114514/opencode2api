package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"opencode2api/internal/config"
	"opencode2api/internal/identity"
	wire "opencode2api/internal/protocol"
)

const proxyModelCheckTimeout = 45 * time.Second
const proxyModelAttemptTimeout = 20 * time.Second

type ProxyCheckAttempt struct {
	Upstream   string `json:"upstream"`
	HTTPStatus int    `json:"http_status"`
	ErrorCode  string `json:"error_code,omitempty"`
	Error      string `json:"error,omitempty"`
}

type ProxyCheckResult struct {
	OK         bool                `json:"ok"`
	HTTPStatus int                 `json:"http_status"`
	DurationMS int64               `json:"duration_ms"`
	Model      string              `json:"model"`
	Output     string              `json:"output,omitempty"`
	ErrorCode  string              `json:"error_code,omitempty"`
	Error      string              `json:"error,omitempty"`
	Attempts   []ProxyCheckAttempt `json:"attempts,omitempty"`
}

// CheckProxy pins an anonymous inference request to one disposable transport.
// Neither paid keys, another proxy, nor a direct fallback can hide a failure.
// Live health, bindings, cooldowns, and model availability are left untouched.
func (m *RuntimeManager) CheckProxy(ctx context.Context, raw string) (ProxyCheckResult, error) {
	runtime := m.current.Load()
	if runtime == nil || runtime.gateway == nil || runtime.gateway.catalog == nil {
		return ProxyCheckResult{}, errors.New("free model catalog is not ready; refresh models and retry")
	}
	g := runtime.gateway
	candidates := g.catalog.List()
	// Prefer a short, known chat probe; fall back to another catalogued free model.
	candidates = append([]string{"glm-5.3-flash-free"}, candidates...)
	model, protocol := "", wire.Protocol("")
	for _, candidate := range candidates {
		d := g.catalog.Diagnostic(candidate, wire.Chat, false, false, true)
		p := d.NativeProtocols[config.TierZen]
		if d.AvailableZen && g.catalog.IsFreeModel(candidate) && g.catalog.Supported(candidate) && (p == wire.Chat || p == wire.Anthropic || p == wire.Responses) {
			model, protocol = candidate, p
			break
		}
	}
	if model == "" {
		return ProxyCheckResult{}, errors.New("no supported anonymous free model in the catalog; refresh models and retry")
	}
	pool, err := newTransportPool([]string{raw}, runtime.config.Performance, proxyModelAttemptTimeout)
	if err != nil {
		return ProxyCheckResult{}, errors.New("could not create proxy transport")
	}
	client := pool.items[0].client
	defer client.CloseIdleConnections()
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	result := checkProxyModel(ctx, client, g.anonymousUpstreams(), model, protocol, proxyModelCheckTimeout, proxyModelAttemptTimeout)
	result.Error = redactProxyCheckError(result.Error, raw)
	result.Output = redactProxyCheckError(result.Output, raw)
	for i := range result.Attempts {
		result.Attempts[i].Upstream = config.RedactURL(result.Attempts[i].Upstream)
		result.Attempts[i].Error = redactProxyCheckError(result.Attempts[i].Error, raw)
	}
	if m.redactor != nil {
		result.Error = m.redactor.String(result.Error)
		result.Output = m.redactor.String(result.Output)
		for i := range result.Attempts {
			result.Attempts[i].Error = m.redactor.String(result.Attempts[i].Error)
		}
	}
	return result, nil
}

func checkProxyModel(ctx context.Context, client *http.Client, upstreams []string, model string, protocol wire.Protocol, timeout, attemptTimeout time.Duration) (result ProxyCheckResult) {
	started := time.Now()
	defer func() { result.DurationMS = time.Since(started).Milliseconds() }()
	result.Model = model
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ids := identity.RequestIDs{Session: identity.CanonicalSessionID(identity.RandomID("proxy-check", 16)), Project: "opencode2api:proxy-check"}
	for _, base := range upstreams {
		if ctx.Err() != nil {
			result.ErrorCode, result.Error = "timeout", ctx.Err().Error()
			break
		}
		attemptCtx, stop := context.WithTimeout(ctx, attemptTimeout)
		attempt, output := proxyModelAttempt(attemptCtx, client, base, model, protocol, ids)
		stop()
		result.Attempts = append(result.Attempts, attempt)
		result.HTTPStatus, result.ErrorCode, result.Error = attempt.HTTPStatus, attempt.ErrorCode, attempt.Error
		if attempt.ErrorCode == "" {
			result.OK, result.Output = true, output
			break
		}
	}
	return result
}

func proxyModelAttempt(ctx context.Context, client *http.Client, base, model string, protocol wire.Protocol, ids identity.RequestIDs) (result ProxyCheckAttempt, output string) {
	result.Upstream = base
	payload, err := wire.ConvertRequest(wire.Chat, protocol, map[string]any{
		"model": model, "stream": true, "max_tokens": 1024,
		"messages": []any{map[string]any{"role": "user", "content": "Reply with exactly OK. Do not use tools."}},
	})
	if err != nil {
		result.ErrorCode, result.Error = "invalid_request", err.Error()
		return
	}
	body, err := json.Marshal(payload)
	if err != nil {
		result.ErrorCode, result.Error = "invalid_request", err.Error()
		return
	}
	body, _ = prepareAnonymousBody(body, protocol, isLegacyZenUpstream(base))
	ids.Request = identity.RandomID("req", 16)
	req, err := newUpstreamRequest(ctx, base, protocol, body, ids, anonymousZenKey)
	if err != nil {
		result.ErrorCode, result.Error = "invalid_request", err.Error()
		return
	}
	response, err := client.Do(req)
	if err != nil {
		result.ErrorCode, result.Error = "connection_failed", err.Error()
		var ne net.Error
		if ctx.Err() != nil || (errors.As(err, &ne) && ne.Timeout()) {
			result.ErrorCode = "timeout"
		}
		if strings.Contains(strings.ToLower(err.Error()), "proxy authentication required") {
			result.ErrorCode = "proxy_auth"
			result.HTTPStatus = http.StatusProxyAuthRequired
		}
		return
	}
	defer response.Body.Close()
	result.HTTPStatus = response.StatusCode
	if response.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(response.Body, 64<<10))
		result.ErrorCode = "upstream_error"
		switch response.StatusCode {
		case 401:
			result.ErrorCode = "unauthorized"
		case 403:
			result.ErrorCode = "free_tier_denied"
		case 407:
			result.ErrorCode = "proxy_auth"
		case 429:
			result.ErrorCode = "rate_limited"
		case 400, 404, 410, 422:
			if isModelUnavailable(response.StatusCode, string(b)) {
				result.ErrorCode = "model_unavailable"
			}
		}
		result.Error = fmt.Sprintf("HTTP %d: %s", response.StatusCode, proxyUpstreamError(b))
		return
	}
	var b []byte
	if strings.Contains(strings.ToLower(response.Header.Get("Content-Type")), "text/event-stream") {
		b, err = wire.CollapseStream(io.LimitReader(response.Body, 1<<20), protocol, model)
	} else {
		b, err = io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
		if len(b) > 1<<20 {
			err = errors.New("model response exceeded 1 MiB")
		}
	}
	if err == nil {
		b, err = wire.ConvertResponse(protocol, wire.Chat, b)
	}
	var document struct {
		Error   json.RawMessage `json:"error"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err == nil {
		err = json.Unmarshal(b, &document)
	}
	if err == nil && len(document.Error) > 0 && string(document.Error) != "null" {
		err = errors.New(proxyUpstreamError(b))
	}
	if err == nil && (len(document.Choices) == 0 || strings.TrimSpace(document.Choices[0].Message.Content) == "" || document.Choices[0].FinishReason == "") {
		err = errors.New("model returned no completed text reply")
	}
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	if err != nil {
		result.ErrorCode, result.Error = "invalid_response", limitProxyText(err.Error(), 512)
		if ctx.Err() != nil {
			result.ErrorCode = "timeout"
		}
		return
	}
	output = limitProxyText(strings.TrimSpace(document.Choices[0].Message.Content), 160)
	return
}

func proxyUpstreamError(body []byte) string {
	var envelope struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(body, &envelope) == nil && len(envelope.Error) > 0 {
		var detail struct {
			Type    string `json:"type"`
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		if json.Unmarshal(envelope.Error, &detail) == nil {
			parts := []string{}
			for _, s := range []string{detail.Type, detail.Code, detail.Message} {
				if s != "" {
					parts = append(parts, s)
				}
			}
			if len(parts) > 0 {
				return limitProxyText(strings.Join(parts, ": "), 512)
			}
		}
		var message string
		if json.Unmarshal(envelope.Error, &message) == nil {
			return limitProxyText(message, 512)
		}
	}
	text := strings.TrimSpace(string(body))
	if text == "" {
		return "empty upstream response"
	}
	return limitProxyText(text, 512)
}

func limitProxyText(value string, max int) string {
	value = strings.Join(strings.Fields(value), " ")
	runes := []rune(value)
	if len(runes) > max {
		return string(runes[:max]) + "…"
	}
	return value
}

func redactProxyCheckError(message, raw string) string {
	clean, _ := config.ParseProxyEntry(raw)
	if parsed, err := url.Parse(clean); err == nil && parsed.User != nil {
		message = strings.ReplaceAll(message, parsed.User.String(), "***")
		username := parsed.User.Username()
		password, _ := parsed.User.Password()
		for _, value := range []string{url.QueryEscape(password), password, url.QueryEscape(username), username} {
			if value != "" {
				message = strings.ReplaceAll(message, value, "***")
			}
		}
	}
	return message
}
