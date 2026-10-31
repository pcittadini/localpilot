package tools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"

	"localpilot/internal/taskctx"
	"localpilot/internal/workspace"
)

const (
	// maxReadLines bounds one read_file call.
	maxReadLines = 150
	// maxListEntries bounds list_files output.
	maxListEntries = 300
	// maxEnumeratedFiles bounds how many files list and search consider.
	maxEnumeratedFiles = 20000
)

// toolset holds what the tool implementations share.
type toolset struct {
	ws       *workspace.Root
	git      *gitRunner // nil when git is unavailable
	maxBytes int
}

type readArgs struct {
	Path      string `json:"path" jsonschema:"workspace-relative file path"`
	LineStart int    `json:"line_start,omitempty" jsonschema:"first line to read, 1-based (default 1)"`
	LineEnd   int    `json:"line_end,omitempty" jsonschema:"last line to read, inclusive (default: 150 lines from line_start)"`
}

func (s *toolset) readFile() Tool {
	return define("read_file",
		"Read a text file from the workspace with line numbers. Reads at most 150 lines per call; use line_start and line_end to read a specific part.",
		func(sc *jsonschema.Schema) {
			strLength(sc, "path", 1, 4096)
			intRange(sc, "line_start", 1, 1e7)
			intRange(sc, "line_end", 1, 1e7)
		},
		func(a readArgs) string {
			if a.LineStart > 0 || a.LineEnd > 0 {
				return fmt.Sprintf("%s:%d-%d", a.Path, a.LineStart, a.LineEnd)
			}
			return a.Path
		},
		func(_ context.Context, a readArgs) (Output, error) {
			data, rel, err := s.ws.ReadFile(a.Path)
			if err != nil {
				return Output{}, err
			}
			lines := taskctx.SplitLines(string(data))
			total := len(lines)
			if total == 0 {
				return Output{Text: fmt.Sprintf("<file path=%q total_lines=\"0\">\n(empty file)\n</file>", rel)}, nil
			}

			start := max(a.LineStart, 1)
			if start > total {
				return Output{}, fmt.Errorf("line_start %d is past the end of %s (%d lines)", start, rel, total)
			}
			end := a.LineEnd
			if end == 0 || end > start+maxReadLines-1 {
				end = start + maxReadLines - 1
			}
			end = min(end, total)
			if end < start {
				return Output{}, fmt.Errorf("line_end %d is before line_start %d", end, start)
			}

			// Stay inside the output budget so every line reported as
			// viewed was really shown.
			body := taskctx.NumberLines(lines, start, end)
			for len(body) > s.maxBytes-200 && end > start {
				end = start + (end-start)/2
				body = taskctx.NumberLines(lines, start, end)
			}

			var b strings.Builder
			fmt.Fprintf(&b, "<file path=%q lines=\"%d-%d\" total_lines=\"%d\">\n%s</file>\n", rel, start, end, total, body)
			if end < total {
				fmt.Fprintf(&b, "[showing lines %d-%d of %d; call read_file with line_start=%d to continue]\n", start, end, total, end+1)
			}
			return Output{Text: b.String(), Viewed: []Span{{File: rel, Start: start, End: end}}}, nil
		})
}

type listArgs struct {
	Path  string `json:"path,omitempty" jsonschema:"directory to list, workspace-relative (default: the workspace root)"`
	Depth int    `json:"depth,omitempty" jsonschema:"directory levels to expand (1-5, default 2); deeper directories are summarized with a file count"`
}

func (s *toolset) listFilesTool() Tool {
	return define("list_files",
		"List files in a workspace directory. Ignored files (per .gitignore) and sensitive files are excluded.",
		func(sc *jsonschema.Schema) {
			strLength(sc, "path", 1, 4096)
			intRange(sc, "depth", 1, 5)
		},
		func(a listArgs) string { return orDot(a.Path) },
		func(ctx context.Context, a listArgs) (Output, error) {
			dir, err := s.dir(orDot(a.Path))
			if err != nil {
				return Output{}, err
			}
			depth := a.Depth
			if depth == 0 {
				depth = 2
			}
			files, err := s.files(ctx, dir)
			if err != nil {
				return Output{}, err
			}
			if len(files) == 0 {
				return Output{Text: fmt.Sprintf("No files under %s (ignored and sensitive files are excluded).\n", dir)}, nil
			}

			dirCounts := map[string]int{}
			var entries []string
			for _, f := range files {
				rest := f
				if dir != "." {
					rest = strings.TrimPrefix(f, dir+"/")
				}
				parts := strings.Split(rest, "/")
				if len(parts) <= depth {
					entries = append(entries, f)
					continue
				}
				d := strings.Join(parts[:depth], "/") + "/"
				if dir != "." {
					d = dir + "/" + d
				}
				if dirCounts[d] == 0 {
					entries = append(entries, d)
				}
				dirCounts[d]++
			}
			slices.Sort(entries)

			var b strings.Builder
			fmt.Fprintf(&b, "%d files under %s:\n", len(files), dir)
			shown := 0
			for _, e := range entries {
				line := e
				if n := dirCounts[e]; n > 0 {
					line = fmt.Sprintf("%s (%d files)", e, n)
				}
				if shown == maxListEntries || b.Len()+len(line) > s.maxBytes-100 {
					fmt.Fprintf(&b, "[%d more entries not shown; list a subdirectory]\n", len(entries)-shown)
					break
				}
				b.WriteString(line + "\n")
				shown++
			}
			return Output{Text: b.String()}, nil
		})
}

// dir validates that p is a directory inside the workspace and returns its
// workspace-relative form.
func (s *toolset) dir(p string) (string, error) {
	abs, rel, err := s.ws.Resolve(p)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%s is not a directory", rel)
	}
	return rel, nil
}

// files returns the workspace-relative paths of non-ignored, non-sensitive
// regular files under dir, sorted. Git's ignore rules apply when the
// workspace is a git repository.
func (s *toolset) files(ctx context.Context, dir string) ([]string, error) {
	var paths []string
	if s.git != nil {
		out, err := s.git.run(ctx, 64<<20, "ls-files", "-z", "--cached", "--others", "--exclude-standard", "--", dir)
		if err != nil {
			return nil, err
		}
		for p := range bytes.SplitSeq(out, []byte{0}) {
			if len(p) > 0 {
				paths = append(paths, string(p))
			}
		}
	} else {
		root := filepath.Join(s.ws.Path(), filepath.FromSlash(dir))
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if d.IsDir() {
				if path != root && skipDir(d.Name()) {
					return filepath.SkipDir
				}
				return nil
			}
			rel, err := filepath.Rel(s.ws.Path(), path)
			if err != nil {
				return err
			}
			paths = append(paths, filepath.ToSlash(rel))
			if len(paths) > maxEnumeratedFiles {
				return errTooManyFiles
			}
			return nil
		})
		if err != nil && !errors.Is(err, errTooManyFiles) {
			return nil, err
		}
	}

	slices.Sort(paths)
	paths = slices.Compact(paths)
	out := paths[:0]
	for _, p := range paths {
		if workspace.IsSensitive(p) {
			continue
		}
		// git lists tracked files that were deleted; skip anything that is
		// not a regular file on disk.
		info, err := os.Lstat(filepath.Join(s.ws.Path(), filepath.FromSlash(p)))
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		out = append(out, p)
		if len(out) == maxEnumeratedFiles {
			break
		}
	}
	return out, nil
}

var errTooManyFiles = errors.New("too many files")

// skipDir reports directories skipped when no git ignore rules are available.
func skipDir(name string) bool {
	if strings.HasPrefix(name, ".") && name != ".github" {
		return true
	}
	switch name {
	case "node_modules", "__pycache__", "venv":
		return true
	}
	return false
}

func orDot(p string) string {
	if p == "" {
		return "."
	}
	return p
}
