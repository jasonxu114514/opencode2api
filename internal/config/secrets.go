package config

import (
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"sort"
	"strings"
	"sync/atomic"
)

// MaskValue hides a secret for display: "••••" followed by its last five
// characters, or "••••" alone when the value has five characters or fewer.
func MaskValue(value string) string {
	runes := []rune(value)
	if len(runes) <= 5 {
		return "••••"
	}
	return "••••" + string(runes[len(runes)-5:])
}

// RedactURL renders a proxy entry or URL for display with any userinfo
// replaced by "***". A pool entry gets a #pool fragment when it has none, and
// an unparsable value becomes "<invalid>".
func RedactURL(raw string) string {
	clean, pool := ParseProxyEntry(raw)
	if clean != "" {
		raw = clean
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "<invalid>"
	}
	if u.User != nil {
		u.User = url.User("***")
	}
	if pool && u.Fragment == "" {
		u.Fragment = "pool"
	}
	return u.String()
}

// Redactor scrubs configured secrets from text such as log lines and error
// messages. It is safe for concurrent use: Replace swaps the whole secret set
// atomically.
type Redactor struct {
	values atomic.Value
}

// SecretRedactor returns a Redactor with no secrets loaded; Replace loads
// them.
func SecretRedactor() *Redactor {
	r := &Redactor{}
	r.values.Store([]string(nil))
	return r
}

// Replace loads the secrets of cfg: the server, Zen and Go keys, the WebUI
// plaintext password, each upstream URL that carries userinfo (whole, plus its
// username and password), and each entry of RuntimeProxies other than
// "direct" (as written and as its clean URL, plus any username and password).
// Longer secrets are replaced first, so a secret that contains a shorter one
// is scrubbed whole.
func (r *Redactor) Replace(cfg Config) {
	values := make([]string, 0, len(cfg.ServerKeys)+len(cfg.ZenKeys)+len(cfg.GoKeys)+2)
	values = append(values, cfg.ServerKeys...)
	values = append(values, cfg.ZenKeys...)
	values = append(values, cfg.GoKeys...)
	if cfg.WebUI.Password != "" {
		values = append(values, cfg.WebUI.Password)
	}
	for _, value := range []string{cfg.Upstream.Zen, cfg.Upstream.Go, cfg.Upstream.Anonymous} {
		if parsed, err := url.Parse(value); err == nil && parsed.User != nil {
			values = append(values, value, parsed.User.Username())
			if password, ok := parsed.User.Password(); ok {
				values = append(values, password)
			}
		}
	}
	for _, value := range cfg.RuntimeProxies() {
		if value != "direct" {
			clean, _ := ParseProxyEntry(value)
			values = append(values, value, clean)
			if parsed, err := url.Parse(clean); err == nil && parsed.User != nil {
				values = append(values, parsed.User.Username())
				if password, ok := parsed.User.Password(); ok {
					values = append(values, password)
				}
			}
		}
	}
	sort.Slice(values, func(i, j int) bool { return len(values[i]) > len(values[j]) })
	r.values.Store(values)
}

// String returns value with every loaded secret of at least four bytes
// replaced by "***".
func (r *Redactor) String(value string) string {
	for _, secret := range r.values.Load().([]string) {
		if len(secret) >= 4 {
			value = strings.ReplaceAll(value, secret, "***")
		}
	}
	return value
}

// Fingerprint returns the first 16 hex digits of the SHA-256 of value. It is
// the stable ID of a secret in config, session and usage bookkeeping
// (ServerKeyMetadata is keyed by it), and it doubles as the id of the masked
// secrets, API keys and Playground key choices that the WebUI shows and sends
// back.
func Fingerprint(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])[:16]
}

// KeyDisplayID returns the last five characters of value, or the whole value
// when it has five characters or fewer. It is intentionally separate from
// Fingerprint: it is meant to be read by a person, and it is the key_id of an
// upstream key in logs, request telemetry and the key status list.
func KeyDisplayID(value string) string {
	runes := []rune(value)
	if len(runes) <= 5 {
		return string(runes)
	}
	return string(runes[len(runes)-5:])
}
