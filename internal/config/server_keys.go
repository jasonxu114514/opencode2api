package config

import "time"

// Metadata is keyed by the existing key fingerprint; secret values remain in
// server_keys for compatibility with existing configuration and clients.
type ServerKeyMetadata struct {
	Name      string    `json:"name"`
	Disabled  bool      `json:"disabled,omitempty"`
	CreatedAt time.Time `json:"created_at,omitempty"`
}

func (c Config) ServerKeyEnabled(key string) bool {
	return !c.ServerKeyMetadata[Fingerprint(key)].Disabled
}

func (c Config) FirstEnabledServerKey() string {
	for _, key := range c.ServerKeys {
		if c.ServerKeyEnabled(key) {
			return key
		}
	}
	return ""
}
