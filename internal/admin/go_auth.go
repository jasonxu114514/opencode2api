package admin

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"time"

	"opencode2api/internal/config"
	"opencode2api/internal/goauth"
	"opencode2api/internal/httpx"
)

type goAuthAttempt struct {
	ID            string
	Owner         string
	Status        string
	Device        goauth.Device
	Credential    *goauth.Credential
	Organizations []goauth.Organization
	Expires       time.Time
	NextPoll      time.Time
	Busy          bool
	Cancel        context.CancelFunc
	Error         string
}

type goAuthPendingView struct {
	ID                string                `json:"id"`
	Status            string                `json:"status"`
	VerificationURL   string                `json:"verification_url,omitempty"`
	UserCode          string                `json:"user_code,omitempty"`
	ExpiresAt         time.Time             `json:"expires_at"`
	IntervalSeconds   int                   `json:"interval_seconds"`
	RetryAfterSeconds int                   `json:"retry_after_seconds"`
	Organizations     []goauth.Organization `json:"organizations,omitempty"`
	Error             string                `json:"error,omitempty"`
}

type goAccountView struct {
	Email        string    `json:"email"`
	Organization string    `json:"organization"`
	ExpiresAt    time.Time `json:"expires_at"`
	Models       int       `json:"models"`
	NeedsLogin   bool      `json:"needs_login"`
	Error        string    `json:"error,omitempty"`
}

type goAuthView struct {
	Connected bool               `json:"connected"`
	Account   *goAccountView     `json:"account,omitempty"`
	Pending   *goAuthPendingView `json:"pending,omitempty"`
}

func goAuthOwner(r *http.Request) string {
	cookie, err := r.Cookie(adminCookieName)
	if err != nil || cookie.Value == "" {
		return ""
	}
	return tokenDigest(cookie.Value)
}

func (a *Server) goAuthSessionValid(owner string, cfg config.Config) bool {
	a.mu.Lock()
	session, ok := a.sessions[owner]
	a.mu.Unlock()
	return ok && time.Now().Before(session.Expires) && session.Username == cfg.WebUI.Username && session.AuthVersion == config.Fingerprint(cfg.WebUI.PasswordHash)
}

func (a *Server) cleanupGoAuthLocked() {
	now := time.Now()
	cfg := a.manager.Config()
	for owner, attempt := range a.goAuth {
		if !now.Before(attempt.Expires) || !a.goAuthSessionValid(owner, cfg) {
			if attempt.Cancel != nil {
				attempt.Cancel()
			}
			delete(a.goAuth, owner)
		}
	}
}

func (a *Server) cancelGoAuth(owner string) {
	a.goAuthMu.Lock()
	defer a.goAuthMu.Unlock()
	for candidate, attempt := range a.goAuth {
		if owner != "" && candidate != owner {
			continue
		}
		if attempt.Cancel != nil {
			attempt.Cancel()
		}
		delete(a.goAuth, candidate)
	}
}

func (a *Server) goAuthViewLocked(owner string) goAuthView {
	view := goAuthView{}
	connection := a.manager.Config().GoSubscription
	if connection != nil {
		view.Connected = true
		view.Account = &goAccountView{
			Email: connection.Credential.Email, Organization: connection.Credential.OrganizationName,
			ExpiresAt: connection.Credential.ExpiresAt, Models: len(connection.Routes), NeedsLogin: connection.NeedsLogin,
		}
		if connection.NeedsLogin {
			view.Account.Error = "Go 订阅授权已失效或组织发生变化，请重新登录。"
		}
	}
	if attempt := a.goAuth[owner]; attempt != nil {
		wait := max(0, int(time.Until(attempt.NextPoll).Seconds()+0.999))
		view.Pending = &goAuthPendingView{
			ID: attempt.ID, Status: attempt.Status, ExpiresAt: attempt.Expires,
			VerificationURL: attempt.Device.VerificationURI, UserCode: attempt.Device.UserCode,
			IntervalSeconds: max(1, int(attempt.Device.Interval.Seconds())), RetryAfterSeconds: wait,
			Organizations: append([]goauth.Organization(nil), attempt.Organizations...), Error: attempt.Error,
		}
	}
	return view
}

func (a *Server) handleGoAuthStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	a.goAuthMu.Lock()
	a.cleanupGoAuthLocked()
	view := a.goAuthViewLocked(goAuthOwner(r))
	a.goAuthMu.Unlock()
	httpx.WriteJSON(w, http.StatusOK, view)
}

func (a *Server) handleGoAuthStart(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var input struct{}
	if err := decodeAdminJSON(w, r, &input); err != nil {
		writeAdminError(w, http.StatusBadRequest, "invalid_request", "登录请求格式无效")
		return
	}
	owner := goAuthOwner(r)
	session, ok := sessionFromContext(r)
	if !ok || owner == "" {
		writeAdminError(w, http.StatusUnauthorized, "authentication_required", "login required")
		return
	}
	id, err := randomToken(24)
	if err != nil {
		writeAdminError(w, http.StatusInternalServerError, "internal_error", "无法创建订阅登录")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	a.goAuthMu.Lock()
	a.cleanupGoAuthLocked()
	if existing := a.goAuth[owner]; existing != nil && existing.Status != "failed" {
		view := a.goAuthViewLocked(owner)
		a.goAuthMu.Unlock()
		httpx.WriteJSON(w, http.StatusOK, view)
		return
	}
	if len(a.goAuth) >= 32 && a.goAuth[owner] == nil {
		a.goAuthMu.Unlock()
		writeAdminError(w, http.StatusTooManyRequests, "login_capacity", "待完成的订阅登录过多，请稍后重试")
		return
	}
	attempt := &goAuthAttempt{ID: id, Owner: owner, Status: "starting", Expires: minTime(session.Expires, time.Now().Add(15*time.Minute)), Busy: true, Cancel: cancel}
	if a.goAuth == nil {
		a.goAuth = make(map[string]*goAuthAttempt)
	}
	a.goAuth[owner] = attempt
	a.goAuthMu.Unlock()
	device, err := a.goAuthClient.Start(ctx)
	a.goAuthMu.Lock()
	if a.goAuth[owner] != attempt || !time.Now().Before(attempt.Expires) || !a.goAuthSessionValid(owner, a.manager.Config()) {
		a.goAuthMu.Unlock()
		writeAdminError(w, http.StatusConflict, "login_canceled", "订阅登录已取消或管理会话已失效")
		return
	}
	attempt.Busy, attempt.Cancel = false, nil
	if err != nil {
		attempt.Status, attempt.Error = "failed", safeGoAuthError(err)
	} else {
		attempt.Device, attempt.Status = device, "pending"
		attempt.Expires = minTime(attempt.Expires, device.ExpiresAt)
		attempt.NextPoll = time.Now().Add(device.Interval)
	}
	view := a.goAuthViewLocked(owner)
	a.goAuthMu.Unlock()
	httpx.WriteJSON(w, http.StatusOK, view)
}

func (a *Server) handleGoAuthPoll(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var input struct {
		ID             string `json:"id"`
		OrganizationID string `json:"organization_id,omitempty"`
	}
	if err := decodeAdminJSON(w, r, &input); err != nil || input.ID == "" {
		writeAdminError(w, http.StatusBadRequest, "invalid_request", "登录请求格式无效")
		return
	}
	owner := goAuthOwner(r)
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	a.goAuthMu.Lock()
	a.cleanupGoAuthLocked()
	attempt := a.goAuth[owner]
	if attempt == nil || attempt.ID != input.ID {
		a.goAuthMu.Unlock()
		writeAdminError(w, http.StatusNotFound, "login_not_found", "订阅登录已过期或不存在，请重新开始")
		return
	}
	if attempt.Busy || attempt.Status == "failed" || time.Now().Before(attempt.NextPoll) {
		view := a.goAuthViewLocked(owner)
		a.goAuthMu.Unlock()
		httpx.WriteJSON(w, http.StatusOK, view)
		return
	}
	if input.OrganizationID != "" && !slices.ContainsFunc(attempt.Organizations, func(org goauth.Organization) bool { return org.ID == input.OrganizationID }) {
		a.goAuthMu.Unlock()
		writeAdminError(w, http.StatusBadRequest, "invalid_organization", "只能选择本次授权允许的组织")
		return
	}
	if attempt.Status == "choosing_org" && input.OrganizationID == "" {
		view := a.goAuthViewLocked(owner)
		a.goAuthMu.Unlock()
		httpx.WriteJSON(w, http.StatusOK, view)
		return
	}
	var credential *goauth.Credential
	if attempt.Credential != nil {
		copied := *attempt.Credential
		credential = &copied
	}
	organizations := append([]goauth.Organization(nil), attempt.Organizations...)
	deviceCode := attempt.Device.DeviceCode
	attempt.Busy, attempt.Cancel, attempt.Status = true, cancel, "authorizing"
	a.goAuthMu.Unlock()
	var err error
	if credential == nil {
		var token goauth.Token
		token, err = a.goAuthClient.Poll(ctx, deviceCode)
		if err == nil {
			var user goauth.User
			user, organizations, err = a.goAuthClient.Identify(ctx, token)
			if err == nil {
				credential = &goauth.Credential{
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
			a.goAuthMu.Lock()
			if a.goAuth[owner] == attempt && time.Now().Before(attempt.Expires) && a.goAuthSessionValid(owner, a.manager.Config()) {
				attempt.Credential, attempt.Organizations = credential, organizations
				attempt.Expires = minTime(attempt.Expires, credential.ExpiresAt)
				attempt.Status, attempt.Busy, attempt.Cancel = "choosing_org", false, nil
				attempt.NextPoll = time.Time{}
				view := a.goAuthViewLocked(owner)
				a.goAuthMu.Unlock()
				httpx.WriteJSON(w, http.StatusOK, view)
			} else {
				a.goAuthMu.Unlock()
				writeAdminError(w, http.StatusConflict, "login_canceled", "订阅登录已取消")
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
	var routes map[string]goauth.Route
	if err == nil {
		routes, err = a.goAuthClient.Discover(ctx, *credential)
	}
	a.goAuthMu.Lock()
	if a.goAuth[owner] != attempt || !time.Now().Before(attempt.Expires) || !a.goAuthSessionValid(owner, a.manager.Config()) {
		a.goAuthMu.Unlock()
		writeAdminError(w, http.StatusConflict, "login_canceled", "订阅登录已取消或管理会话已失效")
		return
	}
	attempt.Busy, attempt.Cancel = false, nil
	if err != nil {
		var oauthError *goauth.OAuthError
		if errors.As(err, &oauthError) && (oauthError.Code == "authorization_pending" || oauthError.Code == "slow_down") {
			if oauthError.Code == "slow_down" {
				attempt.Device.Interval = min(attempt.Device.Interval+5*time.Second, 5*time.Minute)
			}
			attempt.Status = "pending"
			attempt.NextPoll = time.Now().Add(attempt.Device.Interval)
		} else {
			attempt.Status, attempt.Error, attempt.Credential = "failed", safeGoAuthError(err), nil
			attempt.Device.DeviceCode = ""
		}
		view := a.goAuthViewLocked(owner)
		a.goAuthMu.Unlock()
		httpx.WriteJSON(w, http.StatusOK, view)
		return
	}
	connection := &goauth.Connection{Credential: *credential, Routes: routes}
	_, err = a.manager.Update(func(cfg *config.Config) error {
		if ctx.Err() != nil || !time.Now().Before(attempt.Expires) || !time.Now().Before(credential.ExpiresAt) || !a.goAuthSessionValid(owner, *cfg) {
			return errors.New("subscription login is no longer active")
		}
		if cfg.GoSubscription != nil {
			connection.Proxy = cfg.GoSubscription.Proxy
		}
		cfg.GoSubscription = connection
		return nil
	})
	if err != nil {
		attempt.Status, attempt.Error, attempt.Credential = "failed", "无法安全保存订阅登录，请检查配置后重新登录。", nil
		attempt.Device.DeviceCode = ""
	} else {
		delete(a.goAuth, owner)
		a.logger.Info("Go subscription connected", "component", "auth", "event", "go_subscription_connected")
	}
	view := a.goAuthViewLocked(owner)
	a.goAuthMu.Unlock()
	httpx.WriteJSON(w, http.StatusOK, view)
}

func (a *Server) handleGoAuthCancel(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	a.cancelGoAuth(goAuthOwner(r))
	a.handleGoAuthStatus(w, r)
}

func (a *Server) handleGoDisconnect(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	owner := goAuthOwner(r)
	a.goAuthMu.Lock()
	_, err := a.manager.Update(func(cfg *config.Config) error {
		if !a.goAuthSessionValid(owner, *cfg) {
			return errors.New("management session is no longer active")
		}
		cfg.GoSubscription = nil
		return nil
	})
	if err != nil {
		a.goAuthMu.Unlock()
		writeAdminError(w, http.StatusBadRequest, "disconnect_failed", "无法断开订阅；请确保保留至少一个上游 Key 或启用匿名通道。")
		return
	}
	for candidate, attempt := range a.goAuth {
		if attempt.Cancel != nil {
			attempt.Cancel()
		}
		delete(a.goAuth, candidate)
	}
	a.logger.Info("Go subscription disconnected locally", "component", "auth", "event", "go_subscription_disconnected")
	view := a.goAuthViewLocked(owner)
	a.goAuthMu.Unlock()
	httpx.WriteJSON(w, http.StatusOK, view)
}

func safeGoAuthError(err error) string {
	var oauthError *goauth.OAuthError
	if errors.As(err, &oauthError) {
		return oauthError.Error()
	}
	return "订阅授权未完成或请求已取消，请重新开始。"
}

func minTime(first, second time.Time) time.Time {
	if first.Before(second) {
		return first
	}
	return second
}
