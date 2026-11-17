package admin

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"time"

	"opencode2api/internal/config"
	"opencode2api/internal/httpx"
	"opencode2api/internal/oauth"
)

type oauthKey struct {
	Owner string
	Plan  oauth.Plan
}

type oauthAttempt struct {
	ID            string
	Status        string
	Device        oauth.Device
	Credential    *oauth.Credential
	Organizations []oauth.Organization
	Expires       time.Time
	NextPoll      time.Time
	Busy          bool
	Cancel        context.CancelFunc
	Error         string
}

type oauthPendingView struct {
	ID                string               `json:"id"`
	Status            string               `json:"status"`
	VerificationURL   string               `json:"verification_url,omitempty"`
	UserCode          string               `json:"user_code,omitempty"`
	ExpiresAt         time.Time            `json:"expires_at"`
	IntervalSeconds   int                  `json:"interval_seconds"`
	RetryAfterSeconds int                  `json:"retry_after_seconds"`
	Organizations     []oauth.Organization `json:"organizations,omitempty"`
	Error             string               `json:"error,omitempty"`
}

type oauthAccountView struct {
	Email        string    `json:"email"`
	Organization string    `json:"organization"`
	ExpiresAt    time.Time `json:"expires_at"`
	Models       int       `json:"models"`
	NeedsLogin   bool      `json:"needs_login"`
	Error        string    `json:"error,omitempty"`
}

type oauthView struct {
	Connected bool              `json:"connected"`
	Account   *oauthAccountView `json:"account,omitempty"`
	Pending   *oauthPendingView `json:"pending,omitempty"`
}

func (a *Server) cleanupOAuthLocked() {
	now := time.Now()
	cfg := a.manager.Config()
	for key, attempt := range a.oauth {
		if !now.Before(attempt.Expires) || !a.sessionValid(key.Owner, cfg) {
			if attempt.Cancel != nil {
				attempt.Cancel()
			}
			delete(a.oauth, key)
		}
	}
}

func (a *Server) cancelOAuth(owner string) {
	a.oauthMu.Lock()
	defer a.oauthMu.Unlock()
	a.cancelOAuthLocked(owner, "")
}

func (a *Server) cancelOAuthLocked(owner string, plan oauth.Plan) {
	for key, attempt := range a.oauth {
		if (owner != "" && key.Owner != owner) || (plan != "" && key.Plan != plan) {
			continue
		}
		if attempt.Cancel != nil {
			attempt.Cancel()
		}
		delete(a.oauth, key)
	}
}

func (a *Server) oauthViewLocked(owner string, plan oauth.Plan) oauthView {
	view := oauthView{}
	cfg := a.manager.Config()
	connection := cfg.GoOAuth
	if plan == oauth.PlanZen {
		connection = cfg.ZenOAuth
	}
	if connection != nil {
		view.Connected = true
		view.Account = &oauthAccountView{
			Email: connection.Credential.Email, Organization: connection.Credential.OrganizationName,
			ExpiresAt: connection.Credential.ExpiresAt, Models: len(connection.Routes), NeedsLogin: connection.NeedsLogin,
		}
		if connection.NeedsLogin {
			view.Account.Error = "OpenCode 授权已失效或组织发生变化，请重新登录。"
		}
	}
	if attempt := a.oauth[oauthKey{Owner: owner, Plan: plan}]; attempt != nil {
		wait := max(0, int(time.Until(attempt.NextPoll).Seconds()+0.999))
		view.Pending = &oauthPendingView{
			ID: attempt.ID, Status: attempt.Status, ExpiresAt: attempt.Expires,
			VerificationURL: attempt.Device.VerificationURI, UserCode: attempt.Device.UserCode,
			IntervalSeconds: max(1, int(attempt.Device.Interval.Seconds())), RetryAfterSeconds: wait,
			Organizations: append([]oauth.Organization(nil), attempt.Organizations...), Error: attempt.Error,
		}
	}
	return view
}

func (a *Server) handleGoOAuthStatus(w http.ResponseWriter, r *http.Request) {
	a.handleOAuthStatus(w, r, oauth.PlanGo)
}

func (a *Server) handleZenOAuthStatus(w http.ResponseWriter, r *http.Request) {
	a.handleOAuthStatus(w, r, oauth.PlanZen)
}

func (a *Server) handleOAuthStatus(w http.ResponseWriter, r *http.Request, plan oauth.Plan) {
	w.Header().Set("Cache-Control", "no-store")
	a.oauthMu.Lock()
	a.cleanupOAuthLocked()
	view := a.oauthViewLocked(sessionID(r), plan)
	a.oauthMu.Unlock()
	httpx.WriteJSON(w, http.StatusOK, view)
}

func (a *Server) handleGoOAuthStart(w http.ResponseWriter, r *http.Request) {
	a.handleOAuthStart(w, r, oauth.PlanGo)
}

func (a *Server) handleZenOAuthStart(w http.ResponseWriter, r *http.Request) {
	a.handleOAuthStart(w, r, oauth.PlanZen)
}

func (a *Server) handleOAuthStart(w http.ResponseWriter, r *http.Request, plan oauth.Plan) {
	w.Header().Set("Cache-Control", "no-store")
	if plan == oauth.PlanZen {
		var input struct {
			AcknowledgePAYG bool `json:"acknowledge_payg"`
		}
		if err := decodeAdminJSON(w, r, &input); err != nil {
			writeAdminError(w, http.StatusBadRequest, "invalid_request", "登录请求格式无效")
			return
		}
		if !input.AcknowledgePAYG {
			writeAdminError(w, http.StatusBadRequest, "payg_acknowledgement_required", "Zen 按 credits 用量计费，与 Go 订阅额度独立；请明确确认 acknowledge_payg 为 true 后再连接。")
			return
		}
	} else {
		var input struct{}
		if err := decodeAdminJSON(w, r, &input); err != nil {
			writeAdminError(w, http.StatusBadRequest, "invalid_request", "登录请求格式无效")
			return
		}
	}
	owner := sessionID(r)
	key := oauthKey{Owner: owner, Plan: plan}
	session, ok := sessionFromContext(r)
	if !ok || owner == "" {
		writeAdminError(w, http.StatusUnauthorized, "authentication_required", "login required")
		return
	}
	id, err := randomToken(24)
	if err != nil {
		writeAdminError(w, http.StatusInternalServerError, "internal_error", "无法创建 OAuth 登录")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	a.oauthMu.Lock()
	a.cleanupOAuthLocked()
	if existing := a.oauth[key]; existing != nil && existing.Status != "failed" {
		view := a.oauthViewLocked(owner, plan)
		a.oauthMu.Unlock()
		httpx.WriteJSON(w, http.StatusOK, view)
		return
	}
	if len(a.oauth) >= 32 && a.oauth[key] == nil {
		a.oauthMu.Unlock()
		writeAdminError(w, http.StatusTooManyRequests, "login_capacity", "待完成的 OAuth 登录过多，请稍后重试")
		return
	}
	attempt := &oauthAttempt{ID: id, Status: "starting", Expires: minTime(session.Expires, time.Now().Add(15*time.Minute)), Busy: true, Cancel: cancel}
	if a.oauth == nil {
		a.oauth = make(map[oauthKey]*oauthAttempt)
	}
	a.oauth[key] = attempt
	a.oauthMu.Unlock()
	device, err := a.oauthClient.Start(ctx)
	a.oauthMu.Lock()
	if a.oauth[key] != attempt || !time.Now().Before(attempt.Expires) || !a.sessionValid(owner, a.manager.Config()) {
		a.oauthMu.Unlock()
		writeAdminError(w, http.StatusConflict, "login_canceled", "OAuth 登录已取消或管理会话已失效")
		return
	}
	attempt.Busy, attempt.Cancel = false, nil
	if err != nil {
		attempt.Status, attempt.Error = "failed", safeOAuthError(err)
	} else {
		attempt.Device, attempt.Status = device, "pending"
		attempt.Expires = minTime(attempt.Expires, device.ExpiresAt)
		attempt.NextPoll = time.Now().Add(device.Interval)
	}
	view := a.oauthViewLocked(owner, plan)
	a.oauthMu.Unlock()
	httpx.WriteJSON(w, http.StatusOK, view)
}

func (a *Server) handleGoOAuthPoll(w http.ResponseWriter, r *http.Request) {
	a.handleOAuthPoll(w, r, oauth.PlanGo)
}

func (a *Server) handleZenOAuthPoll(w http.ResponseWriter, r *http.Request) {
	a.handleOAuthPoll(w, r, oauth.PlanZen)
}

func (a *Server) handleOAuthPoll(w http.ResponseWriter, r *http.Request, plan oauth.Plan) {
	w.Header().Set("Cache-Control", "no-store")
	var input struct {
		ID             string `json:"id"`
		OrganizationID string `json:"organization_id,omitempty"`
	}
	if err := decodeAdminJSON(w, r, &input); err != nil || input.ID == "" {
		writeAdminError(w, http.StatusBadRequest, "invalid_request", "登录请求格式无效")
		return
	}
	owner := sessionID(r)
	key := oauthKey{Owner: owner, Plan: plan}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	a.oauthMu.Lock()
	a.cleanupOAuthLocked()
	attempt := a.oauth[key]
	if attempt == nil || attempt.ID != input.ID {
		a.oauthMu.Unlock()
		writeAdminError(w, http.StatusNotFound, "login_not_found", "OAuth 登录已过期或不存在，请重新开始")
		return
	}
	if attempt.Busy || attempt.Status == "failed" || time.Now().Before(attempt.NextPoll) {
		view := a.oauthViewLocked(owner, plan)
		a.oauthMu.Unlock()
		httpx.WriteJSON(w, http.StatusOK, view)
		return
	}
	if input.OrganizationID != "" && !slices.ContainsFunc(attempt.Organizations, func(org oauth.Organization) bool { return org.ID == input.OrganizationID }) {
		a.oauthMu.Unlock()
		writeAdminError(w, http.StatusBadRequest, "invalid_organization", "只能选择本次授权允许的组织")
		return
	}
	if attempt.Status == "choosing_org" && input.OrganizationID == "" {
		view := a.oauthViewLocked(owner, plan)
		a.oauthMu.Unlock()
		httpx.WriteJSON(w, http.StatusOK, view)
		return
	}
	var credential *oauth.Credential
	if attempt.Credential != nil {
		copied := *attempt.Credential
		credential = &copied
	}
	organizations := append([]oauth.Organization(nil), attempt.Organizations...)
	deviceCode := attempt.Device.DeviceCode
	attempt.Busy, attempt.Cancel, attempt.Status = true, cancel, "authorizing"
	a.oauthMu.Unlock()
	var err error
	if credential == nil {
		var token oauth.Token
		token, err = a.oauthClient.Poll(ctx, deviceCode)
		if err == nil {
			var user oauth.User
			user, organizations, err = a.oauthClient.Identify(ctx, token)
			if err == nil {
				credential = &oauth.Credential{
					AccessToken: token.AccessToken, RefreshToken: token.RefreshToken, ExpiresAt: token.ExpiresAt,
					AccountID: user.ID, Email: user.Email, OrganizationID: token.OrganizationID,
				}
			}
		}
	}
	if err == nil && credential.OrganizationID == "" {
		switch {
		case input.OrganizationID != "":
			credential.OrganizationID = input.OrganizationID
		case len(organizations) == 1:
			credential.OrganizationID = organizations[0].ID
		case len(organizations) > 1:
			a.oauthMu.Lock()
			if a.oauth[key] == attempt && time.Now().Before(attempt.Expires) && a.sessionValid(owner, a.manager.Config()) {
				attempt.Credential, attempt.Organizations = credential, organizations
				attempt.Expires = minTime(attempt.Expires, credential.ExpiresAt)
				attempt.Status, attempt.Busy, attempt.Cancel = "choosing_org", false, nil
				attempt.NextPoll = time.Time{}
				view := a.oauthViewLocked(owner, plan)
				a.oauthMu.Unlock()
				httpx.WriteJSON(w, http.StatusOK, view)
			} else {
				a.oauthMu.Unlock()
				writeAdminError(w, http.StatusConflict, "login_canceled", "OAuth 登录已取消")
			}
			return
		}
	}
	if credential != nil {
		for _, organization := range organizations {
			if organization.ID == credential.OrganizationID {
				credential.OrganizationName = organization.Name
				break
			}
		}
	}
	var routes map[string]oauth.Route
	if err == nil {
		routes, err = a.oauthClient.DiscoverForPlan(ctx, *credential, plan)
	}
	a.oauthMu.Lock()
	if a.oauth[key] != attempt || !time.Now().Before(attempt.Expires) || !a.sessionValid(owner, a.manager.Config()) {
		a.oauthMu.Unlock()
		writeAdminError(w, http.StatusConflict, "login_canceled", "OAuth 登录已取消或管理会话已失效")
		return
	}
	attempt.Busy, attempt.Cancel = false, nil
	if err != nil {
		var oauthError *oauth.OAuthError
		if errors.As(err, &oauthError) && (oauthError.Code == "authorization_pending" || oauthError.Code == "slow_down") {
			if oauthError.Code == "slow_down" {
				attempt.Device.Interval = min(attempt.Device.Interval+5*time.Second, 5*time.Minute)
			}
			attempt.Status = "pending"
			attempt.NextPoll = time.Now().Add(attempt.Device.Interval)
		} else {
			attempt.Status, attempt.Error, attempt.Credential = "failed", safeOAuthError(err), nil
			attempt.Device.DeviceCode = ""
		}
		view := a.oauthViewLocked(owner, plan)
		a.oauthMu.Unlock()
		httpx.WriteJSON(w, http.StatusOK, view)
		return
	}
	connection := &oauth.Connection{Credential: *credential, Routes: routes}
	_, err = a.manager.Update(func(cfg *config.Config) error {
		if ctx.Err() != nil || a.oauth[key] != attempt || !time.Now().Before(attempt.Expires) || !time.Now().Before(credential.ExpiresAt) || !a.sessionValid(owner, *cfg) {
			return errors.New("OAuth login is no longer active")
		}
		if plan == oauth.PlanZen {
			if cfg.ZenOAuth != nil {
				connection.Proxy = cfg.ZenOAuth.Proxy
			}
			cfg.ZenOAuth = connection
		} else {
			if cfg.GoOAuth != nil {
				connection.Proxy = cfg.GoOAuth.Proxy
			}
			cfg.GoOAuth = connection
		}
		return nil
	})
	if err != nil {
		attempt.Status, attempt.Error, attempt.Credential = "failed", "无法安全保存 OAuth 登录，请检查配置后重新登录。", nil
		attempt.Device.DeviceCode = ""
	} else {
		delete(a.oauth, key)
		a.logger.Info("OpenCode account connected", "component", "auth", "event", string(plan)+"_oauth_connected", "tier", plan)
	}
	view := a.oauthViewLocked(owner, plan)
	a.oauthMu.Unlock()
	httpx.WriteJSON(w, http.StatusOK, view)
}

func (a *Server) handleGoOAuthCancel(w http.ResponseWriter, r *http.Request) {
	a.handleOAuthCancel(w, r, oauth.PlanGo)
}

func (a *Server) handleZenOAuthCancel(w http.ResponseWriter, r *http.Request) {
	a.handleOAuthCancel(w, r, oauth.PlanZen)
}

func (a *Server) handleOAuthCancel(w http.ResponseWriter, r *http.Request, plan oauth.Plan) {
	w.Header().Set("Cache-Control", "no-store")
	owner := sessionID(r)
	a.oauthMu.Lock()
	a.cancelOAuthLocked(owner, plan)
	a.cleanupOAuthLocked()
	view := a.oauthViewLocked(owner, plan)
	a.oauthMu.Unlock()
	httpx.WriteJSON(w, http.StatusOK, view)
}

func (a *Server) handleGoOAuthDisconnect(w http.ResponseWriter, r *http.Request) {
	a.handleOAuthDisconnect(w, r, oauth.PlanGo)
}

func (a *Server) handleZenOAuthDisconnect(w http.ResponseWriter, r *http.Request) {
	a.handleOAuthDisconnect(w, r, oauth.PlanZen)
}

func (a *Server) handleOAuthDisconnect(w http.ResponseWriter, r *http.Request, plan oauth.Plan) {
	w.Header().Set("Cache-Control", "no-store")
	owner := sessionID(r)
	a.oauthMu.Lock()
	_, err := a.manager.Update(func(cfg *config.Config) error {
		if !a.sessionValid(owner, *cfg) {
			return errors.New("management session is no longer active")
		}
		if plan == oauth.PlanZen {
			cfg.ZenOAuth = nil
		} else {
			cfg.GoOAuth = nil
		}
		return nil
	})
	if err != nil {
		a.oauthMu.Unlock()
		writeAdminError(w, http.StatusBadRequest, "disconnect_failed", "无法断开登录；请确保保留至少一个上游 Key、另一个 OpenCode 连接或已启用的匿名通道。")
		return
	}
	a.cancelOAuthLocked("", plan)
	a.logger.Info("OpenCode account disconnected locally", "component", "auth", "event", string(plan)+"_oauth_disconnected", "tier", plan)
	view := a.oauthViewLocked(owner, plan)
	a.oauthMu.Unlock()
	httpx.WriteJSON(w, http.StatusOK, view)
}

func safeOAuthError(err error) string {
	var oauthError *oauth.OAuthError
	if errors.As(err, &oauthError) {
		return oauthError.Error()
	}
	return "OpenCode 授权未完成或请求已取消，请重新开始。"
}

func minTime(first, second time.Time) time.Time {
	if first.Before(second) {
		return first
	}
	return second
}
