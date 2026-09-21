package config

import "testing"

func TestRotationConfigValidationAndClone(t *testing.T) {
	c := RotationConfig{}
	c.Defaults()
	if err := c.Validate(nil); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*RotationConfig){
		func(c *RotationConfig) { c.SOTA.Alias = c.Sweet.Alias },
		func(c *RotationConfig) { c.SOTA.Alias = "glm-5.3" },
		func(c *RotationConfig) { c.SOTA.Alias = "real-model" },
		func(c *RotationConfig) { c.Sweet.Alias = "bad alias" },
		func(c *RotationConfig) { c.SOTA.Order = []string{"other-model"} },
		func(c *RotationConfig) { c.Sweet.Order = []string{"glm-5.3"} },
		func(c *RotationConfig) { c.Sweet.Order = []string{"x", "x"} },
		func(c *RotationConfig) { c.FirstOutputSeconds = -1 },
	} {
		candidate := Clone(Config{Rotation: c}).Rotation
		change(&candidate)
		if candidate.Validate([]string{"real-model"}) == nil {
			t.Fatal("accepted invalid config", candidate)
		}
	}
	clone := Clone(Config{Rotation: c})
	clone.Rotation.SOTA.Order[0] = "changed"
	if c.SOTA.Order[0] != "deepseek-v4.1-flash" {
		t.Fatal("cloning aliased config")
	}
}
