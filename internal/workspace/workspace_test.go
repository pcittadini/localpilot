package workspace

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func setup(t *testing.T) (*Root, string, string) {
	t.Helper()
	base := t.TempDir()
	ws := filepath.Join(base, "ws")
	outside := filepath.Join(base, "outside")
	write(t, filepath.Join(ws, "main.go"), "package main\n")
	write(t, filepath.Join(ws, "pkg", "a.go"), "package pkg\n")
	write(t, filepath.Join(ws, ".env"), "TOKEN=x\n")
	write(t, filepath.Join(ws, ".env.example"), "TOKEN=\n")
	write(t, filepath.Join(outside, "data.txt"), "outside data\n")
	root, err := New(ws, 64)
	if err != nil {
		t.Fatal(err)
	}
	return root, root.Path(), outside
}

func TestResolve(t *testing.T) {
	root, ws, outside := setup(t)

	ok := map[string]string{
		"main.go":                     "main.go",
		"./pkg/../pkg/a.go":           "pkg/a.go",
		filepath.Join(ws, "pkg/a.go"): "pkg/a.go",
		".env.example":                ".env.example",
	}
	for in, want := range ok {
		_, rel, err := root.Resolve(in)
		if err != nil || rel != want {
			t.Errorf("Resolve(%q) = %q, %v; want %q", in, rel, err, want)
		}
	}

	bad := map[string]error{
		"":                                 ErrInvalid,
		"a\x00b":                           ErrInvalid,
		"../outside/data.txt":              ErrOutside,
		"pkg/../../outside/data.txt":       ErrOutside,
		filepath.Join(outside, "data.txt"): ErrOutside,
		"/etc/passwd":                      ErrOutside,
		".env":                             ErrSensitive,
		".git/config":                      ErrSensitive,
	}
	for in, want := range bad {
		if _, _, err := root.Resolve(in); !errors.Is(err, want) {
			t.Errorf("Resolve(%q) err = %v, want %v", in, err, want)
		}
	}

	if _, _, err := root.Resolve("missing.go"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("missing file err = %v", err)
	}
}

func TestSymlinks(t *testing.T) {
	root, ws, outside := setup(t)
	links := map[string]string{
		"escape.txt": filepath.Join(outside, "data.txt"),
		"escape-dir": outside,
		"inside.go":  filepath.Join(ws, "main.go"),
		"env-link":   filepath.Join(ws, ".env"),
	}
	for name, target := range links {
		if err := os.Symlink(target, filepath.Join(ws, name)); err != nil {
			t.Skipf("symlinks unsupported: %v", err)
		}
	}

	if _, _, err := root.Resolve("escape.txt"); !errors.Is(err, ErrOutside) {
		t.Errorf("file symlink escape: err = %v", err)
	}
	if _, _, err := root.Resolve("escape-dir/data.txt"); !errors.Is(err, ErrOutside) {
		t.Errorf("dir symlink escape: err = %v", err)
	}
	if _, _, err := root.Resolve("env-link"); !errors.Is(err, ErrSensitive) {
		t.Errorf("symlink to sensitive file: err = %v", err)
	}
	if _, rel, err := root.Resolve("inside.go"); err != nil || rel != "inside.go" {
		t.Errorf("internal symlink: rel=%q err=%v", rel, err)
	}
}

func TestReadFile(t *testing.T) {
	root, ws, _ := setup(t)

	data, rel, err := root.ReadFile("pkg/a.go")
	if err != nil || rel != "pkg/a.go" || string(data) != "package pkg\n" {
		t.Fatalf("ReadFile = %q, %q, %v", data, rel, err)
	}

	write(t, filepath.Join(ws, "big.txt"), strings.Repeat("x", 65))
	if _, _, err := root.ReadFile("big.txt"); !errors.Is(err, ErrTooLarge) {
		t.Errorf("big file err = %v", err)
	}
	write(t, filepath.Join(ws, "bin.dat"), "ab\x00cd")
	if _, _, err := root.ReadFile("bin.dat"); !errors.Is(err, ErrBinary) {
		t.Errorf("binary file err = %v", err)
	}
	if _, _, err := root.ReadFile("pkg"); !errors.Is(err, ErrNotFile) {
		t.Errorf("directory err = %v", err)
	}
}

func TestIsSensitive(t *testing.T) {
	sensitive := []string{
		".env", ".env.local", "config/.env.production", ".git/HEAD", "deploy/.ssh/known_hosts",
		"id_rsa", "id_ed25519.pub", "certs/server.pem", "tls.KEY", "secrets.yaml", "app.secret",
		"infra/terraform.tfstate", "prod.tfvars", ".npmrc", "credentials.json",
	}
	for _, p := range sensitive {
		if !IsSensitive(p) {
			t.Errorf("IsSensitive(%q) = false", p)
		}
	}
	allowed := []string{
		".env.example", "internal/auth/handler.go", "secret_manager.go", "docs/keys.md",
		"gitignore.go", "README.md", "pkg/environment.go",
	}
	for _, p := range allowed {
		if IsSensitive(p) {
			t.Errorf("IsSensitive(%q) = true", p)
		}
	}
}

func TestNewRejectsMissingRoot(t *testing.T) {
	if _, err := New(filepath.Join(t.TempDir(), "nope"), 0); err == nil {
		t.Fatal("expected error")
	}
}
