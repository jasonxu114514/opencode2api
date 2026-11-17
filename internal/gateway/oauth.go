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
	"opencode2api/internal/httpx"
	"opencode2api/internal/identity"
	"opencode2api/internal/models"
	"opencode2api/internal/oauth"
	wire "opencode2api/internal/protocol"
	"opencode2api/internal/telemetry"
)

func (g *Gateway) goAvailable() bool {
	return g.cfg.GoOAuth != nil || g.goNodes.Len() > 0
}

func (g *Gateway) tierOAuth(tier config.Tier) *oauth.Connection {
	if tier == config.TierZen {
		return g.cfg.ZenOAuth
	}
	if tier == config.TierGo {
		return g.cfg.GoOAuth
	}
	return nil
}

func (g *Gateway) routeForModel(model string, tier config.Tier) (models.Route, error) {
	if g.cfg.GoOAuth == nil && g.cfg.ZenOAuth == nil && !g.catalog.TierPending(config.TierGo) && !g.catalog.TierPending(config.TierZen) {
		if tier != "" {
			return g.catalog.RouteForTier(model, tier, g.zenNodes.Len() > 0, g.goNodes.Len() > 0)
		}
		return g.catalog.Route(model, g.zenNodes.Len() > 0, g.goNodes.Len() > 0, g.cfg.Anonymous)
	}
	tiers := []config.Tier{config.TierGo, config.TierZen}
	if g.cfg.Prefer == config.TierZen {
		tiers = []config.Tier{config.TierZen, config.TierGo}
	}
	if tier != "" {
		tiers = []config.Tier{tier}
	}
	manualRoute, manualErr := g.catalog.Route(model, g.zenNodes.Len() > 0, g.goNodes.Len() > 0, g.cfg.Anonymous)
	for _, candidate := range tiers {
		connection := g.tierOAuth(candidate)
		if connection == nil {
			hasKeys := g.zenNodes.Len() > 0
			if candidate == config.TierGo {
				hasKeys = g.goNodes.Len() > 0
			}
			if hasKeys && g.catalog.TierPending(candidate) {
				return models.Route{}, errors.New("manual key model discovery is pending after OAuth disconnect; retry after catalog refresh")
			}
			if route, err := g.catalog.RouteForTier(model, candidate, g.zenNodes.Len() > 0, g.goNodes.Len() > 0); err == nil {
				if tier != "" {
					return route, nil
				}
				return manualRoute, manualErr
			}
			continue
		}
		if oauthRoute, ok := connection.Routes[model]; ok {
			if connection.NeedsLogin {
				return models.Route{}, &oauth.OAuthError{Code: "invalid_grant"}
			}
			if g.cfg.Models.FreeOnly && !g.catalog.IsFreeModel(model) {
				return models.Route{}, errors.New("this OAuth model is excluded by models.free_only")
			}
			return models.Route{
				ID: model, Tier: candidate, Protocol: oauthRoute.Protocol,
				Protocols: map[config.Tier]wire.Protocol{candidate: oauthRoute.Protocol},
				KeyTiers:  []config.Tier{candidate},
			}, nil
		}
		if tier != "" {
			return models.Route{}, errors.New("model is not included in the connected Console plan")
		}
	}
	if tier != "" {
		return g.catalog.RouteForTier(model, tier, g.zenNodes.Len() > 0, g.goNodes.Len() > 0)
	}
	return manualRoute, manualErr
}

func (g *Gateway) seedOAuthCatalog(capabilities *models.Capabilities, tier config.Tier) []string {
	connection := g.tierOAuth(tier)
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
	capabilities.Protocols[tier] = protocols
	capabilities.Unsupported[tier] = map[string]bool{}
	return modelIDs
}

func (g *Gateway) restoreOAuthCatalog() {
	for _, tier := range []config.Tier{config.TierGo, config.TierZen} {
		if g.tierOAuth(tier) == nil {
			g.catalog.ClearOAuthTier(tier)
		}
	}
	if g.cfg.GoOAuth == nil && g.cfg.ZenOAuth == nil {
		return
	}
	capabilities := models.Capabilities{}
	goModels := g.seedOAuthCatalog(&capabilities, config.TierGo)
	zenModels := g.seedOAuthCatalog(&capabilities, config.TierZen)
	g.catalog.ReplaceWithCapabilities(zenModels, goModels, capabilities.Protocols, capabilities.Unsupported, nil, g.oauthTiers()...)
}

func (g *Gateway) oauthTiers() []config.Tier {
	var tiers []config.Tier
	for _, tier := range []config.Tier{config.TierGo, config.TierZen} {
		if g.tierOAuth(tier) != nil {
			tiers = append(tiers, tier)
		}
	}
	return tiers
}

func (g *Gateway) oauthConnection(ctx context.Context, tier config.Tier) (*oauth.Connection, error) {
	snapshot := g.tierOAuth(tier)
	if snapshot == nil {
		return nil, errors.New("Console plan is not connected")
	}
	connection := snapshot
	resolve := g.resolveGoOAuth
	if tier == config.TierZen {
		resolve = g.resolveZenOAuth
	}
	if resolve != nil {
		resolved, err := resolve(ctx)
		if err != nil {
			return nil, err
		}
		connection = resolved
	}
	if connection == nil || connection.Credential.AccountID != snapshot.Credential.AccountID || connection.Credential.OrganizationID != snapshot.Credential.OrganizationID || connection.Proxy != snapshot.Proxy {
		return nil, errors.New("Console plan connection changed; retry with the current configuration")
	}
	if connection.NeedsLogin {
		return nil, &oauth.OAuthError{Code: "invalid_grant"}
	}
	if err := connection.ValidateForPlan(oauth.Plan(tier)); err != nil {
		return nil, err
	}
	if !time.Now().Before(connection.Credential.ExpiresAt) {
		return nil, &oauth.OAuthError{Code: "expired_token"}
	}
	return connection, nil
}

func oauthAccountID(connection *oauth.Connection) string {
	return config.Fingerprint(connection.Credential.AccountID + "\x00" + connection.Credential.OrganizationID)
}

func (g *Gateway) oauthProxy(ctx context.Context, connection *oauth.Connection, affinity string, offset int) (*proxyTransport, error) {
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
				return nil, errors.New("the selected relay conflicts with the OAuth proxy binding")
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
	digest := sha256.Sum256([]byte(oauthAccountID(connection) + "\x00" + affinity))
	start := int(binary.LittleEndian.Uint64(digest[:8]) % uint64(len(g.transports.items)))
	for step := range len(g.transports.items) {
		proxy := g.transports.items[(start+offset+step)%len(g.transports.items)]
		if proxy != nil && proxy.healthy.Load() {
			return proxy, nil
		}
	}
	return nil, errProxyUnavailable
}

func (g *Gateway) oauthAttempt(ctx context.Context, route models.Route, body []byte, ids identity.RequestIDs, attempt, offset int, firstEventTimeout time.Duration) (*http.Response, error) {
	connection, err := g.oauthConnection(ctx, route.Tier)
	if err != nil {
		return nil, err
	}
	oauthRoute, ok := connection.Routes[route.ID]
	if !ok || oauthRoute.Protocol != route.Protocol || oauthRoute != g.tierOAuth(route.Tier).Routes[route.ID] {
		return nil, errors.New("Console plan route changed; retry with the current configuration")
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil || payload == nil {
		return nil, errors.New("invalid OAuth request body")
	}
	payload["model"], err = json.Marshal(oauthRoute.ModelID)
	if err != nil {
		return nil, errors.New("invalid OAuth model")
	}
	body, err = json.Marshal(payload)
	if err != nil {
		return nil, errors.New("invalid OAuth request body")
	}
	proxy, err := g.oauthProxy(ctx, connection, ids.Session, offset)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(oauthRoute.BaseURL, "/")+wire.Path(oauthRoute.Protocol), bytes.NewReader(body))
	if err != nil {
		return nil, errors.New("invalid OAuth endpoint")
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
	accountID := oauthAccountID(connection)
	setRequestCredential(ctx, route.Tier, accountID, "oauth", false, proxy)
	setRequestShape(ctx, false, nil)
	if meta := telemetry.MetaFromContext(ctx); meta != nil {
		meta.Attempts = attempt
		meta.Tier = string(route.Tier)
	}
	started := time.Now()
	resp, err := doInferenceAttempt(&client, req, firstEventTimeout, g.attemptTimeout)
	if resp != nil {
		sanitizeOAuthResponse(resp, connection.Credential)
		invalidate := g.onGoOAuthUnauthorized
		if route.Tier == config.TierZen {
			invalidate = g.onZenOAuthUnauthorized
		}
		if resp.StatusCode == http.StatusUnauthorized && invalidate != nil {
			invalidate(ctx, connection.Credential)
		}
	}
	if err != nil {
		err = &oauthError{cause: err, message: redactOAuthValue(err.Error(), connection.Credential)}
	}
	if ctx.Err() == nil && !isDiagnosticRequest(ctx) {
		g.syncProxyResult(ctx, proxy, upstreamStatus(resp), err)
	}
	g.recordUpstreamAttempt(ctx, route, ids, attempt, accountID, "oauth", false, proxy, resp, err, time.Since(started))
	if err != nil {
		return resp, asProxyUnavailable(proxy, err)
	}
	return resp, nil
}

type oauthError struct {
	cause   error
	message string
}

func (err *oauthError) Error() string { return err.message }
func (err *oauthError) Unwrap() error { return err.cause }

func redactOAuthValue(value string, credential oauth.Credential) string {
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

func sanitizeOAuthResponse(response *http.Response, credential oauth.Credential) {
	if response.Header == nil {
		response.Header = make(http.Header)
	}
	response.Request = nil
	response.Status = redactOAuthValue(response.Status, credential)
	if response.StatusCode/100 != 2 {
		var body []byte
		var readErr error
		if response.Body != nil {
			body, readErr = io.ReadAll(io.LimitReader(response.Body, 1<<20))
			response.Body.Close()
		}
		if readErr != nil || len(body) >= 1<<20 {
			body = []byte(`{"error":{"message":"OAuth upstream request was rejected"}}`)
			response.Header.Set("Content-Type", "application/json")
			response.Header.Del("Content-Encoding")
		} else {
			var value any
			if json.Unmarshal(body, &value) == nil {
				// Decode before scrubbing so a JSON escape cannot hide a token
				// that a rejected request echoed in its error message.
				body, _ = json.Marshal(redactOAuthJSON(value, credential))
			} else {
				body = []byte(redactOAuthValue(string(body), credential))
			}
		}
		response.Body = io.NopCloser(bytes.NewReader(body))
		response.ContentLength = int64(len(body))
		response.TransferEncoding = nil
		response.Header.Set("Content-Length", strconv.Itoa(len(body)))
	}
	for _, header := range []http.Header{response.Header, response.Trailer} {
		for name, values := range header {
			for index, value := range values {
				values[index] = redactOAuthValue(value, credential)
			}
			safeName := redactOAuthValue(name, credential)
			if safeName != name {
				delete(header, name)
				header[safeName] = append(header[safeName], values...)
			}
		}
	}
}

func redactOAuthJSON(value any, credential oauth.Credential) any {
	switch typed := value.(type) {
	case string:
		return redactOAuthValue(typed, credential)
	case []any:
		for index, item := range typed {
			typed[index] = redactOAuthJSON(item, credential)
		}
	case map[string]any:
		redacted := make(map[string]any, len(typed))
		for name, item := range typed {
			redacted[redactOAuthValue(name, credential)] = redactOAuthJSON(item, credential)
		}
		return redacted
	}
	return value
}

func (g *Gateway) doOAuthUpstream(ctx context.Context, route models.Route, bodies map[config.Tier][]byte, ids identity.RequestIDs, attemptOffset int) (response *http.Response, resultErr error, resultAttempts int) {
	body := bodies[route.Tier]
	if len(body) == 0 {
		return nil, errors.New("no prepared OAuth request body"), 0
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
		resp, err := g.oauthAttempt(ctx, route, body, ids, attemptOffset+attempts+1, attempts, time.Duration(g.cfg.Performance.FirstEventTimeoutSeconds)*time.Second)
		resultAttempts = attempts + 1
		lastResponse = replaceRetryResponse(lastResponse, resp)
		lastErr = err
		if ctx.Err() != nil {
			break
		}
		var authorizationError *oauth.OAuthError
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
		lastErr = errors.New("OAuth request could not be completed")
	}
	return nil, lastErr, resultAttempts
}

func (g *Gateway) routeDiagnostic(model string, requested wire.Protocol) models.RouteDiagnostic {
	diagnostic := g.catalog.Diagnostic(model, requested, g.zenNodes.Len() > 0, g.goNodes.Len() > 0, g.cfg.Anonymous)
	oauthModel := false
	for _, tier := range []config.Tier{config.TierGo, config.TierZen} {
		if connection := g.tierOAuth(tier); connection != nil {
			_, included := connection.Routes[model]
			oauthModel = oauthModel || included
			if tier == config.TierGo {
				diagnostic.AvailableGo = included
			} else {
				diagnostic.AvailableZen = included
			}
		}
	}
	route, err := g.routeForModel(model, "")
	if err != nil {
		diagnostic.RouteError = err.Error()
		if oauthModel {
			diagnostic.Tier, diagnostic.Anonymous, diagnostic.KeyTiers = "", false, nil
			var authorizationError *oauth.OAuthError
			if errors.As(err, &authorizationError) {
				tiers := []config.Tier{config.TierGo, config.TierZen}
				if g.cfg.Prefer == config.TierZen {
					tiers = []config.Tier{config.TierZen, config.TierGo}
				}
				for _, tier := range tiers {
					connection := g.tierOAuth(tier)
					if connection == nil {
						continue
					}
					if oauthRoute, included := connection.Routes[model]; included {
						diagnostic.ProtocolSource = string(tier) + "_oauth"
						diagnostic.Channel, diagnostic.KeyID = "oauth", oauthAccountID(connection)
						diagnostic.NativeProtocol = oauthRoute.Protocol
						diagnostic.NativeProtocols = map[config.Tier]wire.Protocol{tier: oauthRoute.Protocol}
						break
					}
				}
			}
		}
		return diagnostic
	}
	diagnostic.RouteError = ""
	diagnostic.Tier, diagnostic.Anonymous = route.Tier, route.Anonymous
	diagnostic.KeyTiers = route.KeyTiers
	diagnostic.NativeProtocol, diagnostic.NativeProtocols = route.Protocol, route.Protocols
	if connection := g.tierOAuth(route.Tier); connection != nil && !route.Anonymous {
		diagnostic.ProtocolSource = string(route.Tier) + "_oauth"
		diagnostic.Channel = "oauth"
		diagnostic.KeyID = oauthAccountID(connection)
	}
	return diagnostic
}
