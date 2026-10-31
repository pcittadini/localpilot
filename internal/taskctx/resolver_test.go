package taskctx

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"localpilot/internal/workspace"
)

func newWorkspace(t *testing.T, files map[string]string) *workspace.Root {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ws, err := workspace.New(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	return ws
}

const sample = "package a\n\nfunc A() {}   \n\n\nfunc B() {}\n"

func TestResolveFilesNumbersLines(t *testing.T) {
	ws := newWorkspace(t, map[string]string{"a.go": sample})
	pkg, err := Resolve(ws, Request{Files: []string{"a.go", "./a.go"}}, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(pkg.Items) != 1 {
		t.Fatalf("duplicate file not collapsed: %d items", len(pkg.Items))
	}
	it := pkg.Items[0]
	if it.Path != "a.go" || it.LineStart != 1 || it.LineEnd != 6 || it.TotalLines != 6 {
		t.Fatalf("item = %+v", it)
	}
	// Blank lines dropped, numbers preserved, trailing spaces trimmed.
	want := "1| package a\n3| func A() {}\n6| func B() {}\n"
	if it.Text != want {
		t.Fatalf("text = %q, want %q", it.Text, want)
	}
	if !strings.Contains(pkg.Render(), `<file path="a.go" lines="1-6" total_lines="6">`) {
		t.Fatalf("render = %s", pkg.Render())
	}
	if pkg.EstimatedTokens <= 0 {
		t.Error("token estimate missing")
	}
}

func TestResolveRanges(t *testing.T) {
	lines := make([]string, 120)
	for i := range lines {
		lines[i] = "line"
	}
	ws := newWorkspace(t, map[string]string{"big.txt": strings.Join(lines, "\n") + "\n", "a.go": sample})

	pkg, err := Resolve(ws, Request{Ranges: []Range{{File: "big.txt", LineStart: 99, LineEnd: 101}, {File: "big.txt", LineStart: 118}}}, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if got := pkg.Items[0].Text; got != " 99| line\n100| line\n101| line\n" {
		t.Errorf("range text = %q", got)
	}
	if it := pkg.Items[1]; it.LineStart != 118 || it.LineEnd != 120 {
		t.Errorf("open-ended range = %d-%d", it.LineStart, it.LineEnd)
	}

	for _, r := range []Range{{File: "big.txt", LineStart: 0, LineEnd: 3}, {File: "big.txt", LineStart: 121}, {File: "big.txt", LineStart: 10, LineEnd: 5}} {
		if _, err := Resolve(ws, Request{Ranges: []Range{r}}, 1000); err == nil {
			t.Errorf("range %+v: expected error", r)
		}
	}

	// A range of a file already included whole is skipped.
	pkg, err = Resolve(ws, Request{Files: []string{"a.go"}, Ranges: []Range{{File: "a.go", LineStart: 1, LineEnd: 2}}}, 1000)
	if err != nil || len(pkg.Items) != 1 {
		t.Fatalf("items=%d err=%v", len(pkg.Items), err)
	}
}

func TestResolveExcerptsAndRedaction(t *testing.T) {
	ws := newWorkspace(t, map[string]string{"cfg.go": "const key = \"AKIAABCDEFGHIJKLMNOP\"\n"})
	pkg, err := Resolve(ws, Request{
		Files: []string{"cfg.go"},
		Excerpts: []Excerpt{
			{Label: "app.log", Content: "ERROR boom  \n\n\n\nERROR again\n"},
			{Content: "-----BEGIN RSA PRIVATE KEY-----\nMIIE\n-----END RSA PRIVATE KEY-----\n"},
		},
	}, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if pkg.Redactions != 2 {
		t.Errorf("redactions = %d, want 2", pkg.Redactions)
	}
	out := pkg.Render()
	for _, want := range []string{`<excerpt label="app.log" lines="1-5">` + "\n1| ERROR boom\n5| ERROR again\n", `<excerpt label="excerpt-2" lines="1-3">`, `[REDACTED]`} {
		if !strings.Contains(out, want) {
			t.Errorf("render missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "AKIA") || strings.Contains(out, "MIIE") {
		t.Errorf("secret leaked:\n%s", out)
	}
	if it, ok := pkg.Excerpt("app.log"); !ok || it.TotalLines != 5 {
		t.Errorf("Excerpt(app.log) = %+v, %v", it, ok)
	}
	if _, ok := pkg.Excerpt("cfg.go"); ok {
		t.Error("files must not be returned as excerpts")
	}

	dup := Request{Excerpts: []Excerpt{{Label: "x", Content: "a"}, {Label: "x", Content: "b"}}}
	if _, err := Resolve(ws, dup, 1000); err == nil {
		t.Error("duplicate excerpt labels must be rejected")
	}
}

func TestResolveErrors(t *testing.T) {
	ws := newWorkspace(t, map[string]string{"a.go": sample, ".env": "X=1\n"})
	if _, err := Resolve(ws, Request{Files: []string{"a.go"}}, 5); !errors.Is(err, ErrBudget) {
		t.Errorf("budget err = %v", err)
	}
	if _, err := Resolve(ws, Request{Files: []string{"../etc/passwd"}}, 1000); !errors.Is(err, workspace.ErrOutside) {
		t.Errorf("traversal err = %v", err)
	}
	if _, err := Resolve(ws, Request{Files: []string{".env"}}, 1000); !errors.Is(err, workspace.ErrSensitive) {
		t.Errorf("sensitive err = %v", err)
	}
	if _, err := Resolve(ws, Request{Files: []string{"missing.go"}}, 1000); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("missing err = %v", err)
	}
}

func TestLineCount(t *testing.T) {
	for in, want := range map[string]int{"": 0, "a": 1, "a\n": 1, "a\nb": 2, "a\r\nb\r\n": 2, "\n\n": 2} {
		if got := LineCount([]byte(in)); got != want {
			t.Errorf("LineCount(%q) = %d, want %d", in, got, want)
		}
	}
}
