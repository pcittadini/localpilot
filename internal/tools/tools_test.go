package tools

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"localpilot/internal/workspace"
)

func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// gitCmd runs git for test setup, isolated from the user's configuration.
func gitCmd(t *testing.T, dir string, args ...string) string {
	t.Helper()
	full := append([]string{"-c", "user.name=Test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false", "-c", "init.defaultBranch=main"}, args...)
	cmd := exec.Command("git", full...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
}

var allTools = []string{"read_file", "search_code", "list_files", "git_diff", "git_log"}

func newRegistry(t *testing.T, dir string, mutate func(*Options)) *Registry {
	t.Helper()
	ws, err := workspace.New(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	opts := Options{Allowed: allTools, Timeout: 5 * time.Second, MaxResultBytes: 8000}
	if mutate != nil {
		mutate(&opts)
	}
	r, err := New(context.Background(), ws, opts)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func run(t *testing.T, r *Registry, name string, args string) Call {
	t.Helper()
	return r.Execute(context.Background(), r.Enabled(), name, json.RawMessage(args))
}

func ok(t *testing.T, c Call) string {
	t.Helper()
	if c.Status != CallOK {
		t.Fatalf("%s: status %s: %v\n%s", c.Tool, c.Status, c.Err, c.Content)
	}
	return c.Content
}

const handlerGo = `package auth

import "strings"

// Validate checks a bearer token.
func Validate(header string) bool {
	token := strings.TrimPrefix(header, "Bearer ")
	return len(token) >= 32
}
`

func plainWorkspace(t *testing.T) string {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"internal/auth/handler.go":      handlerGo,
		"internal/auth/handler_test.go": "package auth\n",
		"internal/store/db/conn.go":     "package db\n// TOKEN lookup\n",
		"README.md":                     "# Demo\nUses bearer tokens.\n",
		".env":                          "TOKEN=supersecret\n",
		"node_modules/x/index.js":       "token\n",
		".hidden/notes.txt":             "token\n",
		"img.bin":                       "token\x00\x01",
	})
	return dir
}

func TestRegistryPermissions(t *testing.T) {
	dir := plainWorkspace(t)
	ws, _ := workspace.New(dir, 0)

	if _, err := New(context.Background(), ws, Options{Allowed: []string{"read_file", "shell"}, Timeout: time.Second, MaxResultBytes: 8000}); err == nil {
		t.Fatal("unknown allowed tool must be a configuration error")
	}

	r := newRegistry(t, dir, func(o *Options) { o.Denied = []string{"search_code", "shell"} })
	// Not a git repo: git tools are unavailable; search_code is denied.
	if got := r.Enabled(); !slices.Equal(got, []string{"read_file", "list_files"}) {
		t.Fatalf("enabled = %v", got)
	}
	if r.Unavailable()["git_diff"] == "" {
		t.Error("git_diff should be reported unavailable outside a git repo")
	}

	for _, tc := range []struct{ name, permittedNote string }{
		{"shell", "never implemented"},
		{"search_code", "denied by configuration"},
		{"git_log", "unavailable"},
	} {
		c := run(t, r, tc.name, `{"query":"x"}`)
		if c.Status != CallDenied || !strings.Contains(c.Content, "not available") {
			t.Errorf("%s (%s): status=%s content=%q", tc.name, tc.permittedNote, c.Status, c.Content)
		}
	}

	// Enabled but not permitted for this task.
	c := r.Execute(context.Background(), []string{"list_files"}, "read_file", json.RawMessage(`{"path":"README.md"}`))
	if c.Status != CallDenied {
		t.Errorf("tool outside the task's permitted set ran: %+v", c)
	}

	defs := r.Definitions(r.Enabled())
	if len(defs) != 2 || defs[0].Name != "read_file" {
		t.Fatalf("definitions = %+v", defs)
	}
	var schema map[string]any
	if err := json.Unmarshal(defs[0].Parameters, &schema); err != nil || schema["additionalProperties"] != false {
		t.Errorf("read_file schema = %s", defs[0].Parameters)
	}
}

func TestArgumentValidation(t *testing.T) {
	r := newRegistry(t, plainWorkspace(t), nil)
	bad := map[string]string{
		"wrong type":    `{"path": 5}`,
		"unknown field": `{"path": "README.md", "mode": "w"}`,
		"missing path":  `{}`,
		"out of range":  `{"path": "README.md", "line_start": 0}`,
		"not an object": `["README.md"]`,
		"broken json":   `{"path": `,
	}
	for name, args := range bad {
		if c := run(t, r, "read_file", args); c.Status != CallInvalid {
			t.Errorf("%s: status = %s (%s)", name, c.Status, c.Content)
		}
	}
	if c := run(t, r, "search_code", `{"query":"a(","regex":true}`); c.Status != CallInvalid {
		t.Errorf("bad regex: status = %s", c.Status)
	}
	if c := run(t, r, "search_code", `{"query":"a","glob":"[x"}`); c.Status != CallInvalid {
		t.Errorf("bad glob: status = %s", c.Status)
	}
	if c := run(t, r, "list_files", `{"depth": 9}`); c.Status != CallInvalid {
		t.Errorf("depth out of range: status = %s", c.Status)
	}
}

func TestReadFile(t *testing.T) {
	dir := plainWorkspace(t)
	var long strings.Builder
	for i := 1; i <= 400; i++ {
		long.WriteString("line\n")
	}
	writeFiles(t, dir, map[string]string{"long.txt": long.String(), "empty.txt": "", "keys.go": `var k = "AKIAABCDEFGHIJKLMNOP"` + "\n"})
	r := newRegistry(t, dir, nil)

	c := run(t, r, "read_file", `{"path":"internal/auth/handler.go"}`)
	out := ok(t, c)
	if !strings.Contains(out, `lines="1-9" total_lines="9"`) || !strings.Contains(out, "7| \ttoken := strings.TrimPrefix") {
		t.Errorf("read_file output:\n%s", out)
	}
	if len(c.Viewed) != 1 || c.Viewed[0] != (Span{File: "internal/auth/handler.go", Start: 1, End: 9}) {
		t.Errorf("viewed = %+v", c.Viewed)
	}

	c = run(t, r, "read_file", `{"path":"./internal/auth/handler.go","line_start":6,"line_end":7}`)
	if out := ok(t, c); !strings.Contains(out, `lines="6-7"`) || strings.Contains(out, "8|") || c.Target != "./internal/auth/handler.go:6-7" {
		t.Errorf("range read: target=%q\n%s", c.Target, out)
	}

	c = run(t, r, "read_file", `{"path":"long.txt","line_start":100}`)
	if out := ok(t, c); !strings.Contains(out, `lines="100-249"`) || !strings.Contains(out, "line_start=250 to continue") {
		t.Errorf("capped read:\n%s", out)
	}

	if out := ok(t, run(t, r, "read_file", `{"path":"empty.txt"}`)); !strings.Contains(out, "(empty file)") {
		t.Errorf("empty file: %s", out)
	}
	c = run(t, r, "read_file", `{"path":"keys.go"}`)
	if out := ok(t, c); strings.Contains(out, "AKIA") || c.Redactions != 1 {
		t.Errorf("secret not redacted: %s", out)
	}

	for args, want := range map[string]string{
		`{"path":"long.txt","line_start":401}`:             "past the end",
		`{"path":"long.txt","line_start":10,"line_end":5}`: "before line_start",
		`{"path":".env"}`:           "sensitive",
		`{"path":"../outside.txt"}`: "outside the workspace",
		`{"path":"img.bin"}`:        "binary",
		`{"path":"internal"}`:       "not a regular file",
		`{"path":"missing.go"}`:     "no such file",
	} {
		c := run(t, r, "read_file", args)
		if c.Status != CallFailed || !strings.Contains(c.Content, want) {
			t.Errorf("%s: status=%s content=%q, want %q", args, c.Status, c.Content, want)
		}
		if strings.Contains(c.Content, dir) {
			t.Errorf("%s: error leaks the host path: %q", args, c.Content)
		}
	}
}

func TestReadFileRespectsByteBudget(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"wide.txt": strings.Repeat(strings.Repeat("x", 199)+"\n", 150)})
	r := newRegistry(t, dir, func(o *Options) { o.MaxResultBytes = 4000 })
	c := run(t, r, "read_file", `{"path":"wide.txt"}`)
	out := ok(t, c)
	if len(out) > 4000 || c.Viewed[0].End >= 150 || !strings.Contains(out, "to continue") {
		t.Fatalf("len=%d viewed=%+v", len(out), c.Viewed)
	}
	if !strings.Contains(out, "\n"+itoa(c.Viewed[0].End)+"| ") {
		t.Errorf("viewed end %d not in output", c.Viewed[0].End)
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

func TestListFilesWithoutGit(t *testing.T) {
	r := newRegistry(t, plainWorkspace(t), nil)
	out := ok(t, run(t, r, "list_files", `{}`))
	for _, want := range []string{"README.md\n", "internal/auth/ (2 files)", "internal/store/ (1 files)", "img.bin"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
	for _, banned := range []string{".env", "node_modules", ".hidden"} {
		if strings.Contains(out, banned) {
			t.Errorf("listed %q:\n%s", banned, out)
		}
	}

	out = ok(t, run(t, r, "list_files", `{"path":"internal","depth":2}`))
	if !strings.Contains(out, "internal/auth/handler.go") || !strings.Contains(out, "internal/store/db/ (1 files)") {
		t.Errorf("depth 2 under internal:\n%s", out)
	}
	out = ok(t, run(t, r, "list_files", `{"path":"internal","depth":3}`))
	if !strings.Contains(out, "internal/store/db/conn.go") {
		t.Errorf("depth 3 under internal:\n%s", out)
	}
	if c := run(t, r, "list_files", `{"path":"README.md"}`); c.Status != CallFailed {
		t.Errorf("listing a file: %s", c.Status)
	}
}

func TestSearchCodeWithoutGit(t *testing.T) {
	r := newRegistry(t, plainWorkspace(t), nil)

	c := run(t, r, "search_code", `{"query":"token"}`)
	out := ok(t, c)
	if !strings.Contains(out, "internal/auth/handler.go:7: token := strings.TrimPrefix") || !strings.Contains(out, "README.md:2:") {
		t.Errorf("search output:\n%s", out)
	}
	for _, banned := range []string{".env", "supersecret", "node_modules", ".hidden", "img.bin"} {
		if strings.Contains(out, banned) {
			t.Errorf("search leaked %q:\n%s", banned, out)
		}
	}
	if !slices.Contains(c.Viewed, Span{File: "internal/auth/handler.go", Start: 7, End: 7}) {
		t.Errorf("viewed = %+v", c.Viewed)
	}

	if out := ok(t, run(t, r, "search_code", `{"query":"TOKEN","case_sensitive":true}`)); !strings.Contains(out, "1 matches in 1 files") {
		t.Errorf("case-sensitive:\n%s", out)
	}
	if out := ok(t, run(t, r, "search_code", `{"query":"func \\w+\\(","regex":true,"glob":"*.go"}`)); !strings.Contains(out, "handler.go:6: func Validate(") {
		t.Errorf("regex:\n%s", out)
	}
	if out := ok(t, run(t, r, "search_code", `{"query":"token","glob":"*.md"}`)); strings.Contains(out, ".go:") {
		t.Errorf("glob not applied:\n%s", out)
	}
	if out := ok(t, run(t, r, "search_code", `{"query":"token","path":"internal/auth/handler.go"}`)); !strings.Contains(out, "in 1 files") {
		t.Errorf("file path:\n%s", out)
	}
	if out := ok(t, run(t, r, "search_code", `{"query":"e","max_results":2}`)); !strings.Contains(out, "2 matches") || !strings.Contains(out, "more matches exist") {
		t.Errorf("max_results:\n%s", out)
	}
	if out := ok(t, run(t, r, "search_code", `{"query":"no-such-text"}`)); !strings.Contains(out, "No matches") {
		t.Errorf("no match:\n%s", out)
	}
	if c := run(t, r, "search_code", `{"query":"x","path":"../"}`); c.Status != CallFailed {
		t.Errorf("search outside workspace: %s", c.Status)
	}
}

// gitWorkspace makes a repository with one commit, then leaves an unstaged
// edit, a staged edit, an ignored file and a change outside the workspace
// subdirectory.
func gitWorkspace(t *testing.T) (repo, ws string) {
	requireGit(t)
	repo = t.TempDir()
	writeFiles(t, repo, map[string]string{
		"app/internal/auth/handler.go": handlerGo,
		"app/README.md":                "# Demo\n",
		"app/.gitignore":               "build/\n",
		"app/.env":                     "TOKEN=old\n",
		"other/secret_plan.txt":        "outside the workspace\n",
	})
	gitCmd(t, repo, "init", "-q")
	gitCmd(t, repo, "add", "-A")
	gitCmd(t, repo, "commit", "-q", "-m", "Initial import")
	writeFiles(t, repo, map[string]string{"app/README.md": "# Demo\n\nNow with docs.\n"})
	gitCmd(t, repo, "add", "app/README.md")
	gitCmd(t, repo, "commit", "-q", "-m", "Document the demo")

	writeFiles(t, repo, map[string]string{
		"app/internal/auth/handler.go": strings.Replace(handlerGo, "return len(token) >= 32", "if token == \"\" {\n\t\treturn false\n\t}\n\treturn len(token) >= 32", 1),
		"app/.env":                     "TOKEN=new-secret\n",
		"app/build/out.txt":            "generated token\n",
		"app/notes.md":                 "untracked token notes\n",
		"other/secret_plan.txt":        "changed outside\n",
	})
	writeFiles(t, repo, map[string]string{"app/staged.go": "package app\n"})
	gitCmd(t, repo, "add", "app/staged.go")
	return repo, filepath.Join(repo, "app")
}

func TestGitDiff(t *testing.T) {
	_, ws := gitWorkspace(t)
	r := newRegistry(t, ws, nil)
	if !slices.Contains(r.Enabled(), "git_diff") {
		t.Fatalf("git tools not enabled: %v", r.Unavailable())
	}

	c := run(t, r, "git_diff", `{}`)
	out := ok(t, c)
	for _, want := range []string{"+++ b/internal/auth/handler.go", "+    8| \tif token == \"\" {", "    11| \treturn len(token)", "[diff of sensitive file .env omitted]"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
	for _, banned := range []string{"new-secret", "secret_plan", "staged.go"} {
		if strings.Contains(out, banned) {
			t.Errorf("unstaged diff contains %q:\n%s", banned, out)
		}
	}
	if !slices.ContainsFunc(c.Viewed, func(s Span) bool { return s.File == "internal/auth/handler.go" && s.Start <= 8 && s.End >= 11 }) {
		t.Errorf("viewed = %+v", c.Viewed)
	}

	if out := ok(t, run(t, r, "git_diff", `{"staged":true}`)); !strings.Contains(out, "+++ b/staged.go") || strings.Contains(out, "handler.go") {
		t.Errorf("staged diff:\n%s", out)
	}
	if out := ok(t, run(t, r, "git_diff", `{"path":"README.md"}`)); !strings.Contains(out, "No unstaged changes") {
		t.Errorf("path filter:\n%s", out)
	}
	if out := ok(t, run(t, r, "git_diff", `{"context_lines":0}`)); strings.Contains(out, "    11| \treturn len(token)") {
		t.Errorf("context_lines=0 still shows context:\n%s", out)
	}
	for _, args := range []string{`{"path":"../other"}`, `{"path":".env"}`} {
		if c := run(t, r, "git_diff", args); c.Status != CallFailed {
			t.Errorf("%s: status = %s", args, c.Status)
		}
	}
}

func TestGitLog(t *testing.T) {
	_, ws := gitWorkspace(t)
	r := newRegistry(t, ws, nil)

	out := ok(t, run(t, r, "git_log", `{"files":true}`))
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 {
		t.Fatalf("log:\n%s", out)
	}
	var latest commit
	if err := json.Unmarshal([]byte(lines[0]), &latest); err != nil {
		t.Fatal(err)
	}
	if latest.Subject != "Document the demo" || latest.Author != "Test" || !slices.Equal(latest.Files, []string{"README.md"}) {
		t.Errorf("latest = %+v", latest)
	}
	var first commit
	_ = json.Unmarshal([]byte(lines[1]), &first)
	if slices.Contains(first.Files, ".env") || !slices.Contains(first.Files, "(+1 more)") || slices.ContainsFunc(first.Files, func(f string) bool { return strings.Contains(f, "other/") }) {
		t.Errorf("first commit files = %v", first.Files)
	}
	if strings.Contains(out, "test@example.com") {
		t.Error("git_log must not expose author emails")
	}

	if out := ok(t, run(t, r, "git_log", `{"max_count":1,"path":"internal"}`)); !strings.Contains(out, "Initial import") || strings.Contains(out, "Document") {
		t.Errorf("path-filtered log:\n%s", out)
	}
	if out := ok(t, run(t, r, "git_log", `{"path":"deleted/long/ago.go"}`)); !strings.Contains(out, "No commits touch") {
		t.Errorf("missing path:\n%s", out)
	}
}

func TestGitIgnoreRulesApply(t *testing.T) {
	_, ws := gitWorkspace(t)
	r := newRegistry(t, ws, nil)
	list := ok(t, run(t, r, "list_files", `{"depth":5}`))
	if strings.Contains(list, "build/") || !strings.Contains(list, "notes.md") || strings.Contains(list, ".env\n") {
		t.Errorf("list_files ignores:\n%s", list)
	}
	search := ok(t, run(t, r, "search_code", `{"query":"token"}`))
	if strings.Contains(search, "build/out.txt") || !strings.Contains(search, "notes.md:1:") {
		t.Errorf("search_code ignores:\n%s", search)
	}
}

func TestGitEmptyRepository(t *testing.T) {
	requireGit(t)
	dir := t.TempDir()
	gitCmd(t, dir, "init", "-q")
	r := newRegistry(t, dir, nil)
	if out := ok(t, run(t, r, "git_log", `{}`)); !strings.Contains(out, "No commits yet") {
		t.Errorf("empty repo log: %s", out)
	}
}

// A repository's own configuration must not be able to make git run
// programs when the model calls git tools.
func TestGitCannotRunRepositoryConfiguredPrograms(t *testing.T) {
	repo, ws := gitWorkspace(t)
	marker := filepath.Join(t.TempDir(), "pwned")
	script := filepath.Join(t.TempDir(), "evil.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ntouch "+marker+"\necho fake\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, repo, "config", "core.fsmonitor", script)
	gitCmd(t, repo, "config", "diff.external", script)
	gitCmd(t, repo, "config", "diff.evil.textconv", script)
	gitCmd(t, repo, "config", "core.pager", script)
	gitCmd(t, repo, "config", "alias.diff", "!"+script)
	writeFiles(t, ws, map[string]string{".gitattributes": "*.go diff=evil\n"})

	r := newRegistry(t, ws, nil)
	for _, args := range []string{`{}`, `{"staged":true}`} {
		out := ok(t, run(t, r, "git_diff", args))
		if strings.Contains(out, "fake") {
			t.Errorf("external program output reached the diff:\n%s", out)
		}
	}
	ok(t, run(t, r, "git_log", `{"files":true}`))
	ok(t, run(t, r, "list_files", `{}`))
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a repository-configured program was executed")
	}
}

func TestToolTimeoutAndOutputCap(t *testing.T) {
	dir := t.TempDir()
	var b strings.Builder
	for i := 0; i < 3000; i++ {
		b.WriteString("needle line\n")
	}
	writeFiles(t, dir, map[string]string{"big.txt": b.String()})
	r := newRegistry(t, dir, func(o *Options) { o.MaxResultBytes = 1000 })
	c := run(t, r, "search_code", `{"query":"needle","max_results":100}`)
	if out := ok(t, c); len(out) > 1000 || !strings.Contains(out, "more matches exist") {
		t.Errorf("len=%d\n%s", len(out), out)
	}

	r = newRegistry(t, dir, func(o *Options) { o.Timeout = time.Nanosecond })
	if c := run(t, r, "search_code", `{"query":"needle"}`); c.Status != CallFailed || !strings.Contains(c.Content, "timed out") {
		t.Errorf("timeout: status=%s content=%q", c.Status, c.Content)
	}
}
