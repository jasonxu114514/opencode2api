package warp

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// defaultRateLimitPause is used when Cloudflare answers a registration
	// with 429 but omits Retry-After. It matches the historical registration
	// cadence.
	defaultRateLimitPause = 12 * time.Second
	// maxRateLimitPause bounds how long a rate-limit answer can keep new
	// registrations out. A larger server value is clamped so a bad header
	// cannot freeze the pool for hours.
	maxRateLimitPause = 5 * time.Minute
)

// RegistrationWindow is the longest pause Cloudflare's registration rate limit
// can impose: a caller that needs a first exit before it can serve waits out one
// full window rather than failing on a pause that is about to end.
const RegistrationWindow = maxRateLimitPause

// registrationPausedError reports that Cloudflare asked for a pause before
// the next device registration. The pool schedules its retry from it instead
// of sleeping inside the registration path.
type registrationPausedError struct {
	retryAfter time.Duration
	reason     string
}

func (e *registrationPausedError) Error() string {
	return fmt.Sprintf("Cloudflare registration paused for %s: %s", e.retryAfter.Round(time.Second), e.reason)
}

// registrationPause returns the delay a failed registration asked for, or 0
// when the failure was not a rate limit.
func registrationPause(err error) time.Duration {
	var paused *registrationPausedError
	if errors.As(err, &paused) {
		return paused.retryAfter
	}
	return 0
}

const (
	defaultAPIBase = "https://api.cloudflareclient.com/v0i1909051800"
	defaultPeer    = "bmXOC+F1FxEMF9dyiK2H5/1SUtzH0JuVo51h2wPfgyo="
)

var fallbackEndpoints = []string{
	"162.159.192.1:2408",
	"[2606:4700:d0::a29f:c001]:2408",
	"162.159.193.1:2408",
	"engage.cloudflareclient.com:2408",
}

type registration struct {
	ID            string
	Token         string
	PrivateKey    string
	PublicKey     string
	AddressV4     string
	AddressV6     string
	PeerPublicKey string
	Endpoint      string
	Endpoints     []string
}

type registrar struct {
	base   string
	client *http.Client

	mu          sync.Mutex
	blockedTill time.Time
}

func newRegistrar() *registrar {
	return &registrar{
		base: defaultAPIBase,
		client: &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				Proxy:               nil,
				ForceAttemptHTTP2:   false,
				MaxIdleConns:        4,
				IdleConnTimeout:     30 * time.Second,
				TLSHandshakeTimeout: 10 * time.Second,
			},
		},
	}
}

// blocked reports how much longer Cloudflare's last rate-limit answer keeps
// new registrations out.
func (r *registrar) blocked() time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	if wait := time.Until(r.blockedTill); wait > 0 {
		return wait
	}
	return 0
}

// pause records Cloudflare's rate-limit window. Existing windows only grow,
// so overlapping answers cannot shorten one another.
func (r *registrar) pause(wait time.Duration) {
	if wait <= 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if until := time.Now().Add(wait); until.After(r.blockedTill) {
		r.blockedTill = until
	}
}

// register creates and enables one WARP device. Registrations are not paced by
// a fixed gap: the pool registers in the background while traffic is served.
// Cloudflare's own 429/Retry-After answer becomes the pacing instead — it is
// recorded here and the caller retries when the window ends, so a rate limit
// never blocks registration behind a long sleep.
func (r *registrar) register(ctx context.Context) (registration, error) {
	if wait := r.blocked(); wait > 0 {
		return registration{}, &registrationPausedError{retryAfter: wait, reason: "Cloudflare rate limit"}
	}
	privateKey, publicKey, err := wireGuardKeypair()
	if err != nil {
		return registration{}, err
	}
	request := map[string]string{
		"key":           publicKey,
		"install_id":    "",
		"fcm_token":     "",
		"tos":           "2021-09-14T17:00:00.000+00:00",
		"model":         "PC",
		"serial_number": "",
		"locale":        "en_US",
	}
	payload, err := json.Marshal(request)
	if err != nil {
		return registration{}, err
	}
	var created deviceEnvelope
	if err := r.doJSON(ctx, http.MethodPost, "/reg", payload, "", &created); err != nil {
		return registration{}, fmt.Errorf("create WARP device: %w", err)
	}
	if created.Result.ID == "" || created.Result.Token == "" {
		return registration{}, errors.New("create WARP device: response omitted id or token")
	}

	enabled, err := json.Marshal(map[string]bool{"warp_enabled": true})
	if err != nil {
		return registration{}, err
	}
	var patched deviceEnvelope
	if err := r.doJSON(ctx, http.MethodPatch, "/reg/"+created.Result.ID, enabled, created.Result.Token, &patched); err != nil {
		return registration{}, fmt.Errorf("enable WARP device: %w", err)
	}
	var current deviceEnvelope
	if err := r.doJSON(ctx, http.MethodGet, "/reg/"+created.Result.ID, nil, created.Result.Token, &current); err != nil {
		return registration{}, fmt.Errorf("read WARP device: %w", err)
	}
	if !current.Result.WARPEnabled {
		return registration{}, errors.New("read WARP device: WARP is not enabled")
	}
	addresses := current.Result.Config.Interface.Addresses
	var peerPublicKey, endpoint string
	var endpoints []string
	if len(current.Result.Config.Peers) != 0 {
		peer := current.Result.Config.Peers[0]
		peerPublicKey = peer.PublicKey
		bases := []string{peer.Endpoint.V4, peer.Endpoint.V6, peer.Endpoint.Host}
		ports := []int{2408}
		for _, port := range peer.Endpoint.Ports {
			if port <= 0 || port > 65535 {
				continue
			}
			if port != 2408 {
				ports = append(ports, port)
			}
		}
		for _, port := range ports {
			for _, base := range bases {
				endpoints = append(endpoints, replaceEndpointPort(base, port))
			}
		}
		endpoints = append(endpoints, bases...)
	}
	endpoints = append(endpoints, fallbackEndpoints...)
	endpoints = uniqueEndpoints(endpoints)
	if peerPublicKey == "" {
		peerPublicKey = defaultPeer
	}
	if len(endpoints) != 0 {
		endpoint = endpoints[0]
	}
	if addresses.V4 == "" && addresses.V6 == "" {
		return registration{}, errors.New("read WARP device: config omitted both IP addresses")
	}
	return registration{
		ID:            created.Result.ID,
		Token:         created.Result.Token,
		PrivateKey:    privateKey,
		PublicKey:     publicKey,
		AddressV4:     addresses.V4,
		AddressV6:     addresses.V6,
		PeerPublicKey: peerPublicKey,
		Endpoint:      endpoint,
		Endpoints:     endpoints,
	}, nil
}

func replaceEndpointPort(endpoint string, port int) string {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return ""
	}
	host, _, err := net.SplitHostPort(endpoint)
	if err == nil {
		return net.JoinHostPort(host, strconv.Itoa(port))
	}
	if addr, err := netip.ParseAddr(endpoint); err == nil {
		return net.JoinHostPort(addr.String(), strconv.Itoa(port))
	}
	return net.JoinHostPort(endpoint, strconv.Itoa(port))
}

func uniqueEndpoints(endpoints []string) []string {
	seen := make(map[string]bool, len(endpoints))
	result := make([]string, 0, len(endpoints))
	for _, endpoint := range endpoints {
		endpoint = strings.TrimSpace(endpoint)
		if endpoint == "" || seen[endpoint] {
			continue
		}
		seen[endpoint] = true
		result = append(result, endpoint)
	}
	return result
}

func (r *registrar) doJSON(ctx context.Context, method, path string, body []byte, token string, target any) error {
	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		if attempt > 0 {
			wait := time.Duration(1<<(attempt-1)) * 2 * time.Second
			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
		var reader io.Reader
		if body != nil {
			reader = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(r.base, "/")+path, reader)
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("CF-Client-Version", "a-6.10-2158")
		req.Header.Set("User-Agent", "okhttp/3.12.1")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := r.client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		data, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if readErr != nil {
			lastErr = readErr
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			bodyText := strings.TrimSpace(string(data))
			if resp.StatusCode == http.StatusTooManyRequests {
				// Cloudflare's own limit becomes the pacing. Record it and fail
				// fast: sleeping here would hold the pool's create mutex and
				// stall replacements for the whole window.
				pause := parseRetryAfter(resp.Header.Get("Retry-After"))
				if pause <= 0 {
					pause = defaultRateLimitPause
				}
				if pause > maxRateLimitPause {
					pause = maxRateLimitPause
				}
				r.pause(pause)
				return &registrationPausedError{retryAfter: pause, reason: bodyText}
			}
			lastErr = fmt.Errorf("Cloudflare API HTTP %d: %s", resp.StatusCode, bodyText)
			if resp.StatusCode < 500 {
				return lastErr
			}
			continue
		}
		if err := json.Unmarshal(data, target); err != nil {
			return fmt.Errorf("decode Cloudflare API response: %w", err)
		}
		return nil
	}
	if lastErr == nil {
		lastErr = errors.New("Cloudflare API request failed")
	}
	return lastErr
}

func wireGuardKeypair() (string, string, error) {
	privateKey, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return "", "", err
	}
	return base64.StdEncoding.EncodeToString(privateKey.Bytes()), base64.StdEncoding.EncodeToString(privateKey.PublicKey().Bytes()), nil
}

func parseRetryAfter(value string) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(value); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	if at, err := http.ParseTime(value); err == nil && at.After(time.Now()) {
		return time.Until(at)
	}
	return 0
}

type deviceEnvelope struct {
	Result deviceResult `json:"result"`
}

type deviceResult struct {
	ID          string `json:"id"`
	Token       string `json:"token"`
	WARPEnabled bool   `json:"warp_enabled"`
	Config      struct {
		Interface struct {
			Addresses struct {
				V4 string `json:"v4"`
				V6 string `json:"v6"`
			} `json:"addresses"`
		} `json:"interface"`
		Peers []struct {
			PublicKey string `json:"public_key"`
			Endpoint  struct {
				V4    string `json:"v4"`
				V6    string `json:"v6"`
				Host  string `json:"host"`
				Ports []int  `json:"ports"`
			} `json:"endpoint"`
		} `json:"peers"`
	} `json:"config"`
}
