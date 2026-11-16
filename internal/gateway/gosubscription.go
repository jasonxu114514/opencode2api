package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"opencode2api/internal/config"
	"opencode2api/internal/goauth"
	"opencode2api/internal/httpx"
	"opencode2api/internal/identity"
	"opencode2api/internal/models"
	wire "opencode2api/internal/protocol"
	"opencode2api/internal/telemetry"
)

func (g *Gateway) goAvailable() bool {
	return g.cfg.GoSubscription != nil || g.goNodes.Len() > 0
}

func (g *Gateway) routeForModel(model string, tier config.Tier) (models.Route, error) {
	if connection := g.cfg.GoSubscription; connection != nil {
		if subscriptionRoute, ok := connection.Routes[model]; ok && (tier == "" || tier == config.TierGo) {
			if connection.NeedsLogin {
				return models.Route{}, &goauth.OAuthError{Code: "invalid_grant"}
			}
			if g.cfg.Models.FreeOnly && !g.catalog.IsFreeModel(model) {
				return models.Route{}, errors.New("this subscription model is excluded by models.free_only")
			}
			return models.Route{
				ID: model, Tier: config.TierGo, Protocol: subscriptionRoute.Protocol,
				Protocols: map[config.Tier]wire.Protocol{config.TierGo: subscriptionRoute.Protocol},
				KeyTiers:  []config.Tier{config.TierGo},
			}, nil
		}
		if tier == config.TierGo {
			return models.Route{}, errors.New("model is not included in the connected Go subscription")
		}
	}
	if tier != "" {
		return g.catalog.RouteForTier(model, tier, g.zenNodes.Len() > 0, g.goAvailable())
	}
	return g.catalog.Route(model, g.zenNodes.Len() > 0, g.goNodes.Len() > 0, g.cfg.Anonymous)
}

func (g *Gateway) seedGoSubscriptionCatalog(capabilities *models.Capabilities) []string {
	connection := g.cfg.GoSubscription
	if connection == nil {
		return nil
	}
	if capabilities.Protocols == nil {
		capabilities.Protocols = map[config.Tier]map[string]wire.Protocol{}
	}
	if capabilities.Unsupported == nil {
		capabilities.Unsupported = map[config.Tier]map[string]bool{}
	}
	protocols := make(map[string]wire.Protocol, len(connection.Routes))
	modelIDs := make([]string, 0, len(connection.Routes))
	for model, route := range connection.Routes {
		modelIDs = append(modelIDs, model)
		protocols[model] = route.Protocol
	}
	sort.Strings(modelIDs)
	capabilities.Protocols[config.TierGo] = protocols
	capabilities.Unsupported[config.TierGo] = map[string]bool{}
	return modelIDs
}

func (g *Gateway) restoreGoSubscriptionCatalog() {
	if g.cfg.GoSubscription == nil {
		return
	}
	capabilities := models.Capabilities{}
	modelIDs := g.seedGoSubscriptionCatalog(&capabilities)
	g.catalog.ReplaceWithCapabilities(nil, modelIDs, capabilities.Protocols, capabilities.Unsupported, nil)
}

func (g *Gateway) subscriptionConnection(ctx context.Context) (*goauth.Connection, error) {
	snapshot := g.cfg.GoSubscription
	if snapshot == nil {
		return nil, errors.New("Go subscription is not connected")
	}
	connection := snapshot
	if g.resolveGoSubscription != nil {
		resolved, err := g.resolveGoSubscription(ctx)
		if err != nil {
			return nil, err
		}
		connection = resolved
	}
	if connection == nil || connection.Credential.AccountID != snapshot.Credential.AccountID || connection.Credential.OrganizationID != snapshot.Credential.OrganizationID || connection.Proxy != snapshot.Proxy {
		return nil, errors.New("Go subscription connection changed; retry with the current configuration")
	}
	if connection.NeedsLogin {
		return nil, &goauth.OAuthError{Code: "invalid_grant"}
	}
	if err := connection.Validate(); err != nil {
		return nil, err
	}
	if !time.Now().Before(connection.Credential.ExpiresAt) {
		return nil, &goauth.OAuthError{Code: "expired_token"}
	}
	return connection, nil
}

func subscriptionAccountID(connection *goauth.Connection) string {
	return config.Fingerprint(connection.Credential.AccountID + "\x00" + connection.Credential.OrganizationID)
}

func (g *Gateway) subscriptionProxy(ctx context.Context, connection *goauth.Connection, affinity string, offset int) (*proxyTransport, error) {
	if g.transports == nil || len(g.transports.items) == 0 {
		return nil, errProxyUnavailable
	}
	var pinned *proxyTransport
	if selection, selected := relayOverrideFrom(ctx); selected {
		var found bool
		pinned, found = g.RelayProxy(selection)
		if !found {
			return nil, errRelayNotFound
		}
	}
	if connection.Proxy != "" {
		for _, proxy := range g.transports.items {
			if proxy == nil || proxy.name != connection.Proxy {
				continue
			}
			if pinned != nil && pinned != proxy {
				return nil, errors.New("the selected relay conflicts with the Go subscription proxy binding")
			}
			if !proxy.healthy.Load() && !isDiagnosticRequest(ctx) {
				return nil, errProxyUnavailable
			}
			return proxy, nil
		}
		return nil, errProxyUnavailable
	}
	if pinned != nil {
		return pinned, nil
	}
	digest := sha256.Sum256([]byte(subscriptionAccountID(connection) + "\x00" + affinity))
	start := int(binary.LittleEndian.Uint64(digest[:8]) % uint64(len(g.transports.items)))
	for step := range len(g.transports.items) {
		proxy := g.transports.items[(start+offset+step)%len(g.transports.items)]
		if proxy != nil && proxy.healthy.Load() {
			return proxy, nil
		}
	}
	return nil, errProxyUnavailable
}

func (g *Gateway) goSubscriptionAttempt(ctx context.Context, route models.Route, body []byte, ids identity.RequestIDs, attempt, offset int, firstEventTimeout time.Duration) (*http.Response, error) {
	connection, err := g.subscriptionConnection(ctx)
	if err != nil {
		return nil, err
	}
	subscriptionRoute, ok := connection.Routes[route.ID]
	if !ok || subscriptionRoute.Protocol != route.Protocol || subscriptionRoute != g.cfg.GoSubscription.Routes[route.ID] {
		return nil, errors.New("Go subscription route changed; retry with the current configuration")
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil || payload == nil {
		return nil, errors.New("invalid Go subscription request body")
	}
	payload["model"], err = json.Marshal(subscriptionRoute.ModelID)
	if err != nil {
		return nil, errors.New("invalid Go subscription model")
	}
	body, err = json.Marshal(payload)
	if err != nil {
		return nil, errors.New("invalid Go subscription request body")
	}
	proxy, err := g.subscriptionProxy(ctx, connection, ids.Session, offset)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(subscriptionRoute.BaseURL, "/")+wire.Path(subscriptionRoute.Protocol), bytes.NewReader(body))
	if err != nil {
		return nil, errors.New("invalid Go subscription endpoint")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+connection.Credential.AccessToken)
	req.Header.Set("x-opencode-session", ids.Session)
	req.Header.Set("User-Agent", httpx.V2UserAgent())
	if connection.Credential.OrganizationID != "" {
		req.Header.Set("x-org-id", connection.Credential.OrganizationID)
	}
	if route.Protocol == wire.Anthropic {
		req.Header.Set("anthropic-version", "2023-06-01")
	}
	req.GetBody = nil
	if proxy.pool {
		req.Close = true
	}
	client := *proxy.client
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	accountID := subscriptionAccountID(connection)
	setRequestCredential(ctx, config.TierGo, accountID, "subscription", false, proxy)
	setRequestShape(ctx, false, nil)
	if meta := telemetry.MetaFromContext(ctx); meta != nil {
		meta.Attempts = attempt
		meta.Tier = string(config.TierGo)
	}
	started := time.Now()
	resp, err := doInferenceAttempt(&client, req, firstEventTimeout, g.attemptTimeout)
	if resp != nil {
		sanitizeGoSubscriptionResponse(resp, connection.Credential)
		if resp.StatusCode == http.StatusUnauthorized && g.onGoSubscriptionUnauthorized != nil {
			g.onGoSubscriptionUnauthorized(ctx, connection.Credential)
		}
	}
	if err != nil {
		err = &goSubscriptionError{cause: err, message: redactGoSubscriptionValue(err.Error(), connection.Credential)}
	}
	if ctx.Err() == nil && !isDiagnosticRequest(ctx) {
		g.syncProxyResult(ctx, proxy, upstreamStatus(resp), err)
	}
	g.recordUpstreamAttempt(ctx, route, ids, attempt, accountID, "subscription", false, proxy, resp, err, time.Since(started))
	if err != nil {
		return resp, asProxyUnavailable(proxy, err)
	}
	return resp, nil
}

type goSubscriptionError struct {
	cause   error
	message string
}

func (err *goSubscriptionError) Error() string { return err.message }
func (err *goSubscriptionError) Unwrap() error { return err.cause }

func redactGoSubscriptionValue(value string, credential goauth.Credential) string {
	secrets := []string{credential.AccessToken, credential.RefreshToken}
	if len(secrets[1]) > len(secrets[0]) {
		secrets[0], secrets[1] = secrets[1], secrets[0]
	}
	for _, secret := range secrets {
		if secret == "" {
			continue
		}
		value = strings.ReplaceAll(value, secret, "***")
		encoded, _ := json.Marshal(secret)
		value = strings.ReplaceAll(value, string(encoded[1:len(encoded)-1]), "***")
	}
	return value
}

func sanitizeGoSubscriptionResponse(response *http.Response, credential goauth.Credential) {
	if response.Header == nil {
		response.Header = make(http.Header)
	}
	response.Request = nil
	response.Status = redactGoSubscriptionValue(response.Status, credential)
	if response.StatusCode/100 != 2 {
		var body []byte
		var readErr error
		if response.Body != nil {
			body, readErr = io.ReadAll(io.LimitReader(response.Body, 1<<20))
			response.Body.Close()
		}
		if readErr != nil || len(body) >= 1<<20 {
			body = []byte(`{"error":{"message":"Go subscription upstream request was rejected"}}`)
			response.Header.Set("Content-Type", "application/json")
			response.Header.Del("Content-Encoding")
		} else {
			body = []byte(redactGoSubscriptionValue(string(body), credential))
		}
		response.Body = io.NopCloser(bytes.NewReader(body))
		response.ContentLength = int64(len(body))
		response.TransferEncoding = nil
		response.Header.Set("Content-Length", strconv.Itoa(len(body)))
	}
	for _, header := range []http.Header{response.Header, response.Trailer} {
		for name, values := range header {
			for index, value := range values {
				values[index] = redactGoSubscriptionValue(value, credential)
			}
			safeName := redactGoSubscriptionValue(name, credential)
			if safeName != name {
				delete(header, name)
				header[safeName] = append(header[safeName], values...)
			}
		}
	}
}

func (g *Gateway) doGoSubscriptionUpstream(ctx context.Context, route models.Route, bodies map[config.Tier][]byte, ids identity.RequestIDs, attemptOffset int) (response *http.Response, resultErr error, resultAttempts int) {
	body := bodies[config.TierGo]
	if len(body) == 0 {
		return nil, errors.New("no prepared Go subscription request body"), 0
	}
	limit := max(g.cfg.Retry.MaxAttempts, 1)
	if isDiagnosticRequest(ctx) {
		limit = 1
	}
	var lastResponse *http.Response
	var lastErr error
	defer func() {
		if lastResponse != nil && lastResponse != response {
			httpx.DrainAndClose(lastResponse.Body)
		}
	}()
	for attempts := 0; attempts < limit; attempts++ {
		if ctx.Err() != nil {
			lastErr = ctx.Err()
			break
		}
		bufferRetryResponse(lastResponse)
		resp, err := g.goSubscriptionAttempt(ctx, route, body, ids, attemptOffset+attempts+1, attempts, time.Duration(g.cfg.Performance.FirstEventTimeoutSeconds)*time.Second)
		resultAttempts = attempts + 1
		lastResponse = replaceRetryResponse(lastResponse, resp)
		lastErr = err
		if ctx.Err() != nil {
			break
		}
		var authorizationError *goauth.OAuthError
		if errors.As(err, &authorizationError) {
			return nil, err, resultAttempts
		}
		if resp != nil && (err == nil && resp.StatusCode/100 == 2 || resp.StatusCode < 500) {
			return resp, err, resultAttempts
		}
	}
	if errors.Is(ctx.Err(), context.Canceled) {
		return nil, ctx.Err(), resultAttempts
	}
	if lastResponse != nil {
		return lastResponse, nil, resultAttempts
	}
	if lastErr == nil {
		lastErr = errors.New("Go subscription request could not be completed")
	}
	return nil, lastErr, resultAttempts
}

func (g *Gateway) routeDiagnostic(model string, requested wire.Protocol) models.RouteDiagnostic {
	diagnostic := g.catalog.Diagnostic(model, requested, g.zenNodes.Len() > 0, g.goNodes.Len() > 0, g.cfg.Anonymous)
	subscriptionModel := false
	if connection := g.cfg.GoSubscription; connection != nil {
		subscriptionRoute, included := connection.Routes[model]
		subscriptionModel, diagnostic.AvailableGo = included, included
		if included {
			diagnostic.NativeProtocol = subscriptionRoute.Protocol
			diagnostic.NativeProtocols = map[config.Tier]wire.Protocol{config.TierGo: subscriptionRoute.Protocol}
			diagnostic.ProtocolSource = "go_subscription"
			diagnostic.Channel = "subscription"
			diagnostic.KeyID = subscriptionAccountID(connection)
		}
	}
	route, err := g.routeForModel(model, "")
	if err != nil {
		diagnostic.RouteError = err.Error()
		if subscriptionModel {
			diagnostic.Tier, diagnostic.Anonymous, diagnostic.KeyTiers = "", false, nil
		}
		return diagnostic
	}
	diagnostic.RouteError = ""
	diagnostic.Tier, diagnostic.Anonymous = route.Tier, route.Anonymous
	diagnostic.KeyTiers = route.KeyTiers
	diagnostic.NativeProtocol, diagnostic.NativeProtocols = route.Protocol, route.Protocols
	return diagnostic
}
