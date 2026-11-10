package config

import (
	"fmt"
	"regexp"
	"slices"
)

// SOTAModels is the fixed default order of the built-in sota group. A custom
// group may reuse any model, including these, by listing it explicitly.
var SOTAModels = []string{"deepseek-v4.1-flash", "glm-5.3", "grok-4.6", "kimi-k3", "qwen3.8-max"}

// RotationGroup is one model-rotation group: Alias is the model name clients
// request to use the group, and Order is the failover order of its models.
type RotationGroup struct {
	Alias string   `json:"alias"`
	Order []string `json:"order"`
}

// RotationConfig configures the two model-rotation groups, which are served by
// the Go tier. FirstOutputSeconds bounds how long an attempt may take to
// produce its first output before the next attempt runs.
type RotationConfig struct {
	SOTA RotationGroup `json:"sota"`
	// Sweet is the built-in fallback group. An empty Order lets it absorb every
	// available non-SOTA model; a non-empty Order narrows it (models are kept as
	// they appear, availability is still resolved at runtime).
	Sweet RotationGroup `json:"sweet"`
	// Custom holds operator-defined rotation groups. Each one rotates only the
	// models named in its Order and never auto-populates.
	Custom             []RotationGroup `json:"custom,omitempty"`
	FirstOutputSeconds int             `json:"first_output_seconds"`
}

// IsSOTA reports whether model belongs to the built-in SOTA pool.
func IsSOTA(model string) bool { return slices.Contains(SOTAModels, model) }

// All returns every group in display order: the two built-ins first, then the
// custom groups.
func (c RotationConfig) All() []RotationGroup {
	groups := make([]RotationGroup, 0, 2+len(c.Custom))
	groups = append(groups, c.SOTA, c.Sweet)
	groups = append(groups, c.Custom...)
	return groups
}

func (c *RotationConfig) Defaults() {
	if c.SOTA.Alias == "" {
		c.SOTA.Alias = "sota"
	}
	if c.Sweet.Alias == "" {
		c.Sweet.Alias = "sweet"
	}
	if len(c.SOTA.Order) == 0 {
		c.SOTA.Order = slices.Clone(SOTAModels)
	}
	if c.FirstOutputSeconds == 0 {
		c.FirstOutputSeconds = 30
	}
	for i := range c.Custom {
		if c.Custom[i].Alias == "" {
			c.Custom[i].Alias = fmt.Sprintf("custom-%d", i+1)
		}
	}
}

// Validate requires a first-output timeout of 1-300 seconds; distinct aliases
// of up to 64 letters, digits, dots, dashes or underscores, starting with a
// letter or digit, that do not shadow a model ID (one of realModels, SOTAModels
// or any group order); and orders without empty or repeated entries, where the
// SOTA group holds exactly SOTAModels, the sweet group only other models, and
// custom groups are non-empty. A nil realModels skips the catalog check.
func (c RotationConfig) Validate(realModels []string) error {
	if c.FirstOutputSeconds < 1 || c.FirstOutputSeconds > 300 {
		return fmt.Errorf("首个有效输出超时需为 1–300 秒")
	}
	groups := c.All()
	// Models named anywhere. An alias may not shadow a model that another group
	// rotates, because routing resolves aliases before real model IDs.
	models := map[string]bool{}
	for i, g := range groups {
		seen := map[string]bool{}
		for _, model := range g.Order {
			if model == "" || seen[model] {
				return fmt.Errorf("模型顺序包含重复或空值")
			}
			seen[model] = true
			models[model] = true
		}
		switch i {
		case 0:
			if len(g.Order) != len(SOTAModels) {
				return fmt.Errorf("SOTA 组必须包含指定的五个模型")
			}
			for _, model := range g.Order {
				if !IsSOTA(model) {
					return fmt.Errorf("SOTA 组只能包含指定的模型")
				}
			}
		case 1:
			for _, model := range g.Order {
				if IsSOTA(model) {
					return fmt.Errorf("甜点组不能包含 SOTA 模型")
				}
			}
		default:
			if len(g.Order) == 0 {
				return fmt.Errorf("自定义分组至少需要一个模型")
			}
		}
	}
	valid := regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	aliases := map[string]bool{}
	for _, g := range groups {
		if !valid.MatchString(g.Alias) {
			return fmt.Errorf("别名需为 1–64 位英文字母、数字、点、短横线或下划线")
		}
		if aliases[g.Alias] || IsSOTA(g.Alias) || models[g.Alias] || slices.Contains(realModels, g.Alias) {
			return fmt.Errorf("别名不能与真实模型 ID 或其它分组相同")
		}
		aliases[g.Alias] = true
	}
	return nil
}
