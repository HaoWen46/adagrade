package grading

import (
	"encoding/json"
	"fmt"
)

// Grading policy: the curated judgment-under-ambiguity stance (D25). Policy changes
// ONLY the ambiguity-resolution language in the prompt — the rubric, output schema,
// and parse/validation contract are untouched.
const (
	PolicyLenient  = "lenient"
	PolicyStandard = "standard"
	PolicyStrict   = "strict"
)

// ValidPolicy reports whether s is one of the three curated policy names (D25).
func ValidPolicy(s string) bool {
	switch s {
	case PolicyLenient, PolicyStandard, PolicyStrict:
		return true
	default:
		return false
	}
}

// MethodConfig is the config-as-data payload of a grading_method_versions row
// (plan §4: models, prompt, reasoning, reference solutions — knobs, not code).
type MethodConfig struct {
	Provider                string  `json:"provider"`
	Model                   string  `json:"model"`
	Temperature             float64 `json:"temperature"`                // 0 = deterministic default (B-H2)
	ReasoningLevel          string  `json:"reasoning_level,omitempty"`  // off|low|medium|high
	RefSolutions            int     `json:"ref_solutions"`              // 0 or 1 in v0
	ReaskCap                int     `json:"reask_cap"`                  // malformed-output re-asks (spec §5)
	Policy                  string  `json:"policy,omitempty"`           // lenient|standard|strict (D25); empty ⇒ standard
	PromptTemplateVersionID int64   `json:"prompt_template_version_id"` // pinned (D5)
	MaxTokens               int     `json:"max_tokens,omitempty"`
}

// ParseMethodConfig validates and applies defaults.
func ParseMethodConfig(raw []byte) (MethodConfig, error) {
	var c MethodConfig
	if err := json.Unmarshal(raw, &c); err != nil {
		return c, fmt.Errorf("method config: %w", err)
	}
	if c.Provider == "" || c.Model == "" {
		return c, fmt.Errorf("method config: provider and model are required")
	}
	if c.Temperature < 0 || c.Temperature > 2 {
		return c, fmt.Errorf("method config: temperature out of range [0,2]")
	}
	if c.ReaskCap == 0 {
		c.ReaskCap = 2
	}
	if c.ReaskCap < 0 || c.ReaskCap > 5 {
		return c, fmt.Errorf("method config: reask_cap out of range [0,5]")
	}
	if c.RefSolutions < 0 || c.RefSolutions > 1 {
		return c, fmt.Errorf("method config: ref_solutions must be 0 or 1 in v1")
	}
	if c.PromptTemplateVersionID == 0 {
		return c, fmt.Errorf("method config: prompt_template_version_id is required")
	}
	switch c.ReasoningLevel {
	case "", "off", "low", "medium", "high":
	default:
		return c, fmt.Errorf("method config: reasoning_level must be off|low|medium|high")
	}
	if c.Policy == "" {
		c.Policy = PolicyStandard
	} else if !ValidPolicy(c.Policy) {
		return c, fmt.Errorf("method config: policy %q must be lenient|standard|strict", c.Policy)
	}
	return c, nil
}
