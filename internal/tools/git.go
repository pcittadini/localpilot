package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"

	"localpilot/internal/workspace"
)

// gitRunner executes a fixed set of read-only git subcommands. Arguments are
// built by the tools, never taken verbatim from the model.
type gitRunner struct {
	bin string
	dir string
}

// newGit returns a runner when git is installed and dir is inside a work
// tree, nil otherwise.
func newGit(ctx context.Context, dir string) *gitRunner {
	bin, err := exec.LookPath("git")
	if err != nil {
		return nil
	}
	g := &gitRunner{bin: bin, dir: dir}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := g.run(ctx, 1024, "rev-parse", "--is-inside-work-tree")
	if err != nil || strings.TrimSpace(string(out)) != "true" {
		return nil
	}
	return g
}

// run executes git with configuration that cannot trigger external programs
// or network access: global and system config are ignored, fsmonitor,
// external diff drivers and textconv are off, optional index writes are
// disabled, and pathspecs are literal.
func (g *gitRunner) run(ctx context.Context, maxBytes int, args ...string) ([]byte, error) {
	full := append([]string{
		"--no-pager", "--literal-pathspecs",
		"-c", "core.fsmonitor=false",
		"-c", "core.quotepath=false",
		"-c", "color.ui=never",
		"-c", "protocol.allow=never",
	}, args...)
	cmd := exec.CommandContext(ctx, g.bin, full...)
	cmd.Dir = g.dir
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=" + os.DevNull,
		"GIT_OPTIONAL_LOCKS=0",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_PAGER=cat",
		"LC_ALL=C",
	}
	stdout, stderr := &capWriter{max: maxBytes}, &capWriter{max: 4096}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		msg := strings.TrimSpace(stderr.buf.String())
		if i := strings.IndexByte(msg, '\n'); i > 0 {
			msg = msg[:i]
		}
		return nil, fmt.Errorf("git %s: %s", args[0], strings.TrimPrefix(msg, "fatal: "))
	}
	return stdout.buf.Bytes(), nil
}

// capWriter keeps at most max bytes and silently discards the rest.
type capWriter struct {
	buf       bytes.Buffer
	max       int
	truncated bool
}

func (w *capWriter) Write(p []byte) (int, error) {
	room := w.max - w.buf.Len()
	if len(p) > room {
		w.truncated = true
		if room > 0 {
			w.buf.Write(p[:room])
		}
		return len(p), nil
	}
	w.buf.Write(p)
	return len(p), nil
}

type diffArgs struct {
	Staged       bool   `json:"staged,omitempty" jsonschema:"show staged changes instead of unstaged ones"`
	Path         string `json:"path,omitempty" jsonschema:"limit the diff to this file or directory, workspace-relative"`
	ContextLines *int   `json:"context_lines,omitempty" jsonschema:"unchanged lines shown around each change (0-10, default 3)"`
}

func (s *toolset) gitDiff() Tool {
	return define("git_diff",
		"Show uncommitted changes in the workspace as a unified diff. Unchanged and added lines are prefixed with their current line number. Defaults to unstaged changes; set staged for staged ones.",
		func(sc *jsonschema.Schema) {
			strLength(sc, "path", 1, 4096)
			intRange(sc, "context_lines", 0, 10)
		},
		func(a diffArgs) string {
			t := "unstaged"
			if a.Staged {
				t = "staged"
			}
			if a.Path != "" {
				t += " " + a.Path
			}
			return t
		},
		func(ctx context.Context, a diffArgs) (Output, error) {
			spec, err := s.pathspec(a.Path)
			if err != nil {
				return Output{}, err
			}
			n := 3
			if a.ContextLines != nil {
				n = *a.ContextLines
			}
			args := []string{"diff", "--no-ext-diff", "--no-textconv", "--no-color", "--relative", "--unified=" + strconv.Itoa(n)}
			if a.Staged {
				args = append(args, "--cached")
			}
			out, err := s.git.run(ctx, 4*s.maxBytes, append(args, "--", spec)...)
			if err != nil {
				return Output{}, err
			}
			kind := "unstaged"
			if a.Staged {
				kind = "staged"
			}
			if len(bytes.TrimSpace(out)) == 0 {
				return Output{Text: fmt.Sprintf("No %s changes.\n", kind)}, nil
			}
			text, viewed := annotateDiff(string(out), s.maxBytes-200)
			return Output{Text: text, Viewed: viewed}, nil
		})
}

var hunkHeader = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)(?:,\d+)? @@`)

// annotateDiff drops sections for sensitive files, numbers the current-side
// lines of each hunk, and stops at budget bytes. It returns the lines shown
// as viewed spans.
func annotateDiff(diff string, budget int) (string, []Span) {
	var b strings.Builder
	var viewed []Span
	truncated := false

	for _, section := range splitDiffSections(diff) {
		if truncated {
			break
		}
		file := diffPath(section)
		if workspace.IsSensitive(file) {
			fmt.Fprintf(&b, "[diff of sensitive file %s omitted]\n", file)
			continue
		}
		newLine, inHunk := 0, false
		var span *Span
		for _, line := range section {
			var out string
			switch {
			case strings.HasPrefix(line, "@@"):
				m := hunkHeader.FindStringSubmatch(line)
				if m == nil {
					inHunk = false
					out = line
					break
				}
				newLine, _ = strconv.Atoi(m[1])
				inHunk = true
				span = nil
				out = line
			case inHunk && strings.HasPrefix(line, "+"):
				out = fmt.Sprintf("+%5d| %s", newLine, line[1:])
				span = extend(&viewed, span, file, newLine)
				newLine++
			case inHunk && strings.HasPrefix(line, " "):
				out = fmt.Sprintf(" %5d| %s", newLine, line[1:])
				span = extend(&viewed, span, file, newLine)
				newLine++
			case inHunk && strings.HasPrefix(line, "-"):
				out = fmt.Sprintf("-%5s| %s", "", line[1:])
			default:
				out = line
			}
			if b.Len()+len(out)+1 > budget {
				truncated = true
				break
			}
			b.WriteString(out)
			b.WriteByte('\n')
		}
	}
	if truncated {
		b.WriteString("[diff truncated; pass a path to see a single file]\n")
	}
	return b.String(), viewed
}

// extend grows the current hunk's viewed span to include line, starting a new
// span when needed.
func extend(viewed *[]Span, cur *Span, file string, line int) *Span {
	if cur != nil && cur.End == line-1 {
		cur.End = line
		return cur
	}
	*viewed = append(*viewed, Span{File: file, Start: line, End: line})
	return &(*viewed)[len(*viewed)-1]
}

func splitDiffSections(diff string) [][]string {
	var sections [][]string
	for _, line := range strings.Split(strings.TrimRight(diff, "\n"), "\n") {
		if strings.HasPrefix(line, "diff --git ") || len(sections) == 0 {
			sections = append(sections, nil)
		}
		sections[len(sections)-1] = append(sections[len(sections)-1], line)
	}
	return sections
}

// diffPath returns the file a diff section is about, preferring the new path.
func diffPath(section []string) string {
	var old string
	for _, line := range section {
		switch {
		case strings.HasPrefix(line, "+++ b/"):
			return strings.TrimPrefix(line, "+++ b/")
		case strings.HasPrefix(line, "--- a/"):
			old = strings.TrimPrefix(line, "--- a/")
		case strings.HasPrefix(line, "@@"):
			if old != "" {
				return old
			}
		}
	}
	if old != "" {
		return old
	}
	if len(section) > 0 {
		if i := strings.LastIndex(section[0], " b/"); i >= 0 {
			return section[0][i+3:]
		}
	}
	return ""
}

type logArgs struct {
	MaxCount int    `json:"max_count,omitempty" jsonschema:"number of commits to return (1-30, default 10)"`
	Path     string `json:"path,omitempty" jsonschema:"only commits touching this file or directory, workspace-relative"`
	Files    bool   `json:"files,omitempty" jsonschema:"include the files each commit changed"`
}

type commit struct {
	Commit  string   `json:"commit"`
	Author  string   `json:"author"`
	Date    string   `json:"date"`
	Subject string   `json:"subject"`
	Files   []string `json:"files,omitempty"`
}

const maxFilesPerCommit = 40

func (s *toolset) gitLog() Tool {
	return define("git_log",
		"List recent commits that touch the workspace, newest first, as JSON lines with commit, author, date and subject.",
		func(sc *jsonschema.Schema) {
			intRange(sc, "max_count", 1, 30)
			strLength(sc, "path", 1, 4096)
		},
		func(a logArgs) string {
			if a.Path != "" {
				return a.Path
			}
			return "."
		},
		func(ctx context.Context, a logArgs) (Output, error) {
			spec, err := s.pathspec(a.Path)
			if err != nil {
				return Output{}, err
			}
			n := a.MaxCount
			if n == 0 {
				n = 10
			}
			args := []string{"log", "-n", strconv.Itoa(n), "--no-color", "--no-show-signature",
				"--date=iso-strict", "--pretty=format:%x1e%h%x1f%an%x1f%ad%x1f%s"}
			if a.Files {
				args = append(args, "--name-only", "--relative")
			}
			out, err := s.git.run(ctx, 4*s.maxBytes, append(args, "--", spec)...)
			if err != nil {
				if strings.Contains(err.Error(), "does not have any commits") {
					return Output{Text: "No commits yet.\n"}, nil
				}
				return Output{}, err
			}

			var b strings.Builder
			for rec := range strings.SplitSeq(string(out), "\x1e") {
				rec = strings.TrimSpace(rec)
				if rec == "" {
					continue
				}
				lines := strings.Split(rec, "\n")
				fields := strings.Split(lines[0], "\x1f")
				if len(fields) != 4 {
					continue
				}
				c := commit{Commit: fields[0], Author: fields[1], Date: fields[2], Subject: fields[3]}
				hidden := 0
				for _, f := range lines[1:] {
					f = strings.TrimSpace(f)
					switch {
					case f == "":
					case workspace.IsSensitive(f) || len(c.Files) == maxFilesPerCommit:
						hidden++
					default:
						c.Files = append(c.Files, f)
					}
				}
				if hidden > 0 {
					c.Files = append(c.Files, fmt.Sprintf("(+%d more)", hidden))
				}
				line, _ := json.Marshal(c)
				if b.Len()+len(line)+1 > s.maxBytes-200 {
					b.WriteString("[more commits not shown]\n")
					break
				}
				b.Write(line)
				b.WriteByte('\n')
			}
			if b.Len() == 0 {
				return Output{Text: "No commits touch this path.\n"}, nil
			}
			return Output{Text: b.String()}, nil
		})
}

// pathspec validates an optional path for git; "" means the whole workspace.
// The path need not exist (history can name deleted files).
func (s *toolset) pathspec(p string) (string, error) {
	if p == "" {
		return ".", nil
	}
	return s.ws.Clean(p)
}
