package gateway

import (
	"context"
	"errors"
	"time"

	"opencode2api/internal/config"
	"opencode2api/internal/goauth"
)

type goSubscriptionRefresh struct {
	credential goauth.Credential
	done       chan struct{}
	err        error
	retryAt    time.Time
}

func (m *RuntimeController) ResolveGoSubscription(ctx context.Context) (*goauth.Connection, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		m.goSubscriptionMu.Lock()
		connection := m.Config().GoSubscription
		if connection == nil {
			m.goSubscriptionMu.Unlock()
			return nil, errors.New("Go subscription is not connected")
		}
		if connection.NeedsLogin {
			m.goSubscriptionMu.Unlock()
			return nil, &goauth.OAuthError{Code: "invalid_grant"}
		}
		if err := connection.Validate(); err != nil {
			m.goSubscriptionMu.Unlock()
			return nil, err
		}
		if time.Until(connection.Credential.ExpiresAt) >= 5*time.Minute {
			m.goSubscriptionMu.Unlock()
			return connection, nil
		}
		refresh := m.goRefresh
		if refresh == nil || refresh.done == nil && (refresh.credential != connection.Credential || !time.Now().Before(refresh.retryAt)) {
			refresh = &goSubscriptionRefresh{credential: connection.Credential, done: make(chan struct{})}
			m.goRefresh = refresh
			go m.refreshGoSubscription(connection, refresh)
		}
		if refresh.done == nil {
			err := refresh.err
			m.goSubscriptionMu.Unlock()
			if err != nil {
				return nil, err
			}
			if !time.Now().Before(connection.Credential.ExpiresAt) {
				return nil, &goauth.OAuthError{Code: "expired_token"}
			}
			return connection, nil
		}
		done := refresh.done
		m.goSubscriptionMu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-done:
		}
	}
}

func (m *RuntimeController) refreshGoSubscription(connection *goauth.Connection, refresh *goSubscriptionRefresh) {
	root := m.root
	if root == nil {
		root = context.Background()
	}
	ctx, cancel := context.WithTimeout(root, 20*time.Second)
	defer cancel()
	client := m.goAuthClient
	if client == nil {
		client = goauth.NewClient(nil)
	}
	credential, err := client.Refresh(ctx, connection.Credential)
	if err != nil {
		var oauthError *goauth.OAuthError
		if errors.As(err, &oauthError) {
			switch oauthError.Code {
			case "invalid_grant", "expired_token", "unauthorized", "access_denied", "sso_required":
				if saveErr := m.saveGoSubscriptionCredential(connection, connection.Credential, true); saveErr != nil {
					err = saveErr
				}
			}
		} else {
			err = errors.New("Go subscription authorization could not be refreshed")
		}
	} else {
		needsLogin := credential.OrganizationID != connection.Credential.OrganizationID
		err = m.saveGoSubscriptionCredential(connection, credential, needsLogin)
		if err == nil && needsLogin {
			err = &goauth.OAuthError{Code: "invalid_grant"}
		}
	}
	m.goSubscriptionMu.Lock()
	defer m.goSubscriptionMu.Unlock()
	refresh.err = err
	refresh.retryAt = time.Now().Add(15 * time.Second)
	if err == nil {
		refresh.credential = credential
		refresh.retryAt = time.Now().Add(min(30*time.Second, max(time.Millisecond, time.Until(credential.ExpiresAt)/2)))
	}
	close(refresh.done)
	refresh.done = nil
}

func (m *RuntimeController) saveGoSubscriptionCredential(previous *goauth.Connection, credential goauth.Credential, needsLogin bool) error {
	changed := false
	_, err := m.Update(func(cfg *config.Config) error {
		connection := cfg.GoSubscription
		if connection == nil || connection.Credential != previous.Credential || connection.NeedsLogin != previous.NeedsLogin {
			changed = true
			return errors.New("Go subscription connection changed")
		}
		connection.Credential = credential
		connection.NeedsLogin = needsLogin
		return nil
	})
	if changed {
		return errors.New("Go subscription connection changed; retry with the current configuration")
	}
	if err != nil {
		return errors.New("Go subscription authorization could not be saved safely; sign in again")
	}
	return nil
}

func (m *RuntimeController) invalidateGoSubscription(ctx context.Context, credential goauth.Credential) {
	if ctx.Err() != nil {
		return
	}
	unchanged := errors.New("Go subscription authorization is no longer current")
	_, err := m.Update(func(cfg *config.Config) error {
		connection := cfg.GoSubscription
		if connection == nil || connection.Credential != credential || connection.NeedsLogin {
			return unchanged
		}
		connection.NeedsLogin = true
		return nil
	})
	if err != nil && !errors.Is(err, unchanged) && m.logger != nil {
		m.logger.Warn("Go subscription authorization could not be marked for reconnect", "component", "auth", "event", "go_subscription_reconnect_save_failed")
	}
}
