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

	"localpilot/internal/agent"
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
