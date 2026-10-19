package config

import (
	"strings"
	"testing"
	"time"
)

func environment(values map[string]string) func(string) string {
	return func(key string) string { return values[key] }
}

func TestDefaultModeDoesNotRequireLLM(t *testing.T) {
	c, err := Load(environment(nil))
	if err != nil || c.BotMode != ModeRules {
		t.Fatalf("default configuration: mode=%q err=%v", c.BotMode, err)
	}
	c, err = Load(environment(map[string]string{"BOT_MODE": "rules", "LLM_TIMEOUT": "invalid", "LLM_MAX_STEPS": "invalid"}))
	if err != nil || c.BotMode != ModeRules || c.LLMAPIKey != "" {
		t.Fatalf("unused provider configuration should not block rules mode: %v", err)
	}
}

func TestLLMDefaultsAndOverrides(t *testing.T) {
	c, err := Load(environment(map[string]string{"BOT_MODE": "llm", "LLM_MODEL": "chosen-model"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.BotMode != ModeLLM || c.LLMModel != "chosen-model" || c.LLMBaseURL != "https://api.openai.com/v1" || c.LLMTimeout != 20*time.Second || c.LLMMaxSteps != 6 || c.LLMAPIKey != "" {
		t.Fatalf("unexpected default provider configuration")
	}
	c, err = Load(environment(map[string]string{"BOT_MODE": " llm ", "LLM_MODEL": " local-model ", "LLM_BASE_URL": "http://localhost:11434/v1", "LLM_API_KEY": "example-key", "LLM_REASONING_EFFORT": " none ", "LLM_TIMEOUT": "1m", "LLM_MAX_STEPS": "12"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.LLMModel != "local-model" || c.LLMBaseURL != "http://localhost:11434/v1" || c.LLMAPIKey != "example-key" || c.LLMReasoningEffort != "none" || c.LLMTimeout != time.Minute || c.LLMMaxSteps != 12 {
		t.Fatal("provider overrides were not applied")
	}
}

func TestInvalidLLMConfigurationDoesNotExposeValues(t *testing.T) {
	for _, tc := range []struct {
		name, field, value string
	}{
		{"mode", "BOT_MODE", "secret-invalid-mode"},
		{"missing model", "LLM_MODEL", " "},
		{"unknown reasoning effort", "LLM_REASONING_EFFORT", "secret-invalid-effort"},
		{"timeout syntax", "LLM_TIMEOUT", "secret-invalid-timeout"},
		{"short timeout", "LLM_TIMEOUT", "999ms"},
		{"long timeout", "LLM_TIMEOUT", "61s"},
		{"negative timeout", "LLM_TIMEOUT", "-1s"},
		{"steps syntax", "LLM_MAX_STEPS", "secret-invalid-steps"},
		{"fractional steps", "LLM_MAX_STEPS", "2.5"},
		{"few steps", "LLM_MAX_STEPS", "1"},
		{"many steps", "LLM_MAX_STEPS", "13"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			values := map[string]string{"BOT_MODE": "llm", "LLM_MODEL": "chosen-model", "LLM_API_KEY": "secret-provider-key", "LLM_BASE_URL": "https://secret-user:secret-password@example.com/v1"}
			values[tc.field] = tc.value
			c, err := Load(environment(values))
			if err == nil || !strings.Contains(err.Error(), tc.field) {
				t.Fatalf("expected field error for %s", tc.field)
			}
			if strings.Contains(err.Error(), "secret") || c != (Config{}) {
				t.Fatal("invalid configuration exposed a secret or returned partial configuration")
			}
		})
	}
}
