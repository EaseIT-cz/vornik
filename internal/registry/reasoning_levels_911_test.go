package registry

import (
	"testing"

	"vornik.io/vornik/internal/config"
)

// GitHub #911: config_assistant.reasoning is validated in package config,
// which cannot import this one. Both must accept the same levels.
func TestReasoningLevels_911_ConfigAssistantAgrees(t *testing.T) {
	for _, level := range []string{"", "off", "low", "medium", "high", "max", "Low", "none", "xhigh"} {
		cfgOK := config.AssistantConfig{Reasoning: level}.Validate() == nil
		if cfgOK != ValidReasoningLevel(level) {
			t.Errorf("level %q: config_assistant accepts %v, ValidReasoningLevel %v", level, cfgOK, ValidReasoningLevel(level))
		}
	}
}
