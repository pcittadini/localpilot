// Copyright 2026 The LocalPilot Authors
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestDefaultIsValid(t *testing.T) {
	if err := Default().Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestParseMergesWithDefaults(t *testing.T) {
	cfg, err := Parse([]byte(`
version: 1
models:
  default: fast
  available:
    - name: fast
      provider: ollama
      model: qwen3.5:9b
      temperature: 0.2
      context_window: 4096
      think: false
delegation:
  budgets:
    max_input_tokens: 3000
    max_output_tokens: 1024
    max_tool_calls: 4
    max_iterations: 3
    timeout_seconds: 30
`))
	if err != nil {
		t.Fatal(err)
	}
	m, ok := cfg.Model("")
	if !ok || m.Name != "fast" || m.ContextWindow != 4096 || m.Think == nil || *m.Think {
		t.Fatalf("default model = %+v", m)
	}
	if cfg.Delegation.Budgets.MaxInputTokens != 3000 {
		t.Errorf("budget not applied: %+v", cfg.Delegation.Budgets)
	}
	// Untouched sections keep their defaults.
	if cfg.Providers.Ollama.BaseURL != "http://localhost:11434" || !cfg.CategoryEnabled("code_analysis") {
		t.Errorf("defaults lost: %+v", cfg)
	}
}

func TestParseRejectsUnknownKeys(t *testing.T) {
	_, err := Parse([]byte("delegation:\n  budgetz:\n    max_input_tokens: 1\n"))
	if err == nil || !strings.Contains(err.Error(), "budgetz") {
		t.Fatalf("err = %v, want unknown field error", err)
	}
}

func TestValidateErrors(t *testing.T) {
	tests := map[string]string{
		"unknown default model":    "models:\n  default: nope\n",
		"bad url":                  "providers:\n  ollama:\n    base_url: localhost:11434\n",
		"unsupported provider":     "models:\n  default: m\n  available:\n    - {name: m, provider: openai, model: x}\n",
		"missing model id":         "models:\n  default: m\n  available:\n    - {name: m, provider: ollama}\n",
		"negative budget":          "delegation:\n  budgets:\n    max_tool_calls: -1\n",
		"bad version":              "version: 2\n",
		"bad log level":            "logging:\n  level: loud\n",
		"duplicate category":       "delegation:\n  policies:\n    - {category: extraction, enabled: true}\n    - {category: extraction, enabled: false}\n",
		"unknown category model":   "delegation:\n  policies:\n    - {category: extraction, enabled: true, model: nope}\n",
		"negative category budget": "delegation:\n  policies:\n    - {category: extraction, enabled: true, budgets: {max_tool_calls: -1}}\n",
		"relative metrics path":    "metrics:\n  path: data/metrics.db\n",
		"negative retention":       "metrics:\n  retention_days: -1\n",
		"unknown billing":          "savings:\n  billing: free\n",
		"unpriced reference model": "savings:\n  reference_model: claude-3-opus\n",
		"negative price":           "savings:\n  pricing:\n    - {model: claude-opus-5-5, input: -4, output: 20}\n",
		"duplicate price":          "savings:\n  pricing:\n    - {model: claude-opus-5-5, input: 4, output: 20}\n    - {model: claude-opus-5-5, input: 5, output: 25}\n",
	}
	for name, yml := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(yml)); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestLoadSources(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(file, []byte("providers:\n  ollama:\n    base_url: http://gpu-box:11434\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("explicit path", func(t *testing.T) {
		cfg, err := Load(LoadOptions{Path: file, Getenv: env(nil)})
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Source != file || cfg.Providers.Ollama.BaseURL != "http://gpu-box:11434" {
			t.Fatalf("got %+v", cfg)
		}
	})

	t.Run("env path", func(t *testing.T) {
		cfg, err := Load(LoadOptions{Getenv: env(map[string]string{EnvConfig: file})})
		if err != nil || cfg.Source != file {
			t.Fatalf("cfg=%v err=%v", cfg, err)
		}
	})

	t.Run("explicit path must exist", func(t *testing.T) {
		if _, err := Load(LoadOptions{Path: filepath.Join(dir, "missing.yaml"), Getenv: env(nil)}); err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("env overrides", func(t *testing.T) {
		cfg, err := Load(LoadOptions{Path: file, Getenv: env(map[string]string{
			EnvOllamaURL: "http://other:11434",
			EnvModel:     "qwen3.5:9b-q8_0",
			EnvWorkspace: "/tmp/ws",
			EnvLogLevel:  "debug",
		})})
		if err != nil {
			t.Fatal(err)
		}
		m, _ := cfg.Model("")
		if cfg.Providers.Ollama.BaseURL != "http://other:11434" || m.Model != "qwen3.5:9b-q8_0" ||
			cfg.Workspace != "/tmp/ws" || cfg.Logging.Level != "debug" {
			t.Fatalf("overrides not applied: %+v", cfg)
		}
	})

	t.Run("metrics path", func(t *testing.T) {
		cfg, err := Load(LoadOptions{Path: file, Getenv: env(map[string]string{"XDG_DATA_HOME": "/data"})})
		if err != nil || cfg.Metrics.Path != "/data/localpilot/metrics.db" {
			t.Fatalf("XDG path = %q, %v", cfg.Metrics.Path, err)
		}
		cfg, err = Load(LoadOptions{Path: file, Getenv: env(map[string]string{"XDG_DATA_HOME": "relative", EnvMetricsPath: "/tmp/m.db"})})
		if err != nil || cfg.Metrics.Path != "/tmp/m.db" {
			t.Fatalf("env path = %q, %v", cfg.Metrics.Path, err)
		}
		cfg, err = Load(LoadOptions{Path: file, Getenv: env(map[string]string{"XDG_DATA_HOME": "relative"})})
		if err != nil || !strings.HasSuffix(cfg.Metrics.Path, filepath.Join(".local", "share", "localpilot", "metrics.db")) {
			t.Fatalf("a relative XDG_DATA_HOME must be ignored: %q, %v", cfg.Metrics.Path, err)
		}
	})

	t.Run("invalid env override", func(t *testing.T) {
		if _, err := Load(LoadOptions{Path: file, Getenv: env(map[string]string{EnvOllamaURL: "nope"})}); err == nil {
			t.Fatal("expected validation error")
		}
	})
}

func TestCategoryEnabled(t *testing.T) {
	cfg := Default()
	if !cfg.CategoryEnabled("summarization") {
		t.Error("summarization should be enabled")
	}
	if cfg.CategoryEnabled("architecture") {
		t.Error("architecture should be disabled")
	}
	if cfg.CategoryEnabled("not_listed") {
		t.Error("unlisted categories must be disabled")
	}
	cfg.Delegation.Defaults.Enabled = false
	if cfg.CategoryEnabled("summarization") {
		t.Error("defaults.enabled=false must disable every category")
	}
}

// configs/default.yaml documents the defaults; keep the two in sync.
func TestShippedDefaultConfigMatchesDefaults(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "configs", "default.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg, Default()) {
		t.Errorf("configs/default.yaml differs from Default():\n got %+v\nwant %+v", cfg, Default())
	}
}

func TestSavingsConfig(t *testing.T) {
	cfg, err := Parse([]byte("savings:\n  billing: api\n  reference_model: claude-sonnet-5-5\n  pricing:\n    - {model: claude-sonnet-5-5, input: 2.5, output: 12}\n"))
	if err != nil {
		t.Fatal(err)
	}
	// A pricing list replaces the defaults rather than merging with them.
	if p, ok := cfg.Savings.Price("claude-sonnet-5-5"); !ok || p.Input != 2.5 || len(cfg.Savings.Pricing) != 1 {
		t.Errorf("pricing = %+v", cfg.Savings.Pricing)
	}
	if _, ok := cfg.Savings.Price("claude-opus-5-5"); ok {
		t.Error("default prices leaked into a custom list")
	}
	clone := cfg.Clone()
	clone.Savings.Pricing[0].Input = 99
	if cfg.Savings.Pricing[0].Input != 2.5 {
		t.Error("Clone shares the pricing list")
	}
}

func TestCategoryOverridesAndClone(t *testing.T) {
	cfg, err := Parse([]byte(`
delegation:
  policies:
    - category: summarization
      enabled: true
      tools: []
    - category: code_analysis
      enabled: true
      model: qwen-local
      tools: [read_file, search_code]
      budgets: {max_tool_calls: 4, timeout_seconds: 30}
`))
	if err != nil {
		t.Fatal(err)
	}
	sum, code := cfg.Delegation.Policies[0], cfg.Delegation.Policies[1]
	if sum.Tools == nil || len(*sum.Tools) != 0 {
		t.Errorf("tools: [] must mean no tools, got %v", sum.Tools)
	}
	if code.Model != "qwen-local" || len(*code.Tools) != 2 || code.Budgets.MaxToolCalls != 4 || code.Budgets.TimeoutSeconds != 30 {
		t.Errorf("code_analysis = %+v", code)
	}

	clone := cfg.Clone()
	(*clone.Delegation.Policies[1].Tools)[0] = "changed"
	clone.Delegation.Policies[0].Enabled = false
	clone.Delegation.Tools.Allowed[0] = "changed"
	*clone.Models.Available[0].Think = true
	if (*code.Tools)[0] != "read_file" || !cfg.Delegation.Policies[0].Enabled ||
		cfg.Delegation.Tools.Allowed[0] != "read_file" || *cfg.Models.Available[0].Think {
		t.Error("Clone shares state with the original")
	}
}
