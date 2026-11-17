package gateway

import (
	"context"
	"errors"
	"time"

	"opencode2api/internal/config"
	"opencode2api/internal/oauth"
)

type oauthRefresh struct {
	credential oauth.Credential
	done       chan struct{}
	err        error
	retryAt    time.Time
}

func (m *RuntimeController) ResolveOAuth(ctx context.Context, tiers ...config.Tier) (*oauth.Connection, error) {
	tier := config.TierGo
	if len(tiers) > 0 {
		tier = tiers[0]
	}
	if tier != config.TierGo && tier != config.TierZen {
		return nil, errors.New("invalid Console plan")
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		m.oauthMu.Lock()
		connection := m.Config().GoOAuth
		refreshSlot := &m.goRefresh
		if tier == config.TierZen {
			connection = m.Config().ZenOAuth
			refreshSlot = &m.zenRefresh
		}
		if connection == nil {
			m.oauthMu.Unlock()
			return nil, errors.New("Console plan is not connected")
		}
		if connection.NeedsLogin {
			m.oauthMu.Unlock()
			return nil, &oauth.OAuthError{Code: "invalid_grant"}
		}
		if err := connection.ValidateForPlan(oauth.Plan(tier)); err != nil {
			m.oauthMu.Unlock()
			return nil, err
		}
		if time.Until(connection.Credential.ExpiresAt) >= 5*time.Minute {
			m.oauthMu.Unlock()
			return connection, nil
		}
		refresh := *refreshSlot
		if refresh == nil || refresh.done == nil && (refresh.credential != connection.Credential || !time.Now().Before(refresh.retryAt)) {
			refresh = &oauthRefresh{credential: connection.Credential, done: make(chan struct{})}
			*refreshSlot = refresh
			go m.refreshOAuth(connection, refresh, tier)
		}
		if refresh.done == nil {
			err := refresh.err
			m.oauthMu.Unlock()
			if err != nil {
				return nil, err
			}
			if !time.Now().Before(connection.Credential.ExpiresAt) {
				return nil, &oauth.OAuthError{Code: "expired_token"}
			}
			return connection, nil
		}
		done := refresh.done
		m.oauthMu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-done:
		}
	}
}

func (m *RuntimeController) refreshOAuth(connection *oauth.Connection, refresh *oauthRefresh, tier config.Tier) {
	root := m.root
	if root == nil {
		root = context.Background()
	}
	ctx, cancel := context.WithTimeout(root, 20*time.Second)
	defer cancel()
	client := m.oauthClient
	if client == nil {
		client = oauth.NewClient(nil)
	}
	credential, err := client.Refresh(ctx, connection.Credential)
	if err != nil {
		var oauthError *oauth.OAuthError
		if errors.As(err, &oauthError) {
			switch oauthError.Code {
			case "invalid_grant", "expired_token", "unauthorized", "access_denied", "sso_required":
				if saveErr := m.saveOAuthCredential(connection, connection.Credential, true, tier); saveErr != nil {
					err = saveErr
				}
			}
		} else {
			err = errors.New("Console authorization could not be refreshed")
		}
	} else {
		needsLogin := credential.OrganizationID != connection.Credential.OrganizationID
		err = m.saveOAuthCredential(connection, credential, needsLogin, tier)
		if err == nil && needsLogin {
			err = &oauth.OAuthError{Code: "invalid_grant"}
		}
	}
	m.oauthMu.Lock()
	defer m.oauthMu.Unlock()
	refresh.err = err
	refresh.retryAt = time.Now().Add(15 * time.Second)
	if err == nil {
		refresh.credential = credential
		refresh.retryAt = time.Now().Add(min(30*time.Second, max(time.Millisecond, time.Until(credential.ExpiresAt)/2)))
	}
	close(refresh.done)
	refresh.done = nil
}

func (m *RuntimeController) saveOAuthCredential(previous *oauth.Connection, credential oauth.Credential, needsLogin bool, tier config.Tier) error {
	changed := false
	_, err := m.Update(func(cfg *config.Config) error {
		connection := cfg.GoOAuth
		if tier == config.TierZen {
			connection = cfg.ZenOAuth
		}
		if connection == nil || connection.Credential != previous.Credential || connection.NeedsLogin != previous.NeedsLogin {
			changed = true
			return errors.New("Console plan connection changed")
		}
		connection.Credential = credential
		connection.NeedsLogin = needsLogin
		return nil
	})
	if changed {
		return errors.New("Console plan connection changed; retry with the current configuration")
	}
	if err != nil {
		return errors.New("Console authorization could not be saved safely; sign in again")
	}
	return nil
}

func (m *RuntimeController) invalidateOAuth(ctx context.Context, credential oauth.Credential, tiers ...config.Tier) {
	tier := config.TierGo
	if len(tiers) > 0 {
		tier = tiers[0]
	}
	if ctx.Err() != nil {
		return
	}
	unchanged := errors.New("Console authorization is no longer current")
	_, err := m.Update(func(cfg *config.Config) error {
		connection := cfg.GoOAuth
		if tier == config.TierZen {
			connection = cfg.ZenOAuth
		}
		if connection == nil || connection.Credential != credential || connection.NeedsLogin {
			return unchanged
		}
		connection.NeedsLogin = true
		return nil
	})
	if err != nil && !errors.Is(err, unchanged) && m.logger != nil {
		m.logger.Warn("Console authorization could not be marked for reconnect", "component", "auth", "event", "oauth_reconnect_save_failed", "tier", tier)
	}
}
