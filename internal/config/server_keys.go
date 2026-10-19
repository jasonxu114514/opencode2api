package config

import "time"

// ServerKeyMetadata holds the WebUI-managed details of one server key: its
// display name, whether it is disabled, and when it was created. It is keyed
// by the existing key fingerprint; secret values remain in server_keys for
// compatibility with existing configuration and clients.
type ServerKeyMetadata struct {
	Name      string    `json:"name"`
	Disabled  bool      `json:"disabled,omitempty"`
	CreatedAt time.Time `json:"created_at,omitempty"`
}

// ServerKeyEnabled reports whether key may authenticate clients. A key
// without metadata is enabled.
func (c Config) ServerKeyEnabled(key string) bool {
	return !c.ServerKeyMetadata[Fingerprint(key)].Disabled
}

// FirstEnabledServerKey returns the first enabled server key, or "" when every
// key is disabled. The Playground sends its requests with this key.
func (c Config) FirstEnabledServerKey() string {
	for _, key := range c.ServerKeys {
		if c.ServerKeyEnabled(key) {
			return key
		}
	}
	return ""
}
