package taskctx

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// numberLines renders lines[start-1:end] with explicit line numbers so the
// model can cite exact locations. Blank lines are dropped: the numbers keep
// every reference accurate while saving tokens.
func numberLines(lines []string, start, end int) string {
	width := len(strconv.Itoa(end))
	var b strings.Builder
	for n := start; n <= end; n++ {
		line := strings.TrimRight(lines[n-1], " \t")
		if line == "" {
			continue
		}
		fmt.Fprintf(&b, "%*d| %s\n", width, n, line)
	}
	return b.String()
}

// secretPatterns match high-confidence credential formats. Matching is a
// safety net: sensitive files are already excluded by the workspace rules.
var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----`),
	regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`),
	regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{36,}\b`),
	regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{22,}\b`),
	regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}\b`),
	regexp.MustCompile(`\bsk-(?:ant-|proj-)?[A-Za-z0-9_-]{20,}\b`),
	regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}\b`),
}

// Redact replaces likely secrets with a placeholder and reports how many it
// replaced.
func Redact(s string) (string, int) {
	count := 0
	for _, re := range secretPatterns {
		s = re.ReplaceAllStringFunc(s, func(string) string {
			count++
			return "[REDACTED]"
		})
	}
	return s, count
}
