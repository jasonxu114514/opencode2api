package config

import (
	"fmt"
	"regexp"
	"slices"
)

var SOTAModels = []string{"deepseek-v4.1-flash", "glm-5.3", "grok-4.6", "kimi-k3", "qwen3.8-max"}

type RotationGroup struct {
	Alias string   `json:"alias"`
	Order []string `json:"order"`
}
type RotationConfig struct {
	SOTA               RotationGroup `json:"sota"`
	Sweet              RotationGroup `json:"sweet"`
	FirstOutputSeconds int           `json:"first_output_seconds"`
}

func IsSOTA(model string) bool { return slices.Contains(SOTAModels, model) }
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
}
func (c RotationConfig) Validate(realModels []string) error {
	if c.FirstOutputSeconds < 1 || c.FirstOutputSeconds > 300 {
		return fmt.Errorf("首个有效输出超时需为 1–300 秒")
	}
	if c.SOTA.Alias == c.Sweet.Alias {
		return fmt.Errorf("两个分组的别名不能相同")
	}
	valid := regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	for i, g := range []RotationGroup{c.SOTA, c.Sweet} {
		if !valid.MatchString(g.Alias) {
			return fmt.Errorf("别名需为 1–64 位英文字母、数字、点、短横线或下划线")
		}
		if slices.Contains(realModels, g.Alias) || IsSOTA(g.Alias) || slices.Contains(c.Sweet.Order, g.Alias) {
			return fmt.Errorf("别名不能与真实模型 ID 相同")
		}
		seen := map[string]bool{}
		for _, model := range g.Order {
			if model == "" || seen[model] || IsSOTA(model) != (i == 0) {
				return fmt.Errorf("模型顺序包含重复、空值或不属于该分组的模型")
			}
			seen[model] = true
		}
		if i == 0 && len(g.Order) != len(SOTAModels) {
			return fmt.Errorf("SOTA 组必须包含指定的五个模型")
		}
	}
	return nil
}
