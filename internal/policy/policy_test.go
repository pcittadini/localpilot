// Copyright 2026 The LocalPilot Authors
// SPDX-License-Identifier: Apache-2.0

package policy

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pcittadini/localpilot/internal/config"
	"github.com/pcittadini/localpilot/internal/workspace"
)

var (
	supported = []string{"extraction", "summarization", "log_analysis", "code_analysis", "documentation"}
	known     = []string{"read_file", "search_code", "list_files", "git_diff", "git_log"}
)

func engine(t *testing.T, cfg *config.Config, mutate func(*Options)) *Engine {
	t.Helper()
	opts := Options{Supported: supported, Known: known, Enabled: known}
	if mutate != nil {
		mutate(&opts)
	}
	e, err := New(cfg, opts)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func policyFor(cfg *config.Config, category string) *config.CategoryPolicy {
	for i := range cfg.Delegation.Policies {
		if cfg.Delegation.Policies[i].Category == category {
			return &cfg.Delegation.Policies[i]
		}
	}
	return nil
}

func wantReject(t *testing.T, d Decision, reason, detail string) {
	t.Helper()
	if d.Allowed || d.Reason != reason || !strings.Contains(d.Detail, detail) {
		t.Fatalf("decision = %+v, want rejection %s containing %q", d, reason, detail)
	}
}

func TestDefaultPolicy(t *testing.T) {
	e := engine(t, config.Default(), nil)
	d := e.Evaluate(Request{Category: "code_analysis"})
	want := Decision{
		Allowed:  true,
		Category: "code_analysis",
		Model:    "qwen-local",
		Tools:    known,
		Budgets: Budgets{
			MaxInputTokens:  8192 - 2048, // clamped to the context window
			MaxOutputTokens: 2048,
			MaxToolCalls:    8,
			MaxIterations:   5,
			Timeout:         60 * time.Second,
		},
		Version: e.Version(),
	}
	if !reflect.DeepEqual(d, want) {
		t.Fatalf("decision =\n%+v\nwant\n%+v", d, want)
	}
	if !slices.Equal(e.EnabledCategories(), supported) || len(e.Warnings()) != 0 {
		t.Errorf("enabled=%v warnings=%v", e.EnabledCategories(), e.Warnings())
	}
}

func TestDecisionsAreDeterministic(t *testing.T) {
	a, b := engine(t, config.Default(), nil), engine(t, config.Default(), nil)
	if a.Version() != b.Version() || len(a.Version()) != 12 {
		t.Fatalf("versions %q and %q", a.Version(), b.Version())
	}
	req := Request{Category: "summarization", RestrictTools: true, Tools: []string{"git_diff", "read_file"}}
	if !reflect.DeepEqual(a.Evaluate(req), b.Evaluate(req)) || !reflect.DeepEqual(a.Evaluate(req), a.Evaluate(req)) {
		t.Error("equal policies and requests must give equal decisions")
	}

	cfg := config.Default()
	cfg.Delegation.Budgets.MaxToolCalls = 7
	if engine(t, cfg, nil).Version() == a.Version() {
		t.Error("a different policy must have a different version")
	}
}

func TestCategoryRejections(t *testing.T) {
	cfg := config.Default()
	policyFor(cfg, "extraction").Enabled = false
	cfg.Delegation.Policies = slices.DeleteFunc(cfg.Delegation.Policies, func(p config.CategoryPolicy) bool { return p.Category == "documentation" })
	e := engine(t, cfg, nil)

	wantReject(t, e.Evaluate(Request{Category: "poetry"}), ReasonPolicy, "not supported")
	wantReject(t, e.Evaluate(Request{Category: "security_review"}), ReasonPolicy, "not supported")
	wantReject(t, e.Evaluate(Request{Category: "extraction"}), ReasonPolicy, "disabled by configuration")
	wantReject(t, e.Evaluate(Request{Category: "documentation"}), ReasonPolicy, "not enabled by configuration")

	cfg = config.Default()
	cfg.Delegation.Defaults.Enabled = false
	e = engine(t, cfg, nil)
	wantReject(t, e.Evaluate(Request{Category: "summarization"}), ReasonPolicy, "disabled")
	if len(e.EnabledCategories()) != 0 {
		t.Errorf("enabled = %v", e.EnabledCategories())
	}

	cfg = config.Default()
	policyFor(cfg, "architecture").Enabled = true
	e = engine(t, cfg, nil)
	wantReject(t, e.Evaluate(Request{Category: "architecture"}), ReasonPolicy, "not supported")
	if !strings.Contains(strings.Join(e.Warnings(), "\n"), `"architecture" is enabled but not implemented`) {
		t.Errorf("warnings = %v", e.Warnings())
	}
}

func TestRequestedTools(t *testing.T) {
	cfg := config.Default()
	policyFor(cfg, "summarization").Tools = &[]string{"read_file", "git_diff"}
	e := engine(t, cfg, nil)

	wantReject(t, e.Evaluate(Request{Category: "summarization", RestrictTools: true, Tools: []string{"shell"}}), ReasonInvalidRequest, `unknown tool "shell"`)
	wantReject(t, e.Evaluate(Request{Category: "summarization", RestrictTools: true, Tools: []string{"git_log"}}), ReasonPolicy, `"git_log" is not permitted`)

	d := e.Evaluate(Request{Category: "summarization", RestrictTools: true, Tools: []string{"git_diff", "read_file", "git_diff"}})
	if !d.Allowed || !slices.Equal(d.Tools, []string{"read_file", "git_diff"}) {
		t.Errorf("subset: %+v", d)
	}
	d = e.Evaluate(Request{Category: "summarization", RestrictTools: true, Tools: []string{}})
	if !d.Allowed || len(d.Tools) != 0 {
		t.Errorf("empty list must disable tools: %+v", d)
	}
	d = e.Evaluate(Request{Category: "summarization"})
	if !slices.Equal(d.Tools, []string{"read_file", "git_diff"}) {
		t.Errorf("unrestricted request gets the category's tools: %v", d.Tools)
	}
}

func TestCategoryOverrides(t *testing.T) {
	cfg := config.Default()
	cfg.Models.Available = append(cfg.Models.Available, config.ModelConfig{
		Name: "big", Provider: "ollama", Model: "qwen3.5:27b", Temperature: 0.1, MaxTokens: 1024, ContextWindow: 16384,
	})
	code := policyFor(cfg, "code_analysis")
	code.Model = "big"
	code.Tools = &[]string{"git_log", "read_file"}
	code.Budgets = config.Budgets{MaxToolCalls: 12, TimeoutSeconds: 90, MaxOutputTokens: 4096}
	policyFor(cfg, "log_analysis").Tools = &[]string{}

	e := engine(t, cfg, func(o *Options) { o.Enabled = []string{"read_file", "search_code", "list_files"} })
	d := e.Evaluate(Request{Category: "code_analysis"})
	// Tools follow registry order and drop tools not enabled here (git_log).
	// Output is capped by the model's max_tokens, input by its context.
	if d.Model != "big" || !slices.Equal(d.Tools, []string{"read_file"}) || d.Budgets.MaxToolCalls != 12 ||
		d.Budgets.Timeout != 90*time.Second || d.Budgets.MaxOutputTokens != 1024 || d.Budgets.MaxInputTokens != 12000 {
		t.Fatalf("decision = %+v", d)
	}
	if !strings.Contains(strings.Join(e.Warnings(), "\n"), `"git_log", which is not enabled here`) {
		t.Errorf("warnings = %v", e.Warnings())
	}
	if d := e.Evaluate(Request{Category: "log_analysis"}); len(d.Tools) != 0 {
		t.Errorf("tools: [] must disable tools: %v", d.Tools)
	}
	if d := e.Evaluate(Request{Category: "extraction"}); d.Model != "qwen-local" || d.Budgets.MaxToolCalls != 8 {
		t.Errorf("other categories keep the defaults: %+v", d)
	}
}

func TestEngineConfigurationErrors(t *testing.T) {
	cfg := config.Default()
	policyFor(cfg, "extraction").Tools = &[]string{"read_file", "shell"}
	if _, err := New(cfg, Options{Supported: supported, Known: known, Enabled: known}); err == nil || !strings.Contains(err.Error(), `unknown tool "shell"`) {
		t.Errorf("unknown category tool: err = %v", err)
	}

	cfg = config.Default()
	cfg.Models.Available[0].ContextWindow = 2048
	if _, err := New(cfg, Options{Supported: supported, Known: known}); err == nil || !strings.Contains(err.Error(), "leaves no room") {
		t.Errorf("context too small: err = %v", err)
	}
}

func TestFailClosed(t *testing.T) {
	e := engine(t, config.Default(), func(o *Options) {
		o.ProjectPath, o.ProjectErr = ProjectConfigPath, errors.New("yaml: line 3: bad indentation")
	})
	d := e.Evaluate(Request{Category: "summarization"})
	wantReject(t, d, ReasonPolicy, ".localpilot/config.yaml is invalid")
	if !strings.Contains(d.Detail, "bad indentation") || e.EnabledCategories() != nil || e.FailClosed() == nil {
		t.Errorf("decision = %+v", d)
	}
	if e.Version() == engine(t, config.Default(), nil).Version() {
		t.Error("a fail-closed policy must not share the version of the open one")
	}
}

func TestParseProjectRejectsForbiddenSettings(t *testing.T) {
	forbidden := map[string]string{
		"providers": "providers:\n  ollama:\n    base_url: http://attacker.example:11434\n",
		"models":    "models:\n  default: x\n",
		"logging":   "logging:\n  file: /tmp/steal.log\n",
		"workspace": "workspace: /\n",
		"metrics":   "metrics:\n  path: /tmp/collect.db\n",
		"savings":   "savings:\n  billing: api\n",
		"fallback":  "delegation:\n  defaults:\n    fallback: cloud\n",
		"version":   "version: 2\n",
		"typo":      "delegation:\n  polices: []\n",
		"too large": "# " + strings.Repeat("x", maxProjectConfigBytes) + "\n",
	}
	for name, yml := range forbidden {
		if _, err := ParseProject([]byte(yml)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	if p, err := ParseProject([]byte("")); err != nil || p == nil {
		t.Errorf("empty file: %v, %v", p, err)
	}
}

func project(t *testing.T, yml string) *ProjectConfig {
	t.Helper()
	p, err := ParseProject([]byte(yml))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestApplyNarrows(t *testing.T) {
	global := config.Default()
	policyFor(global, "code_analysis").Budgets.MaxToolCalls = 12
	global.Models.Available = append(global.Models.Available, config.ModelConfig{Name: "small", Provider: "ollama", Model: "qwen3.5:4b"})

	eff, warnings := Apply(global, project(t, `
version: 1
delegation:
  budgets:
    max_tool_calls: 4
    timeout_seconds: 30
  policies:
    - category: extraction
      enabled: false
    - category: summarization
      model: small
      tools: [read_file, git_diff]
      budgets: {max_iterations: 3}
  tools:
    allowed: [read_file, search_code, list_files, git_diff]
    denied: [git_diff]
    timeout_seconds: 5
    max_result_bytes: 4000
`))
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}
	b := eff.Delegation.Budgets
	if b.MaxToolCalls != 4 || b.TimeoutSeconds != 30 || b.MaxIterations != 5 {
		t.Errorf("budgets = %+v", b)
	}
	if got := policyFor(eff, "code_analysis").Budgets.MaxToolCalls; got != 4 {
		t.Errorf("project budget must cap a larger global category budget, got %d", got)
	}
	if policyFor(eff, "extraction").Enabled {
		t.Error("extraction should be disabled")
	}
	sum := policyFor(eff, "summarization")
	if sum.Model != "small" || !slices.Equal(*sum.Tools, []string{"read_file", "git_diff"}) || sum.Budgets.MaxIterations != 3 {
		t.Errorf("summarization = %+v", sum)
	}
	tp := eff.Delegation.Tools
	if !slices.Equal(tp.Allowed, []string{"read_file", "search_code", "list_files", "git_diff"}) || !slices.Contains(tp.Denied, "git_diff") ||
		tp.TimeoutSeconds != 5 || tp.MaxResultBytes != 4000 {
		t.Errorf("tools = %+v", tp)
	}

	// The global configuration is untouched.
	if global.Delegation.Budgets.MaxToolCalls != 8 || !policyFor(global, "extraction").Enabled || policyFor(global, "summarization").Tools != nil {
		t.Error("Apply modified the global configuration")
	}

	// Through the engine: denied git_diff is gone even where the project
	// listed it for summarization.
	e := engine(t, eff, func(o *Options) { o.Enabled = []string{"read_file", "search_code", "list_files"} })
	if d := e.Evaluate(Request{Category: "summarization"}); !slices.Equal(d.Tools, []string{"read_file"}) || d.Budgets.MaxToolCalls != 4 {
		t.Errorf("summarization decision = %+v", d)
	}
}

// A project file that tries to widen anything is ignored for those settings,
// with a warning for each, and the result is never wider than global.
func TestApplyIgnoresWidening(t *testing.T) {
	global := config.Default()
	policyFor(global, "log_analysis").Tools = &[]string{"read_file"}
	global.Delegation.Tools.Allowed = []string{"read_file", "search_code", "list_files"}

	eff, warnings := Apply(global, project(t, `
delegation:
  defaults: {enabled: true}
  budgets:
    max_tool_calls: 50
    max_input_tokens: -1
  policies:
    - category: architecture
      enabled: true
    - category: log_analysis
      tools: [read_file, search_code, git_log]
      budgets: {timeout_seconds: 600}
    - category: code_analysis
      model: gpt-cloud
    - category: poetry
      enabled: true
  tools:
    allowed: [read_file, git_diff, shell]
    timeout_seconds: 3600
    max_result_bytes: 999999999
`))
	joined := strings.Join(warnings, "\n")
	for _, want := range []string{
		"max_tool_calls: 50 ignored",
		"max_input_tokens: -1 ignored",
		"architecture].enabled: cannot enable",
		`log_analysis].tools: "search_code" is not permitted`,
		`log_analysis].tools: "git_log" is not permitted`,
		"log_analysis].budgets.timeout_seconds: 600 ignored",
		`"gpt-cloud" is not a globally configured model`,
		`category "poetry" is not configured globally`,
		`tools.allowed: "git_diff" is not allowed globally`,
		`tools.allowed: "shell" is not allowed globally`,
		"tools.timeout_seconds: 3600 ignored",
		"tools.max_result_bytes: 999999999 ignored",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing warning %q in:\n%s", want, joined)
		}
	}

	assertNotWider(t, global, eff)
}

// assertNotWider checks the property behind project configuration: for every
// category the effective policy enables, permits and allows no more than the
// global one.
func assertNotWider(t *testing.T, global, eff *config.Config) {
	t.Helper()
	g, p := engine(t, global, nil), engine(t, eff, nil)
	gRules := map[string]Rule{}
	for _, r := range g.Rules() {
		gRules[r.Category] = r
	}
	for _, r := range p.Rules() {
		gr := gRules[r.Category]
		if r.Enabled && !gr.Enabled {
			t.Errorf("%s: enabled by project", r.Category)
		}
		for _, tool := range r.Tools {
			if !slices.Contains(gr.Tools, tool) {
				t.Errorf("%s: tool %s added by project", r.Category, tool)
			}
		}
		if r.MaxToolCalls > gr.MaxToolCalls || r.MaxIterations > gr.MaxIterations || r.MaxInputTokens > gr.MaxInputTokens ||
			r.MaxOutput > gr.MaxOutput || r.TimeoutSeconds > gr.TimeoutSeconds {
			t.Errorf("%s: budgets widened: %+v > %+v", r.Category, r, gr)
		}
	}
	if eff.Providers != global.Providers || !reflect.DeepEqual(eff.Models, global.Models) || eff.Logging != global.Logging {
		t.Error("project configuration changed providers, models or logging")
	}
	if eff.Delegation.Tools.TimeoutSeconds > global.Delegation.Tools.TimeoutSeconds ||
		eff.Delegation.Tools.MaxResultBytes > global.Delegation.Tools.MaxResultBytes {
		t.Error("tool limits widened")
	}
}

func TestApplyCannotReenableDelegation(t *testing.T) {
	global := config.Default()
	global.Delegation.Defaults.Enabled = false
	eff, warnings := Apply(global, project(t, "delegation:\n  defaults: {enabled: true}\n"))
	if eff.Delegation.Defaults.Enabled || len(warnings) != 1 {
		t.Errorf("enabled=%v warnings=%v", eff.Delegation.Defaults.Enabled, warnings)
	}

	eff, _ = Apply(config.Default(), project(t, "delegation:\n  defaults: {enabled: false}\n"))
	if len(engine(t, eff, nil).EnabledCategories()) != 0 {
		t.Error("a project must be able to disable delegation")
	}
}

func TestLoadProject(t *testing.T) {
	newWS := func(files map[string]string) *workspace.Root {
		dir := t.TempDir()
		for name, content := range files {
			p := filepath.Join(dir, name)
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		ws, err := workspace.New(dir, 0)
		if err != nil {
			t.Fatal(err)
		}
		return ws
	}

	if p, err := LoadProject(newWS(nil)); p != nil || err != nil {
		t.Errorf("no project file: %v, %v", p, err)
	}
	p, err := LoadProject(newWS(map[string]string{ProjectConfigPath: "delegation:\n  budgets: {max_tool_calls: 2}\n"}))
	if err != nil || p.Delegation.Budgets.MaxToolCalls != 2 {
		t.Errorf("valid file: %+v, %v", p, err)
	}
	if _, err := LoadProject(newWS(map[string]string{ProjectConfigPath: "delegation: [\n"})); err == nil {
		t.Error("malformed file must be an error")
	}

	// A symlink out of the workspace is refused, not followed.
	outside := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(outside, []byte("version: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ws := newWS(map[string]string{".localpilot/keep": ""})
	if err := os.Symlink(outside, filepath.Join(ws.Path(), ProjectConfigPath)); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	if _, err := LoadProject(ws); !errors.Is(err, workspace.ErrOutside) {
		t.Errorf("symlinked project config: err = %v", err)
	}
}

// configs/project.example.yaml is documentation; keep it valid and narrowing.
func TestProjectExampleIsValid(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "configs", "project.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	p, err := ParseProject(data)
	if err != nil {
		t.Fatal(err)
	}
	eff, warnings := Apply(config.Default(), p)
	if len(warnings) != 0 {
		t.Errorf("the example must not produce warnings: %v", warnings)
	}
	assertNotWider(t, config.Default(), eff)
}
