package tools

import (
	"context"
	"fmt"
	"os"
	"path"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/google/jsonschema-go/jsonschema"

	"localpilot/internal/taskctx"
)

const maxMatchLineChars = 200

type searchArgs struct {
	Query         string `json:"query" jsonschema:"text to search for; an RE2 regular expression when regex is true"`
	Regex         bool   `json:"regex,omitempty" jsonschema:"treat query as an RE2 regular expression (default: literal text)"`
	Path          string `json:"path,omitempty" jsonschema:"directory or file to search, workspace-relative (default: the whole workspace)"`
	Glob          string `json:"glob,omitempty" jsonschema:"only search files whose name matches this pattern, e.g. *.go"`
	CaseSensitive bool   `json:"case_sensitive,omitempty" jsonschema:"match case exactly (default: case-insensitive)"`
	MaxResults    int    `json:"max_results,omitempty" jsonschema:"maximum matching lines to return (1-100, default 30)"`
}

// searchCode searches file contents natively. The search is read-only and
// takes no command-line arguments, so the model cannot inject options; RE2
// regular expressions run in linear time.
func (s *toolset) searchCode() Tool {
	return define("search_code",
		"Search the workspace for text or a regular expression. Returns matching lines as path:line: text. Ignored, sensitive and binary files are skipped.",
		func(sc *jsonschema.Schema) {
			strLength(sc, "query", 1, 200)
			strLength(sc, "path", 1, 4096)
			strLength(sc, "glob", 1, 100)
			intRange(sc, "max_results", 1, 100)
		},
		func(a searchArgs) string {
			t := fmt.Sprintf("%q", a.Query)
			if a.Path != "" {
				t += " in " + a.Path
			}
			if a.Glob != "" {
				t += " (" + a.Glob + ")"
			}
			return t
		},
		func(ctx context.Context, a searchArgs) (Output, error) {
			match, err := matcher(a)
			if err != nil {
				return Output{}, err
			}
			if a.Glob != "" {
				if _, err := path.Match(a.Glob, ""); err != nil {
					return Output{}, fmt.Errorf("%w: bad glob %q", ErrInvalidArguments, a.Glob)
				}
			}
			limit := a.MaxResults
			if limit == 0 {
				limit = 30
			}

			files, err := s.searchTargets(ctx, orDot(a.Path))
			if err != nil {
				return Output{}, err
			}

			var b strings.Builder
			var viewed []Span
			matchedFiles := map[string]bool{}
			hits, more := 0, false
		scan:
			for _, f := range files {
				if ctx.Err() != nil {
					return Output{}, ctx.Err()
				}
				if a.Glob != "" {
					if ok, _ := path.Match(a.Glob, path.Base(f)); !ok {
						continue
					}
				}
				data, rel, err := s.ws.ReadFile(f)
				if err != nil {
					continue // binary, too large, or vanished: skip
				}
				for i, line := range taskctx.SplitLines(string(data)) {
					if !match(line) {
						continue
					}
					entry := fmt.Sprintf("%s:%d: %s\n", rel, i+1, clip(strings.TrimSpace(line), maxMatchLineChars))
					if hits == limit || b.Len()+len(entry) > s.maxBytes-200 {
						more = true
						break scan
					}
					b.WriteString(entry)
					viewed = append(viewed, Span{File: rel, Start: i + 1, End: i + 1})
					matchedFiles[rel] = true
					hits++
				}
			}

			if hits == 0 {
				return Output{Text: fmt.Sprintf("No matches for %q.\n", a.Query)}, nil
			}
			header := fmt.Sprintf("%d matches in %d files", hits, len(matchedFiles))
			if more {
				header += " (more matches exist; narrow the search with path or glob)"
			}
			return Output{Text: header + ":\n" + b.String(), Viewed: viewed}, nil
		})
}

// searchTargets returns the files to search for a path that may name a
// directory or a single file.
func (s *toolset) searchTargets(ctx context.Context, p string) ([]string, error) {
	abs, rel, err := s.ws.Resolve(p)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return []string{rel}, nil
	}
	return s.files(ctx, rel)
}

func matcher(a searchArgs) (func(string) bool, error) {
	if a.Regex {
		expr := a.Query
		if !a.CaseSensitive {
			expr = "(?i)" + expr
		}
		re, err := regexp.Compile(expr)
		if err != nil {
			return nil, fmt.Errorf("%w: bad regular expression: %v", ErrInvalidArguments, err)
		}
		return re.MatchString, nil
	}
	if a.CaseSensitive {
		return func(line string) bool { return strings.Contains(line, a.Query) }, nil
	}
	q := strings.ToLower(a.Query)
	return func(line string) bool { return strings.Contains(strings.ToLower(line), q) }, nil
}

// clip shortens s to at most n bytes without splitting a UTF-8 sequence.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}
