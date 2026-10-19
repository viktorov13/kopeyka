// Package config validates the optional language-model configuration before the
// server connects to external services. Errors identify fields without echoing
// environment values, which may contain credentials.
package config

import (
	"errors"
	"strconv"
	"strings"
	"time"
)

const (
	ModeRules = "rules"
	ModeLLM   = "llm"
)

type Config struct {
	BotMode            string
	LLMBaseURL         string
	LLMAPIKey          string
	LLMModel           string
	LLMReasoningEffort string
	LLMTimeout         time.Duration
	LLMMaxSteps        int
}

// Load accepts getenv so validation can be tested independently of process
// environment. Provider settings are used only when BOT_MODE=llm.
func Load(getenv func(string) string) (Config, error) {
	c := Config{BotMode: strings.TrimSpace(getenv("BOT_MODE"))}
	if c.BotMode == "" {
		c.BotMode = ModeRules
	}
	if c.BotMode != ModeRules && c.BotMode != ModeLLM {
		return Config{}, errors.New("BOT_MODE must be rules or llm")
	}
	if c.BotMode == ModeRules {
		return c, nil
	}
	c.LLMBaseURL = strings.TrimSpace(getenv("LLM_BASE_URL"))
	if c.LLMBaseURL == "" {
		c.LLMBaseURL = "https://api.openai.com/v1"
	}
	c.LLMAPIKey = getenv("LLM_API_KEY")
	c.LLMModel = strings.TrimSpace(getenv("LLM_MODEL"))
	if c.LLMModel == "" {
		return Config{}, errors.New("LLM_MODEL is required when BOT_MODE=llm")
	}
	c.LLMReasoningEffort = strings.TrimSpace(getenv("LLM_REASONING_EFFORT"))
	switch c.LLMReasoningEffort {
	case "", "none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra":
	default:
		return Config{}, errors.New("LLM_REASONING_EFFORT must be empty, none, minimal, low, medium, high, xhigh, max or ultra")
	}
	c.LLMTimeout = 20 * time.Second
	if value := strings.TrimSpace(getenv("LLM_TIMEOUT")); value != "" {
		duration, err := time.ParseDuration(value)
		if err != nil || duration < time.Second || duration > 60*time.Second {
			return Config{}, errors.New("LLM_TIMEOUT must be a duration between 1s and 60s")
		}
		c.LLMTimeout = duration
	}
	c.LLMMaxSteps = 6
	if value := strings.TrimSpace(getenv("LLM_MAX_STEPS")); value != "" {
		steps, err := strconv.Atoi(value)
		if err != nil || steps < 2 || steps > 12 {
			return Config{}, errors.New("LLM_MAX_STEPS must be an integer between 2 and 12")
		}
		c.LLMMaxSteps = steps
	}
	return c, nil
}
