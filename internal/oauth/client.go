package oauth

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"opencode2api/internal/httpx"
	"opencode2api/internal/protocol"
)

const (
	ConsoleURL       = "https://opencode.ai/console"
	clientID         = "opencode-cli"
	requestTimeout   = 15 * time.Second
	responseLimit    = 2 << 20
	maximumTokenSize = 16 << 10
)

type Plan string

const (
	PlanGo  Plan = "go"
	PlanZen Plan = "zen"
)

type Credential struct {
	AccessToken      string    `json:"access_token"`
	RefreshToken     string    `json:"refresh_token"`
	ExpiresAt        time.Time `json:"expires_at"`
	AccountID        string    `json:"account_id"`
	Email            string    `json:"email"`
	OrganizationID   string    `json:"organization_id"`
	OrganizationName string    `json:"organization_name"`
}

type Device struct {
	DeviceCode      string        `json:"-"`
	UserCode        string        `json:"user_code"`
	VerificationURI string        `json:"verification_uri"`
	ExpiresAt       time.Time     `json:"expires_at"`
	Interval        time.Duration `json:"interval"`
}

type Token struct {
	AccessToken    string    `json:"access_token"`
	RefreshToken   string    `json:"refresh_token"`
	ExpiresAt      time.Time `json:"expires_at"`
	OrganizationID string    `json:"organization_id"`
}

type User struct {
	ID    string `json:"id"`
	Email string `json:"email"`
}

type Organization struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type Route struct {
	ModelID  string            `json:"model_id"`
	Protocol protocol.Protocol `json:"protocol"`
	BaseURL  string            `json:"base_url"`
}

type Connection struct {
	Credential Credential       `json:"credential"`
	Routes     map[string]Route `json:"routes"`
	Proxy      string           `json:"proxy,omitempty"`
	NeedsLogin bool             `json:"needs_login,omitempty"`
}

type OAuthError struct {
	Code string
}

func (err *OAuthError) Error() string {
	switch err.Code {
	case "authorization_pending":
		return "OpenCode authorization is awaiting approval"
	case "slow_down":
		return "OpenCode authorization polling must slow down"
	case "invalid_grant", "expired_token":
		return "OpenCode authorization expired or was revoked; sign in again"
	case "access_denied":
		return "OpenCode authorization was denied"
	case "unauthorized":
		return "OpenCode sign-in is no longer authorized; sign in again"
	case "sso_required":
		return "OpenCode workspace requires a new SSO sign-in"
	case "no_go_oauth":
		return "OpenCode did not provide a supported Go OAuth route"
	case "no_zen_oauth":
		return "OpenCode did not provide a supported Zen OAuth pay-as-you-go route"
	case "invalid_plan":
		return "OpenCode authorization plan is invalid"
	case "rate_limited":
		return "OpenCode authorization service is rate limited; try again later"
	case "invalid_credential":
		return "OpenCode OAuth credential is invalid"
	case "invalid_response":
		return "OpenCode authorization service returned an invalid response"
	case "request_timeout":
		return "OpenCode authorization request timed out"
	case "connection_failed":
		return "OpenCode authorization service could not be reached"
	default:
		return "OpenCode authorization service rejected the request"
	}
}

type Client struct {
	http *http.Client
}

func NewClient(client *http.Client) *Client {
	httpClient := http.Client{}
	if client != nil {
		httpClient = *client
	}
	if httpClient.Timeout <= 0 || httpClient.Timeout > requestTimeout {
		httpClient.Timeout = requestTimeout
	}
	// Credentials and device codes must never follow a redirect to another
	// origin, including redirects supplied by the official service.
	httpClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{http: &httpClient}
}

func (credential Credential) Validate() error {
	if !validOpaque(credential.AccessToken, maximumTokenSize) || !validOpaque(credential.RefreshToken, maximumTokenSize) || credential.ExpiresAt.IsZero() {
		return &OAuthError{Code: "invalid_credential"}
	}
	if credential.OrganizationID != "" && !validOpaque(credential.OrganizationID, 1024) {
		return &OAuthError{Code: "invalid_credential"}
	}
	if !validText(credential.AccountID, 1024) || !validText(credential.Email, 1024) || !validText(credential.OrganizationName, 4096) {
		return &OAuthError{Code: "invalid_credential"}
	}
	return nil
}

func (connection Connection) Validate() error {
	return connection.ValidateForPlan(PlanGo)
}

func (connection Connection) ValidateForPlan(plan Plan) error {
	if !validPlan(plan) {
		return &OAuthError{Code: "invalid_plan"}
	}
	if err := connection.Credential.Validate(); err != nil {
		return err
	}
	if !validText(connection.Proxy, 8192) {
		return &OAuthError{Code: "invalid_credential"}
	}
	if len(connection.Routes) == 0 {
		return noRoutesError(plan)
	}
	for model, route := range connection.Routes {
		base, ok := normalizeBaseForPlan(route.BaseURL, route.Protocol, plan)
		if !validOpaque(model, 1024) || !validOpaque(route.ModelID, 1024) || !ok || base != route.BaseURL {
			return &OAuthError{Code: "invalid_credential"}
		}
	}
	return nil
}

func (client *Client) Start(ctx context.Context) (Device, error) {
	body, err := client.request(ctx, http.MethodPost, "/auth/device/code", map[string]any{
		"client_id": clientID, "supports_org_scope": true,
	}, nil)
	if err != nil {
		return Device{}, err
	}
	var response struct {
		DeviceCode      string `json:"device_code"`
		UserCode        string `json:"user_code"`
		VerificationURI string `json:"verification_uri_complete"`
		ExpiresIn       int64  `json:"expires_in"`
		Interval        int64  `json:"interval"`
	}
	if err := json.Unmarshal(body, &response); err != nil || !validOpaque(response.DeviceCode, maximumTokenSize) || !validOpaque(response.UserCode, 256) || !validText(response.VerificationURI, 8192) || response.ExpiresIn < 1 || response.ExpiresIn > 86400 || response.Interval < 0 || response.Interval > 300 {
		return Device{}, &OAuthError{Code: "invalid_response"}
	}
	base, _ := url.Parse(ConsoleURL + "/")
	verification, err := url.Parse(response.VerificationURI)
	if err != nil || response.VerificationURI == "" {
		return Device{}, &OAuthError{Code: "invalid_response"}
	}
	verification = base.ResolveReference(verification)
	if !officialURL(verification) {
		return Device{}, &OAuthError{Code: "invalid_response"}
	}
	interval := response.Interval
	if interval == 0 {
		interval = 5
	}
	return Device{
		DeviceCode:      response.DeviceCode,
		UserCode:        response.UserCode,
		VerificationURI: verification.String(),
		ExpiresAt:       time.Now().UTC().Add(time.Duration(response.ExpiresIn) * time.Second),
		Interval:        time.Duration(interval) * time.Second,
	}, nil
}

func (client *Client) Poll(ctx context.Context, deviceCode string) (Token, error) {
	if !validOpaque(deviceCode, maximumTokenSize) {
		return Token{}, &OAuthError{Code: "invalid_credential"}
	}
	return client.exchange(ctx, map[string]string{
		"grant_type":  "urn:ietf:params:oauth:grant-type:device_code",
		"device_code": deviceCode,
		"client_id":   clientID,
	})
}

func (client *Client) Identify(ctx context.Context, token Token) (User, []Organization, error) {
	if !validOpaque(token.AccessToken, maximumTokenSize) {
		return User{}, nil, &OAuthError{Code: "invalid_credential"}
	}
	credential := Credential{AccessToken: token.AccessToken}
	body, err := client.request(ctx, http.MethodGet, "/api/user", nil, &credential)
	if err != nil {
		return User{}, nil, err
	}
	var user User
	if err := json.Unmarshal(body, &user); err != nil || user.ID == "" || !validText(user.ID, 1024) || !validText(user.Email, 1024) {
		return User{}, nil, &OAuthError{Code: "invalid_response"}
	}
	body, err = client.request(ctx, http.MethodGet, "/api/orgs", nil, &credential)
	if err != nil {
		return User{}, nil, err
	}
	var organizations []Organization
	if err := json.Unmarshal(body, &organizations); err != nil || organizations == nil || len(organizations) > 4096 {
		return User{}, nil, &OAuthError{Code: "invalid_response"}
	}
	seen := make(map[string]bool, len(organizations))
	for _, organization := range organizations {
		if !validOpaque(organization.ID, 1024) || !validText(organization.Name, 4096) || seen[organization.ID] {
			return User{}, nil, &OAuthError{Code: "invalid_response"}
		}
		seen[organization.ID] = true
	}
	if token.OrganizationID != "" && !seen[token.OrganizationID] {
		return User{}, nil, &OAuthError{Code: "access_denied"}
	}
	return user, organizations, nil
}

func (client *Client) Refresh(ctx context.Context, credential Credential) (Credential, error) {
	if err := credential.Validate(); err != nil {
		return Credential{}, err
	}
	token, err := client.exchange(ctx, map[string]string{
		"grant_type":    "refresh_token",
		"refresh_token": credential.RefreshToken,
		"client_id":     clientID,
	})
	if err != nil {
		return Credential{}, err
	}
	credential.AccessToken = token.AccessToken
	credential.RefreshToken = token.RefreshToken
	credential.ExpiresAt = token.ExpiresAt
	if token.OrganizationID != "" {
		if credential.OrganizationID != token.OrganizationID {
			credential.OrganizationName = token.OrganizationID
		}
		credential.OrganizationID = token.OrganizationID
	}
	return credential, nil
}

func (client *Client) exchange(ctx context.Context, payload map[string]string) (Token, error) {
	body, err := client.request(ctx, http.MethodPost, "/auth/device/token", payload, nil)
	if err != nil {
		return Token{}, err
	}
	var response struct {
		AccessToken    string `json:"access_token"`
		RefreshToken   string `json:"refresh_token"`
		ExpiresIn      int64  `json:"expires_in"`
		OrganizationID string `json:"org_id"`
		Error          string `json:"error"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return Token{}, &OAuthError{Code: "invalid_response"}
	}
	if response.Error != "" {
		return Token{}, responseError(http.StatusBadRequest, body)
	}
	if !validOpaque(response.AccessToken, maximumTokenSize) || !validOpaque(response.RefreshToken, maximumTokenSize) || response.ExpiresIn < 1 || response.ExpiresIn > 30*86400 || response.OrganizationID != "" && !validOpaque(response.OrganizationID, 1024) {
		return Token{}, &OAuthError{Code: "invalid_response"}
	}
	return Token{
		AccessToken:    response.AccessToken,
		RefreshToken:   response.RefreshToken,
		ExpiresAt:      time.Now().UTC().Add(time.Duration(response.ExpiresIn) * time.Second),
		OrganizationID: response.OrganizationID,
	}, nil
}

func (client *Client) request(ctx context.Context, method, path string, payload any, credential *Credential) ([]byte, error) {
	requestCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	var input io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return nil, &OAuthError{Code: "invalid_credential"}
		}
		input = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(requestCtx, method, ConsoleURL+path, input)
	if err != nil {
		return nil, &OAuthError{Code: "connection_failed"}
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", httpx.V2UserAgent())
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if credential != nil {
		request.Header.Set("Authorization", "Bearer "+credential.AccessToken)
		if credential.OrganizationID != "" {
			request.Header.Set("x-org-id", credential.OrganizationID)
		}
	}
	requestClient := client
	if requestClient == nil || requestClient.http == nil {
		requestClient = NewClient(nil)
	}
	response, err := requestClient.http.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if requestCtx.Err() != nil {
			return nil, &OAuthError{Code: "request_timeout"}
		}
		return nil, &OAuthError{Code: "connection_failed"}
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, responseLimit+1))
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if requestCtx.Err() != nil {
			return nil, &OAuthError{Code: "request_timeout"}
		}
		return nil, &OAuthError{Code: "invalid_response"}
	}
	if len(body) > responseLimit {
		return nil, &OAuthError{Code: "invalid_response"}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, responseError(response.StatusCode, body)
	}
	return body, nil
}

func responseError(status int, body []byte) error {
	var response struct {
		Error string `json:"error"`
		Tag   string `json:"_tag"`
	}
	_ = json.Unmarshal(body, &response)
	if response.Tag == "SsoRequired" {
		return &OAuthError{Code: "sso_required"}
	}
	if response.Tag == "Unauthorized" {
		return &OAuthError{Code: "unauthorized"}
	}
	switch response.Error {
	case "authorization_pending", "slow_down", "invalid_grant", "expired_token", "access_denied":
		return &OAuthError{Code: response.Error}
	}
	switch status {
	case http.StatusUnauthorized:
		return &OAuthError{Code: "unauthorized"}
	case http.StatusForbidden:
		return &OAuthError{Code: "access_denied"}
	case http.StatusTooManyRequests:
		return &OAuthError{Code: "rate_limited"}
	default:
		return &OAuthError{Code: "upstream_error"}
	}
}

func validOpaque(value string, limit int) bool {
	if value == "" || len(value) > limit {
		return false
	}
	for _, character := range value {
		if character <= ' ' || character > '~' {
			return false
		}
	}
	return true
}

func validText(value string, limit int) bool {
	if len(value) > limit || !utf8.ValidString(value) {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) || character == '\u2028' || character == '\u2029' {
			return false
		}
	}
	return true
}

func officialURL(parsed *url.URL) bool {
	return parsed != nil && parsed.Scheme == "https" && strings.EqualFold(parsed.Hostname(), "opencode.ai") && (parsed.Port() == "" || parsed.Port() == "443") && parsed.User == nil && parsed.Opaque == ""
}
