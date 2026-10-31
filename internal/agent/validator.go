package agent

import (
	"encoding/json"
	"fmt"
	"strings"

	"localpilot/internal/taskctx"
	"localpilot/internal/workspace"
)

// outputSchema constrains the model's reply when JSON output is requested.
// It mirrors Output and Finding.
var outputSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "summary": {"type": "string"},
    "findings": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "description": {"type": "string"},
          "file": {"type": "string"},
          "line_start": {"type": "integer"},
          "line_end": {"type": "integer"}
        },
        "required": ["description"]
      }
    },
    "complete": {"type": "boolean"},
    "limitations": {"type": "string"}
  },
  "required": ["summary", "findings", "complete", "limitations"]
}`)

// parseOutput decodes and structurally validates a JSON reply. A non-empty
// issue list means the output must not be trusted.
func parseOutput(content string) (Output, []string) {
	var raw struct {
		Summary     *string    `json:"summary"`
		Findings    *[]Finding `json:"findings"`
		Complete    *bool      `json:"complete"`
		Limitations string     `json:"limitations"`
	}
	if err := json.Unmarshal([]byte(stripFences(content)), &raw); err != nil {
		return Output{}, []string{"reply is not valid JSON: " + err.Error()}
	}

	var out Output
	var issues []string
	if raw.Summary == nil || strings.TrimSpace(*raw.Summary) == "" {
		issues = append(issues, "summary is missing or empty")
	} else {
		out.Summary = strings.TrimSpace(*raw.Summary)
	}
	if raw.Findings == nil {
		issues = append(issues, "findings is missing")
	} else {
		out.Findings = *raw.Findings
	}
	if raw.Complete == nil {
		issues = append(issues, "complete is missing")
	} else {
		out.Complete = *raw.Complete
	}
	out.Limitations = strings.TrimSpace(raw.Limitations)

	for i, f := range out.Findings {
		if strings.TrimSpace(f.Description) == "" {
			issues = append(issues, fmt.Sprintf("findings[%d].description is empty", i))
		}
		if f.File == "" && (f.LineStart != 0 || f.LineEnd != 0) {
			issues = append(issues, fmt.Sprintf("findings[%d] cites lines without a file", i))
		}
	}
	return out, issues
}

// checkEvidence verifies every location cited in findings. A citation naming
// an excerpt label is checked against that excerpt; anything else must be a
// file in the workspace. Cited lines must exist and overlap lines the model
// was actually shown. Finding paths are normalized to workspace-relative
// form.
func checkEvidence(ws *workspace.Root, pkg *taskctx.Package, cov *coverage, findings []Finding) ([]Evidence, []string) {
	type fileInfo struct {
		rel   string
		lines int
		err   error
	}
	cache := map[string]fileInfo{}
	seen := map[Evidence]bool{}
	var evidence []Evidence
	var issues []string

	for i := range findings {
		f := &findings[i]
		if f.File == "" {
			continue
		}
		info, ok := cache[f.File]
		source := SourceFile
		if ex, isExcerpt := pkg.Excerpt(f.File); isExcerpt {
			info, source = fileInfo{rel: ex.Label, lines: ex.TotalLines}, SourceExcerpt
		} else if !ok {
			data, rel, err := ws.ReadFile(f.File)
			info = fileInfo{rel: rel, lines: taskctx.LineCount(data), err: err}
			cache[f.File] = info
		}

		ev := Evidence{File: f.File, Source: source, LineStart: f.LineStart, LineEnd: f.LineEnd, Valid: true}
		if info.err != nil {
			ev.Valid, ev.Issue = false, info.err.Error()
		} else {
			f.File, ev.File = info.rel, info.rel
			if ev.LineStart != 0 || ev.LineEnd != 0 {
				if ev.LineEnd == 0 {
					ev.LineEnd = ev.LineStart
				}
				switch {
				case ev.LineStart < 1 || ev.LineEnd < ev.LineStart || ev.LineEnd > info.lines:
					ev.Valid = false
					ev.Issue = fmt.Sprintf("lines %d-%d are outside the %s (1-%d)", ev.LineStart, ev.LineEnd, source, info.lines)
				case !cov.seen(ev.File, ev.LineStart, ev.LineEnd):
					ev.Valid = false
					ev.Issue = fmt.Sprintf("the model was never shown lines %d-%d of this %s", ev.LineStart, ev.LineEnd, source)
				}
			}
		}

		key := ev
		key.Issue = ""
		if seen[key] {
			continue
		}
		seen[key] = true
		evidence = append(evidence, ev)
		if !ev.Valid {
			issues = append(issues, fmt.Sprintf("findings[%d]: %s: %s", i, ev.File, ev.Issue))
		}
	}
	return evidence, issues
}

// stripFences removes a Markdown code fence some models wrap JSON in.
func stripFences(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "```") {
		return s
	}
	s = strings.TrimPrefix(s, "```")
	if nl := strings.IndexByte(s, '\n'); nl >= 0 {
		s = s[nl+1:]
	}
	return strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s), "```"))
}
