// Copyright 2026 The LocalPilot Authors
// SPDX-License-Identifier: Apache-2.0

package integration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/pcittadini/localpilot/internal/agent"
)

// syncBuffer collects the server's stderr safely across goroutines.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// startServer builds the binary and connects to it over stdio, the way
// Claude Code does.
func startServer(t *testing.T, workspace string) (*mcpsdk.ClientSession, *syncBuffer) {
	t.Helper()
	url, model := liveOllama(t)
	dir := t.TempDir()

	bin := filepath.Join(dir, "localpilot")
	build := exec.Command("go", "build", "-o", bin, "./cmd/localpilot")
	build.Dir = filepath.Join("..", "..")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}

	// An explicit config keeps the test independent of the user's global file.
	cfgPath := filepath.Join(dir, "config.yaml")
	cfg := fmt.Sprintf(`version: 1
providers:
  ollama:
    base_url: %s
models:
  default: test-model
  available:
    - name: test-model
      provider: ollama
      model: %s
      temperature: 0.1
      max_tokens: 2048
      context_window: 8192
      think: false
logging:
  level: info
`, url, model)
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(bin, "serve", "--workspace", workspace)
	cmd.Env = append(os.Environ(), "LOCALPILOT_CONFIG="+cfgPath)
	stderr := &syncBuffer{}
	cmd.Stderr = stderr

	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "e2e", Version: "1"}, nil)
	cs, err := client.Connect(context.Background(), &mcpsdk.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatalf("connect: %v\nstderr:\n%s", err, stderr.String())
	}
	t.Cleanup(func() { cs.Close() })
	return cs, stderr
}

func hashTree(t *testing.T, root string) string {
	t.Helper()
	h := sha256.New()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		fmt.Fprintf(h, "%s\x00%x\x00", path, sha256.Sum256(data))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

func call(t *testing.T, cs *mcpsdk.ClientSession, tool string, args map[string]any) agent.Result {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	res, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	if res.IsError {
		t.Fatalf("%s returned a tool error: %v", tool, res.Content)
	}
	data, _ := json.Marshal(res.StructuredContent)
	var out agent.Result
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	pretty, _ := json.MarshalIndent(out, "", "  ")
	t.Logf("%s result:\n%s", tool, pretty)
	return out
}

func TestEndToEndDuplicatedAuthLogic(t *testing.T) {
	fixture, err := filepath.Abs(filepath.Join("testdata", "sampleapp"))
	if err != nil {
		t.Fatal(err)
	}
	before := hashTree(t, fixture)
	cs, stderr := startServer(t, fixture)

	tools, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools.Tools) != 2 {
		t.Fatalf("tools = %d", len(tools.Tools))
	}

	res := call(t, cs, "delegate_task", map[string]any{
		"category":  "code_analysis",
		"objective": "Identify duplicated authentication logic",
		"context": map[string]any{
			"files": []any{"internal/handlers/users.go", "internal/handlers/orders.go"},
		},
		"constraints":   []any{"Read only", "Do not modify files", "Provide source evidence"},
		"output_format": "json",
	})

	if res.Status != agent.StatusCompleted {
		t.Fatalf("status = %s, escalation = %+v", res.Status, res.Escalation)
	}
	if !res.Validation.SchemaValid || !res.Validation.EvidenceValid {
		t.Errorf("validation = %+v", res.Validation)
	}
	if res.Result == nil || res.Result.Summary == "" || len(res.Result.Findings) == 0 {
		t.Fatalf("result = %+v", res.Result)
	}
	if !strings.Contains(strings.ToLower(res.Result.Summary), "auth") {
		t.Errorf("summary does not mention authentication: %q", res.Result.Summary)
	}

	// The duplicated block is lines 16-29 in both handlers.
	overlaps := 0
	for _, ev := range res.Evidence {
		if !ev.Valid {
			t.Errorf("invalid evidence: %+v", ev)
		}
		if strings.HasPrefix(ev.File, "internal/handlers/") && ev.LineStart <= 29 && ev.LineEnd >= 16 {
			overlaps++
		}
	}
	if overlaps == 0 {
		t.Errorf("no evidence points at the duplicated block (lines 16-29): %+v", res.Evidence)
	}
	if res.Metrics.InputTokens == 0 || res.Metrics.OutputTokens == 0 || res.Metrics.DurationMS == 0 {
		t.Errorf("metrics = %+v", res.Metrics)
	}

	status := call(t, cs, "get_delegation_status", map[string]any{"task_id": res.TaskID})
	if status.TaskID != res.TaskID || status.Status != agent.StatusCompleted {
		t.Errorf("status = %+v", status)
	}

	if after := hashTree(t, fixture); after != before {
		t.Error("the delegated task modified the workspace")
	}
	if logs := stderr.String(); !strings.Contains(logs, `"msg":"task finished"`) {
		t.Errorf("expected JSON task logs on stderr, got:\n%s", logs)
	}
}

func TestEndToEndOtherCategories(t *testing.T) {
	fixture, _ := filepath.Abs(filepath.Join("testdata", "sampleapp"))
	cs, _ := startServer(t, fixture)

	t.Run("log analysis from excerpts", func(t *testing.T) {
		res := call(t, cs, "delegate_task", map[string]any{
			"category":  "log_analysis",
			"objective": "Which error occurs most often, and how many times?",
			"context": map[string]any{"excerpts": []any{map[string]any{"label": "api.log", "content": strings.Join([]string{
				"12:00:01 INFO  request ok path=/users/1",
				"12:00:02 ERROR db timeout after 5s path=/orders",
				"12:00:03 WARN  slow request 900ms path=/users/2",
				"12:00:04 ERROR db timeout after 5s path=/orders",
				"12:00:05 ERROR invalid token path=/users/3",
				"12:00:06 ERROR db timeout after 5s path=/orders",
			}, "\n")}}},
		})
		if res.Status != agent.StatusCompleted {
			t.Fatalf("status = %s, escalation = %+v", res.Status, res.Escalation)
		}
		summary := strings.ToLower(res.Result.Summary)
		if !strings.Contains(summary, "timeout") || !(strings.Contains(summary, "3") || strings.Contains(summary, "three")) {
			t.Errorf("summary = %q", res.Result.Summary)
		}
	})

	t.Run("documentation summary as text", func(t *testing.T) {
		res := call(t, cs, "delegate_task", map[string]any{
			"category":      "documentation",
			"objective":     "List the HTTP endpoints this README documents.",
			"context":       map[string]any{"files": []any{"README.md"}},
			"output_format": "text",
		})
		if res.Status != agent.StatusCompleted {
			t.Fatalf("status = %s, escalation = %+v", res.Status, res.Escalation)
		}
		if !strings.Contains(res.Result.Summary, "/users") || !strings.Contains(res.Result.Summary, "/orders") {
			t.Errorf("summary = %q", res.Result.Summary)
		}
	})

	t.Run("workspace escape is rejected", func(t *testing.T) {
		res := call(t, cs, "delegate_task", map[string]any{
			"category":  "summarization",
			"objective": "Summarize",
			"context":   map[string]any{"files": []any{"../../../../go.mod"}},
		})
		if res.Status != agent.StatusRejected || res.Escalation.Reason != agent.ReasonContextInvalid {
			t.Fatalf("result = %+v", res)
		}
	})

	t.Run("disabled category is refused", func(t *testing.T) {
		ctx := context.Background()
		r, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{Name: "delegate_task", Arguments: map[string]any{
			"category": "security_review", "objective": "Audit auth",
		}})
		if err == nil && !r.IsError {
			t.Fatalf("security_review must be refused, got %+v", r)
		}
	})
}

// gitFixture copies the sample app into a fresh git repository with one
// commit, so git tools have history that does not depend on this repo.
func gitFixture(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	src, _ := filepath.Abs(filepath.Join("testdata", "sampleapp"))
	dst := t.TempDir()
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-c", "user.name=Sample Dev", "-c", "user.email=dev@example.com",
			"-c", "commit.gpgsign=false", "-c", "init.defaultBranch=main"}, args...)...)
		cmd.Dir = dst
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q")
	git("add", "-A")
	git("commit", "-q", "-m", "Add users and orders handlers")
	return dst
}

func toolNames(trace []agent.ToolUse) []string {
	var names []string
	for _, u := range trace {
		names = append(names, u.Tool+":"+u.Status)
	}
	return names
}

// Phase 2 acceptance: with no files given, the local model inspects a Go
// package on its own, using several tools, and returns verified evidence.
func TestEndToEndAgentInspectsPackage(t *testing.T) {
	ws := gitFixture(t)
	cs, _ := startServer(t, ws)

	res := call(t, cs, "delegate_task", map[string]any{
		"category":    "code_analysis",
		"objective":   "Inspect the Go package in internal/handlers. List every exported function, with the file and line where it is defined.",
		"constraints": []any{"Provide source evidence"},
	})
	if res.Status != agent.StatusCompleted {
		t.Fatalf("status = %s, escalation = %+v, trace = %v", res.Status, res.Escalation, toolNames(res.Trace))
	}
	if len(res.Trace) < 2 || res.Metrics.ToolCalls < 2 || res.Metrics.Iterations < 2 {
		t.Errorf("expected several tool calls, got trace %v, metrics %+v", toolNames(res.Trace), res.Metrics)
	}
	read := false
	for _, u := range res.Trace {
		if u.Status == "ok" && (u.Tool == "read_file" || u.Tool == "search_code") {
			read = true
		}
	}
	if !read {
		t.Errorf("the model never read code: %v", toolNames(res.Trace))
	}

	text := res.Result.Summary
	for _, f := range res.Result.Findings {
		text += " " + f.Description
	}
	for _, fn := range []string{"GetUser", "CreateOrder"} {
		if !strings.Contains(text, fn) {
			t.Errorf("result does not mention %s: %q", fn, text)
		}
	}
	// Both functions are defined on line 15 of their files.
	defs := map[string]bool{}
	for _, ev := range res.Evidence {
		if !ev.Valid {
			t.Errorf("invalid evidence: %+v", ev)
		}
		if ev.LineStart <= 15 && ev.LineEnd >= 15 {
			defs[ev.File] = true
		}
	}
	if !defs["internal/handlers/users.go"] && !defs["internal/handlers/orders.go"] {
		t.Errorf("no evidence points at a function definition (line 15): %+v", res.Evidence)
	}
}

// The model uses git_diff and git_log to describe uncommitted work.
func TestEndToEndAgentUsesGitTools(t *testing.T) {
	ws := gitFixture(t)
	orders := filepath.Join(ws, "internal", "handlers", "orders.go")
	data, _ := os.ReadFile(orders)
	changed := strings.Replace(string(data), "\tvar o Order\n", "\tvar o Order\n\tif r.ContentLength > 1<<20 {\n\t\thttp.Error(w, \"body too large\", http.StatusRequestEntityTooLarge)\n\t\treturn\n\t}\n", 1)
	if err := os.WriteFile(orders, []byte(changed), 0o644); err != nil {
		t.Fatal(err)
	}
	cs, _ := startServer(t, ws)

	res := call(t, cs, "delegate_task", map[string]any{
		"category":  "summarization",
		"objective": "Summarize the uncommitted changes in this repository, and give the subject of the most recent commit.",
	})
	if res.Status != agent.StatusCompleted {
		t.Fatalf("status = %s, escalation = %+v, trace = %v", res.Status, res.Escalation, toolNames(res.Trace))
	}
	used := map[string]bool{}
	for _, u := range res.Trace {
		if u.Status == "ok" {
			used[u.Tool] = true
		}
	}
	if !used["git_diff"] || !used["git_log"] {
		t.Errorf("expected git_diff and git_log, got %v", toolNames(res.Trace))
	}
	summary := strings.ToLower(res.Result.Summary)
	if !strings.Contains(summary, "orders") && !strings.Contains(summary, "size") && !strings.Contains(summary, "large") {
		t.Errorf("summary does not describe the change: %q", res.Result.Summary)
	}
	if !strings.Contains(summary, "add users and orders handlers") {
		t.Errorf("summary does not name the last commit: %q", res.Result.Summary)
	}
}
