package admin

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"opencode2api/internal/config"
	"opencode2api/internal/httpx"
)

type APIKeyView struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Display   string     `json:"display"`
	Enabled   bool       `json:"enabled"`
	CreatedAt *time.Time `json:"created_at,omitempty"`
}

func apiKeyViews(cfg config.Config) []APIKeyView {
	items := make([]APIKeyView, 0, len(cfg.ServerKeys))
	for i, key := range cfg.ServerKeys {
		id := config.Fingerprint(key)
		meta := cfg.ServerKeyMetadata[id]
		name := meta.Name
		if name == "" {
			name = fmt.Sprintf("已有 Key %d", i+1)
		}
		item := APIKeyView{ID: id, Name: name, Display: config.MaskValue(key), Enabled: !meta.Disabled}
		if !meta.CreatedAt.IsZero() {
			created := meta.CreatedAt
			item.CreatedAt = &created
		}
		items = append(items, item)
	}
	return items
}

func (a *Server) handleListAPIKeys(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"keys": apiKeyViews(a.manager.Config())})
}

func validateKeyName(name string) error {
	if name == "" || len([]rune(name)) > 80 {
		return errors.New("名称需为 1–80 个字符")
	}
	return nil
}

func addAPIKey(cfg *config.Config, name, key string) error {
	if err := validateKeyName(name); err != nil {
		return err
	}
	if len(key) < 16 || len(key) > 256 {
		return errors.New("自定义 Key 需为 16–256 位")
	}
	for _, ch := range key {
		if !((ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') || ch == '-' || ch == '_' || ch == '.') {
			return errors.New("Key 仅支持英文字母、数字、短横线、下划线和点")
		}
	}
	if len(cfg.ServerKeys) >= 200 {
		return errors.New("最多创建 200 个 API Key")
	}
	id := config.Fingerprint(key)
	for _, existing := range cfg.ServerKeys {
		if config.Fingerprint(existing) == id {
			return errors.New("此 API Key 已存在")
		}
	}
	if cfg.ServerKeyMetadata == nil {
		cfg.ServerKeyMetadata = map[string]config.ServerKeyMetadata{}
	}
	cfg.ServerKeys = append(cfg.ServerKeys, key)
	cfg.ServerKeyMetadata[id] = config.ServerKeyMetadata{Name: name, CreatedAt: time.Now().UTC()}
	return nil
}

func (a *Server) handleCreateAPIKey(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	}
	if err := decodeAdminJSON(w, r, &input); err != nil {
		writeAdminError(w, 400, "invalid_request", err.Error())
		return
	}
	key := input.Value
	if key == "" {
		var random [24]byte
		if _, err := rand.Read(random[:]); err != nil {
			writeAdminError(w, 500, "generation_failed", "无法生成密钥")
			return
		}
		key = "sk-local-" + hex.EncodeToString(random[:])
	}
	_, err := a.manager.Update(func(cfg *config.Config) error { return addAPIKey(cfg, strings.TrimSpace(input.Name), key) })
	if err != nil {
		writeAdminError(w, 400, "key_create_failed", a.manager.Redact(err.Error()))
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	httpx.WriteJSON(w, 201, map[string]any{"key": key, "keys": apiKeyViews(a.manager.Config())})
}

func changeAPIKey(cfg *config.Config, id string, name *string, enabled *bool, remove bool) error {
	index := -1
	for i, key := range cfg.ServerKeys {
		if config.Fingerprint(key) == id {
			index = i
			break
		}
	}
	if index < 0 {
		return errors.New("API Key 不存在，请刷新列表")
	}
	if cfg.ServerKeyMetadata == nil {
		cfg.ServerKeyMetadata = map[string]config.ServerKeyMetadata{}
	}
	if remove {
		cfg.ServerKeys = append(cfg.ServerKeys[:index], cfg.ServerKeys[index+1:]...)
		delete(cfg.ServerKeyMetadata, id)
	} else {
		meta := cfg.ServerKeyMetadata[id]
		if name != nil {
			value := strings.TrimSpace(*name)
			if err := validateKeyName(value); err != nil {
				return err
			}
			meta.Name = value
		}
		if enabled != nil {
			meta.Disabled = !*enabled
		}
		cfg.ServerKeyMetadata[id] = meta
	}
	if cfg.FirstEnabledServerKey() == "" {
		return errors.New("至少保留一个启用的 API Key，请先创建替代密钥")
	}
	return nil
}

func (a *Server) handleUpdateAPIKey(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Name    *string `json:"name"`
		Enabled *bool   `json:"enabled"`
	}
	if err := decodeAdminJSON(w, r, &input); err != nil {
		writeAdminError(w, 400, "invalid_request", err.Error())
		return
	}
	_, err := a.manager.Update(func(cfg *config.Config) error {
		return changeAPIKey(cfg, r.PathValue("id"), input.Name, input.Enabled, false)
	})
	if err != nil {
		writeAdminError(w, 400, "key_update_failed", a.manager.Redact(err.Error()))
		return
	}
	a.handleListAPIKeys(w, r)
}

func (a *Server) handleDeleteAPIKey(w http.ResponseWriter, r *http.Request) {
	_, err := a.manager.Update(func(cfg *config.Config) error { return changeAPIKey(cfg, r.PathValue("id"), nil, nil, true) })
	if err != nil {
		writeAdminError(w, 400, "key_delete_failed", a.manager.Redact(err.Error()))
		return
	}
	a.handleListAPIKeys(w, r)
}
