// Copyright 2026 The LocalPilot Authors
// SPDX-License-Identifier: Apache-2.0

// Package workspace confines file access to a single project directory.
//
// Every path that reaches the filesystem on behalf of a delegated task goes
// through Root. It rejects paths that escape the root (directly, via "..", or
// via symlinks), sensitive files such as keys and .env files, non-regular
// files, binaries, and files above a size limit. Access is read-only.
package workspace

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

var (
	ErrOutside   = errors.New("path is outside the workspace")
	ErrSensitive = errors.New("path matches a sensitive-file rule")
	ErrTooLarge  = errors.New("file exceeds the size limit")
	ErrNotFile   = errors.New("path is not a regular file")
	ErrBinary    = errors.New("file appears to be binary")
	ErrInvalid   = errors.New("invalid path")
)

// DefaultMaxFileSize bounds a single file read.
const DefaultMaxFileSize = 512 * 1024

type Root struct {
	path        string
	maxFileSize int64
}

// New returns a Root for dir. dir must exist; symlinks in it are resolved so
// containment checks compare real paths.
func New(dir string, maxFileSize int64) (*Root, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, fmt.Errorf("workspace root: %w", err)
	}
	info, err := os.Stat(real)
	if err != nil {
		return nil, fmt.Errorf("workspace root: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("workspace root %s is not a directory", real)
	}
	if maxFileSize <= 0 {
		maxFileSize = DefaultMaxFileSize
	}
	return &Root{path: real, maxFileSize: maxFileSize}, nil
}

// Path returns the absolute, symlink-free workspace root.
func (r *Root) Path() string { return r.path }

// Resolve maps a workspace-relative (or absolute, inside the workspace) path
// to its real location. It returns the real absolute path and the cleaned
// workspace-relative path that callers should report back.
func (r *Root) Resolve(p string) (abs, rel string, err error) {
	rel, err = r.Clean(p)
	if err != nil {
		return "", "", err
	}
	joined := filepath.Join(r.path, filepath.FromSlash(rel))

	real, err := filepath.EvalSymlinks(joined)
	if err != nil {
		return "", "", fmt.Errorf("resolve %s: %w", rel, err)
	}
	realRel, ok := r.relative(real)
	if !ok {
		return "", "", fmt.Errorf("%w: %s resolves outside the workspace", ErrOutside, rel)
	}
	if IsSensitive(realRel) {
		return "", "", fmt.Errorf("%w: %s", ErrSensitive, rel)
	}
	return real, rel, nil
}

// Clean validates a path lexically, without touching the filesystem, and
// returns it workspace-relative with forward slashes ("." for the root). Use
// it for paths that may not exist on disk, such as git pathspecs; use Resolve
// before reading anything.
func (r *Root) Clean(p string) (string, error) {
	if p == "" || strings.ContainsRune(p, 0) {
		return "", fmt.Errorf("%w: %q", ErrInvalid, p)
	}
	var joined string
	if filepath.IsAbs(p) {
		joined = filepath.Clean(p)
	} else {
		joined = filepath.Join(r.path, p)
	}
	rel, ok := r.relative(joined)
	if !ok {
		return "", fmt.Errorf("%w: %s", ErrOutside, p)
	}
	if IsSensitive(rel) {
		return "", fmt.Errorf("%w: %s", ErrSensitive, rel)
	}
	return filepath.ToSlash(rel), nil
}

// ReadFile reads a text file inside the workspace, enforcing all access rules.
// It returns the content and the cleaned workspace-relative path.
func (r *Root) ReadFile(p string) ([]byte, string, error) {
	abs, rel, err := r.Resolve(p)
	if err != nil {
		return nil, "", err
	}
	f, err := os.Open(abs)
	if err != nil {
		return nil, "", fmt.Errorf("open %s: %w", rel, err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil, "", err
	}
	if !info.Mode().IsRegular() {
		return nil, "", fmt.Errorf("%w: %s", ErrNotFile, rel)
	}
	if info.Size() > r.maxFileSize {
		return nil, "", fmt.Errorf("%w: %s is %d bytes (limit %d)", ErrTooLarge, rel, info.Size(), r.maxFileSize)
	}
	// Read through a limit in case the file grew after Stat.
	data, err := io.ReadAll(io.LimitReader(f, r.maxFileSize+1))
	if err != nil {
		return nil, "", fmt.Errorf("read %s: %w", rel, err)
	}
	if int64(len(data)) > r.maxFileSize {
		return nil, "", fmt.Errorf("%w: %s", ErrTooLarge, rel)
	}
	if looksBinary(data) {
		return nil, "", fmt.Errorf("%w: %s", ErrBinary, rel)
	}
	return data, rel, nil
}

func (r *Root) relative(abs string) (string, bool) {
	rel, err := filepath.Rel(r.path, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", false
	}
	return rel, true
}

func looksBinary(data []byte) bool {
	head := data
	if len(head) > 8000 {
		head = head[:8000]
	}
	return bytes.IndexByte(head, 0) >= 0
}

var sensitiveDirs = map[string]bool{
	".git": true, ".ssh": true, ".gnupg": true, ".aws": true, ".azure": true,
	".kube": true, ".docker": true, ".terraform": true,
}

var sensitiveNames = map[string]bool{
	".env": true, ".netrc": true, ".npmrc": true, ".pypirc": true, ".pgpass": true,
	".htpasswd": true, ".git-credentials": true, "credentials": true,
	"credentials.json": true, "id_rsa": true, "id_dsa": true, "id_ecdsa": true,
	"id_ed25519": true, "terraform.tfstate": true, "terraform.tfstate.backup": true,
}

var sensitiveExts = map[string]bool{
	".pem": true, ".key": true, ".p12": true, ".pfx": true, ".jks": true,
	".keystore": true, ".kdbx": true, ".tfvars": true, ".secret": true, ".secrets": true,
}

// envTemplates are .env variants meant to be committed.
var envTemplates = map[string]bool{".env.example": true, ".env.sample": true, ".env.template": true, ".env.dist": true}

// IsSensitive reports whether a workspace-relative path refers to a file that
// delegated tasks must never see.
func IsSensitive(rel string) bool {
	parts := strings.Split(filepath.ToSlash(rel), "/")
	for _, dir := range parts[:len(parts)-1] {
		if sensitiveDirs[strings.ToLower(dir)] {
			return true
		}
	}
	base := strings.ToLower(parts[len(parts)-1])
	switch {
	case sensitiveDirs[base], sensitiveNames[base], sensitiveExts[filepath.Ext(base)]:
		return true
	case strings.HasPrefix(base, ".env.") && !envTemplates[base]:
		return true
	case strings.HasPrefix(base, "id_rsa") || strings.HasPrefix(base, "id_ed25519") || strings.HasPrefix(base, "id_ecdsa"):
		return true
	case strings.HasPrefix(base, "secret.") || strings.HasPrefix(base, "secrets."):
		return true
	}
	return false
}
