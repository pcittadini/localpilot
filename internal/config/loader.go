// Copyright 2026 The LocalPilot Authors
// SPDX-License-Identifier: Apache-2.0

// Package config loads LocalPilot configuration from YAML files and
// environment variables.
//
// Precedence (lowest to highest): built-in defaults, the global config file,
// environment variable overrides. Project-level configuration is deliberately
// not loaded yet: it arrives with the policy engine (Phase 3), which can
// guarantee that a project file never grants capabilities the global file
// forbids.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Environment variables recognised by Load.
const (
	EnvConfig      = "LOCALPILOT_CONFIG"
	EnvWorkspace   = "LOCALPILOT_WORKSPACE"
	EnvOllamaURL   = "LOCALPILOT_OLLAMA_URL"
	EnvModel       = "LOCALPILOT_MODEL"
	EnvLogLevel    = "LOCALPILOT_LOG_LEVEL"
	EnvLogFile     = "LOCALPILOT_LOG_FILE"
	currentVersion = 1
)

type Config struct {
	Version    int        `yaml:"version"`
	Workspace  string     `yaml:"workspace"`
	Providers  Providers  `yaml:"providers"`
	Models     Models     `yaml:"models"`
	Delegation Delegation `yaml:"delegation"`
	Logging    Logging    `yaml:"logging"`

	// Source is the file the configuration was read from, or "" when only
	// defaults and environment overrides apply.
	Source string `yaml:"-"`
}

type Providers struct {
	Ollama OllamaProvider `yaml:"ollama"`
}

type OllamaProvider struct {
	BaseURL string `yaml:"base_url"`
}

type Models struct {
	Default   string        `yaml:"default"`
	Available []ModelConfig `yaml:"available"`
}

type ModelConfig struct {
	// Name is the logical name used by LocalPilot and its policies.
	Name string `yaml:"name"`
	// Provider selects the inference backend. Only "ollama" exists today.
	Provider string `yaml:"provider"`
	// Model is the provider-specific model identifier, e.g. "qwen3.5:9b".
	Model       string  `yaml:"model"`
	Temperature float64 `yaml:"temperature"`
	MaxTokens   int     `yaml:"max_tokens"`
	// ContextWindow is the context length requested from the provider (Ollama
	// num_ctx). Input budgets are clamped so prompt plus output fit inside it,
	// so prompts are never silently truncated. 0 leaves the provider default
	// and disables the clamp.
	ContextWindow int `yaml:"context_window"`
	// Think toggles the model's reasoning mode. Unset leaves the provider
	// default; reasoning tokens count against MaxTokens.
	Think *bool `yaml:"think"`
}

type Delegation struct {
	Defaults Defaults         `yaml:"defaults"`
	Budgets  Budgets          `yaml:"budgets"`
	Policies []CategoryPolicy `yaml:"policies"`
	Tools    ToolPolicy       `yaml:"tools"`
}

// ToolPolicy controls which tools the local model may call. Denied always
// wins; allowed names must be tools LocalPilot implements.
type ToolPolicy struct {
	Allowed []string `yaml:"allowed"`
	Denied  []string `yaml:"denied"`
	// TimeoutSeconds bounds a single tool call.
	TimeoutSeconds int `yaml:"timeout_seconds"`
	// MaxResultBytes bounds what one tool call returns to the model.
	MaxResultBytes int `yaml:"max_result_bytes"`
}

type Defaults struct {
	Enabled bool `yaml:"enabled"`
	// Fallback tells Claude who handles a task LocalPilot cannot complete.
	// It is advisory: LocalPilot never calls a cloud model itself.
	Fallback          string `yaml:"fallback"`
	RequireValidation bool   `yaml:"require_validation"`
}

type Budgets struct {
	MaxInputTokens  int `yaml:"max_input_tokens"`
	MaxOutputTokens int `yaml:"max_output_tokens"`
	MaxToolCalls    int `yaml:"max_tool_calls"`
	MaxIterations   int `yaml:"max_iterations"`
	TimeoutSeconds  int `yaml:"timeout_seconds"`
}

// CategoryPolicy configures one task category. Unset fields inherit the
// delegation-wide settings.
type CategoryPolicy struct {
	Category string `yaml:"category"`
	Enabled  bool   `yaml:"enabled"`
	// Model is the logical model that runs this category ("" = default).
	Model string `yaml:"model,omitempty"`
	// Tools restricts the category to a subset of the enabled tools. Unset
	// inherits all enabled tools; an empty list disables tools.
	Tools *[]string `yaml:"tools,omitempty"`
	// Budgets overrides delegation budgets for this category (0 = inherit).
	Budgets Budgets `yaml:"budgets,omitempty"`
}

type Logging struct {
	Level string `yaml:"level"`
	// File receives logs when set; otherwise they go to stderr. Logs never go
	// to stdout, which belongs to the MCP stdio transport.
	File string `yaml:"file"`
}

// Default returns the built-in configuration.
func Default() *Config {
	return &Config{
		Version: currentVersion,
		Providers: Providers{
			Ollama: OllamaProvider{BaseURL: "http://localhost:11434"},
		},
		Models: Models{
			Default: "qwen-local",
			Available: []ModelConfig{{
				Name:          "qwen-local",
				Provider:      "ollama",
				Model:         "qwen3.5:9b",
				Temperature:   0.1,
				MaxTokens:     2048,
				ContextWindow: 8192,
				Think:         Bool(false),
			}},
		},
		Delegation: Delegation{
			Defaults: Defaults{Enabled: true, Fallback: "claude", RequireValidation: true},
			Budgets: Budgets{
				MaxInputTokens:  12000,
				MaxOutputTokens: 2048,
				MaxToolCalls:    8,
				MaxIterations:   5,
				TimeoutSeconds:  60,
			},
			Policies: []CategoryPolicy{
				{Category: "extraction", Enabled: true},
				{Category: "summarization", Enabled: true},
				{Category: "log_analysis", Enabled: true},
				{Category: "code_analysis", Enabled: true},
				{Category: "documentation", Enabled: true},
				{Category: "code_generation", Enabled: false},
				{Category: "architecture", Enabled: false},
				{Category: "security_review", Enabled: false},
			},
			Tools: ToolPolicy{
				Allowed:        []string{"read_file", "search_code", "list_files", "git_diff", "git_log"},
				Denied:         []string{"shell", "write_file", "delete_file", "network_request"},
				TimeoutSeconds: 10,
				MaxResultBytes: 8000,
			},
		},
		Logging: Logging{Level: "info"},
	}
}

// LoadOptions controls where Load looks for configuration.
type LoadOptions struct {
	// Path is an explicit config file (e.g. from a --config flag). It must
	// exist when set.
	Path string
	// Getenv reads environment variables; os.Getenv when nil.
	Getenv func(string) string
}

// GlobalPath returns the default global config location,
// ~/.config/localpilot/config.yaml.
func GlobalPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "localpilot", "config.yaml")
}

// Load builds the effective configuration and validates it.
func Load(opts LoadOptions) (*Config, error) {
	getenv := opts.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}

	cfg := Default()

	path, required := opts.Path, opts.Path != ""
	if !required {
		if p := getenv(EnvConfig); p != "" {
			path, required = p, true
		} else {
			path = GlobalPath()
		}
	}
	if path != "" {
		data, err := os.ReadFile(path)
		switch {
		case err == nil:
			if err := decodeInto(cfg, data); err != nil {
				return nil, fmt.Errorf("config %s: %w", path, err)
			}
			cfg.Source = path
		case errors.Is(err, os.ErrNotExist) && !required:
			// No global config: defaults apply.
		default:
			return nil, fmt.Errorf("read config: %w", err)
		}
	}

	applyEnv(cfg, getenv)

	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Parse decodes YAML on top of the defaults and validates the result.
func Parse(data []byte) (*Config, error) {
	cfg := Default()
	if err := decodeInto(cfg, data); err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func decodeInto(cfg *Config, data []byte) error {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	// Reject unknown keys so a typo in a security setting is an error, not a
	// silently ignored line.
	dec.KnownFields(true)
	if err := dec.Decode(cfg); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

func applyEnv(cfg *Config, getenv func(string) string) {
	if v := getenv(EnvWorkspace); v != "" {
		cfg.Workspace = v
	}
	if v := getenv(EnvOllamaURL); v != "" {
		cfg.Providers.Ollama.BaseURL = v
	}
	if v := getenv(EnvModel); v != "" {
		for i := range cfg.Models.Available {
			if cfg.Models.Available[i].Name == cfg.Models.Default {
				cfg.Models.Available[i].Model = v
			}
		}
	}
	if v := getenv(EnvLogLevel); v != "" {
		cfg.Logging.Level = v
	}
	if v := getenv(EnvLogFile); v != "" {
		cfg.Logging.File = v
	}
}

// Validate reports the first structural problem in the configuration.
func (c *Config) Validate() error {
	var errs []string
	add := func(format string, args ...any) { errs = append(errs, fmt.Sprintf(format, args...)) }

	if c.Version != currentVersion {
		add("version: unsupported value %d (want %d)", c.Version, currentVersion)
	}

	if u, err := url.Parse(c.Providers.Ollama.BaseURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		add("providers.ollama.base_url: must be an http(s) URL, got %q", c.Providers.Ollama.BaseURL)
	}

	seen := map[string]bool{}
	for i, m := range c.Models.Available {
		switch {
		case m.Name == "":
			add("models.available[%d].name: required", i)
		case seen[m.Name]:
			add("models.available[%d].name: duplicate %q", i, m.Name)
		}
		seen[m.Name] = true
		if m.Provider != "ollama" {
			add("models.available[%d].provider: unsupported %q (supported: ollama)", i, m.Provider)
		}
		if m.Model == "" {
			add("models.available[%d].model: required", i)
		}
		if m.Temperature < 0 || m.Temperature > 2 {
			add("models.available[%d].temperature: must be within [0, 2]", i)
		}
		if m.MaxTokens < 0 || m.ContextWindow < 0 {
			add("models.available[%d]: max_tokens and context_window must not be negative", i)
		}
	}
	if !seen[c.Models.Default] {
		add("models.default: %q is not listed in models.available", c.Models.Default)
	}

	b := c.Delegation.Budgets
	if b.MaxInputTokens <= 0 || b.MaxOutputTokens <= 0 || b.MaxToolCalls <= 0 || b.MaxIterations <= 0 || b.TimeoutSeconds <= 0 {
		add("delegation.budgets: all budgets must be positive")
	}

	if t := c.Delegation.Tools; t.TimeoutSeconds <= 0 || t.MaxResultBytes < 1000 || t.MaxResultBytes > 1<<20 {
		add("delegation.tools: timeout_seconds must be positive and max_result_bytes within [1000, 1048576]")
	}

	cats := map[string]bool{}
	for i, p := range c.Delegation.Policies {
		if p.Category == "" {
			add("delegation.policies[%d].category: required", i)
		}
		if cats[p.Category] {
			add("delegation.policies[%d].category: duplicate %q", i, p.Category)
		}
		cats[p.Category] = true
		if p.Model != "" && !seen[p.Model] {
			add("delegation.policies[%d].model: %q is not listed in models.available", i, p.Model)
		}
		if b := p.Budgets; b.MaxInputTokens < 0 || b.MaxOutputTokens < 0 || b.MaxToolCalls < 0 || b.MaxIterations < 0 || b.TimeoutSeconds < 0 {
			add("delegation.policies[%d].budgets: must not be negative (0 inherits)", i)
		}
	}

	switch strings.ToLower(c.Logging.Level) {
	case "debug", "info", "warn", "error":
	default:
		add("logging.level: must be debug, info, warn or error")
	}

	if len(errs) > 0 {
		return fmt.Errorf("invalid configuration:\n  %s", strings.Join(errs, "\n  "))
	}
	return nil
}

// Bool returns a pointer to b, for optional boolean settings.
func Bool(b bool) *bool { return &b }

// Model returns the named model, or the default model when name is empty.
func (c *Config) Model(name string) (ModelConfig, bool) {
	if name == "" {
		name = c.Models.Default
	}
	for _, m := range c.Models.Available {
		if m.Name == name {
			return m, true
		}
	}
	return ModelConfig{}, false
}

// Clone returns a deep copy, so policy merging never mutates the original.
func (c *Config) Clone() *Config {
	out := *c
	out.Models.Available = append([]ModelConfig(nil), c.Models.Available...)
	for i, m := range out.Models.Available {
		if m.Think != nil {
			out.Models.Available[i].Think = Bool(*m.Think)
		}
	}
	out.Delegation.Policies = append([]CategoryPolicy(nil), c.Delegation.Policies...)
	for i, p := range out.Delegation.Policies {
		if p.Tools != nil {
			tools := append([]string{}, *p.Tools...)
			out.Delegation.Policies[i].Tools = &tools
		}
	}
	out.Delegation.Tools.Allowed = append([]string(nil), c.Delegation.Tools.Allowed...)
	out.Delegation.Tools.Denied = append([]string(nil), c.Delegation.Tools.Denied...)
	return &out
}

// CategoryEnabled reports whether the configuration enables a category.
// Categories absent from the policy list are disabled.
func (c *Config) CategoryEnabled(category string) bool {
	if !c.Delegation.Defaults.Enabled {
		return false
	}
	for _, p := range c.Delegation.Policies {
		if p.Category == category {
			return p.Enabled
		}
	}
	return false
}
