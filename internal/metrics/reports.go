// Copyright 2026 The LocalPilot Authors
// SPDX-License-Identifier: Apache-2.0

package metrics

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/pcittadini/localpilot/internal/agent"
	"github.com/pcittadini/localpilot/internal/config"
	"github.com/pcittadini/localpilot/internal/storage"
)

// Report periods are rolling windows ending now.
var periods = map[string]time.Duration{
	"day":   24 * time.Hour,
	"week":  7 * 24 * time.Hour,
	"month": 30 * 24 * time.Hour,
	"all":   0,
}

// Report scopes.
const (
	ScopeAll       = "all"
	ScopeWorkspace = "workspace"
)

// Report aggregates recorded tasks. Every Claude figure is an estimate.
type Report struct {
	Period    string `json:"period" jsonschema:"day, week, month or all"`
	Since     string `json:"since,omitempty" jsonschema:"start of the window (RFC 3339); absent for all"`
	Until     string `json:"until"`
	Scope     string `json:"scope" jsonschema:"all workspaces, or only the current one"`
	Workspace string `json:"workspace,omitempty"`

	Tasks TaskCounts `json:"tasks"`
	Local LocalUsage `json:"local_usage" jsonschema:"measured on the local model"`
	// Claude holds the estimates; see Notes for the method.
	Claude ClaudeEstimate `json:"claude_estimate" jsonschema:"estimated, not measured: see notes"`

	BillingMode string `json:"billing_mode" jsonschema:"subscription or api, from configuration"`
	// APIEquivalentValueUSD is the net estimate priced at API list prices.
	APIEquivalentValueUSD float64 `json:"api_equivalent_value_usd" jsonschema:"net estimated Claude tokens avoided, priced at API list prices; not money saved on a subscription"`
	// EstimatedSavingsUSD is set only in api billing mode.
	EstimatedSavingsUSD *float64 `json:"estimated_savings_usd,omitempty" jsonschema:"estimated API spend avoided; only in api billing mode"`

	ByCategory []CategoryUsage `json:"by_category,omitempty"`
	Notes      []string        `json:"notes"`
}

type TaskCounts struct {
	Total     int `json:"total"`
	Completed int `json:"completed"`
	Escalated int `json:"escalated"`
	Failed    int `json:"failed"`
	Rejected  int `json:"rejected"`
	// SuccessRate is completed over tasks that ran (rejected excluded).
	SuccessRate float64 `json:"success_rate" jsonschema:"completed / (completed + escalated + failed); rejected tasks never ran"`
}

type LocalUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	ModelCalls   int `json:"model_calls"`
	ToolCalls    int `json:"tool_calls"`
	// AvgDurationMS averages tasks that ran.
	AvgDurationMS int64 `json:"avg_duration_ms" jsonschema:"average over tasks that ran (rejected excluded)"`
}

type ClaudeEstimate struct {
	ReferenceModel       string `json:"reference_model" jsonschema:"Claude model the work is priced against"`
	InputTokensAvoided   int    `json:"input_tokens_avoided"`
	OutputTokensAvoided  int    `json:"output_tokens_avoided"`
	InputTokensOverhead  int    `json:"input_tokens_overhead" jsonschema:"delegation results Claude read"`
	OutputTokensOverhead int    `json:"output_tokens_overhead" jsonschema:"delegation requests Claude wrote"`
	NetInputTokens       int    `json:"net_input_tokens"`
	NetOutputTokens      int    `json:"net_output_tokens"`
}

type CategoryUsage struct {
	Category        string  `json:"category"`
	Tasks           int     `json:"tasks"`
	Completed       int     `json:"completed"`
	NetInputTokens  int     `json:"net_input_tokens"`
	NetOutputTokens int     `json:"net_output_tokens"`
	ValueUSD        float64 `json:"api_equivalent_value_usd"`
}

// Reporter builds reports from the metrics database.
type Reporter struct {
	store     *storage.Store
	savings   config.Savings
	workspace string
	now       func() time.Time
}

// NewReporter reports on store; workspace is the current workspace path,
// used by the "workspace" scope.
func NewReporter(store *storage.Store, savings config.Savings, workspace string) *Reporter {
	return &Reporter{store: store, savings: savings, workspace: workspace, now: time.Now}
}

// ValidPeriod reports whether p names a report period ("" means month).
func ValidPeriod(p string) bool {
	_, ok := periods[p]
	return ok || p == ""
}

// Report aggregates tasks for a period ("" = month) and scope ("" = all).
func (r *Reporter) Report(ctx context.Context, period, scope string) (Report, error) {
	if period == "" {
		period = "month"
	}
	if scope == "" {
		scope = ScopeAll
	}
	window, ok := periods[period]
	if !ok {
		return Report{}, fmt.Errorf("unknown period %q (use day, week, month or all)", period)
	}
	if scope != ScopeAll && scope != ScopeWorkspace {
		return Report{}, fmt.Errorf("unknown scope %q (use %s or %s)", scope, ScopeAll, ScopeWorkspace)
	}

	now := r.now()
	f := storage.Filter{}
	if window > 0 {
		f.Since = now.Add(-window)
	}
	if scope == ScopeWorkspace {
		f.Project = r.workspace
	}
	tasks, err := r.store.ListTasks(ctx, f)
	if err != nil {
		return Report{}, fmt.Errorf("read metrics: %w", err)
	}
	rep := Build(tasks, r.savings)
	rep.Period, rep.Scope, rep.Until = period, scope, now.UTC().Format(time.RFC3339)
	if !f.Since.IsZero() {
		rep.Since = f.Since.UTC().Format(time.RFC3339)
	}
	if scope == ScopeWorkspace {
		rep.Workspace = r.workspace
	}
	return rep, nil
}

// Build aggregates tasks. Claude figures are re-priced at the current
// configuration, so a pricing update applies to past tasks too.
func Build(tasks []storage.TaskSummary, savings config.Savings) Report {
	price, _ := savings.Price(savings.ReferenceModel)
	rep := Report{BillingMode: savings.Billing, Claude: ClaudeEstimate{ReferenceModel: savings.ReferenceModel}}
	byCat := map[string]*CategoryUsage{}
	var ran int
	var ranMS int64

	for _, t := range tasks {
		c := &rep.Tasks
		c.Total++
		switch agent.Status(t.Status) {
		case agent.StatusCompleted:
			c.Completed++
		case agent.StatusEscalated:
			c.Escalated++
		case agent.StatusRejected:
			c.Rejected++
		default:
			c.Failed++
		}
		if agent.Status(t.Status) != agent.StatusRejected {
			ran++
			ranMS += t.DurationMS
		}

		u := t.Usage
		rep.Local.InputTokens += u.LocalInputTokens
		rep.Local.OutputTokens += u.LocalOutputTokens
		rep.Local.ModelCalls += u.ModelCalls
		rep.Local.ToolCalls += t.ToolCallCount

		e := Estimate{InputAvoided: u.ClaudeInputAvoided, OutputAvoided: u.ClaudeOutputAvoided,
			InputOverhead: u.ClaudeInputOverhead, OutputOverhead: u.ClaudeOutputOverhead}
		ce := &rep.Claude
		ce.InputTokensAvoided += e.InputAvoided
		ce.OutputTokensAvoided += e.OutputAvoided
		ce.InputTokensOverhead += e.InputOverhead
		ce.OutputTokensOverhead += e.OutputOverhead

		cu := byCat[t.Category]
		if cu == nil {
			cu = &CategoryUsage{Category: t.Category}
			byCat[t.Category] = cu
		}
		cu.Tasks++
		if agent.Status(t.Status) == agent.StatusCompleted {
			cu.Completed++
		}
		cu.NetInputTokens += e.NetInput()
		cu.NetOutputTokens += e.NetOutput()
	}

	if ran > 0 {
		rep.Tasks.SuccessRate = round2(float64(rep.Tasks.Completed) / float64(ran))
		rep.Local.AvgDurationMS = ranMS / int64(ran)
	}
	ce := &rep.Claude
	ce.NetInputTokens = ce.InputTokensAvoided - ce.InputTokensOverhead
	ce.NetOutputTokens = ce.OutputTokensAvoided - ce.OutputTokensOverhead
	rep.APIEquivalentValueUSD = roundUSD(Cost(ce.NetInputTokens, ce.NetOutputTokens, price))
	if savings.Billing == config.BillingAPI {
		v := rep.APIEquivalentValueUSD
		rep.EstimatedSavingsUSD = &v
	}

	for _, cu := range byCat {
		cu.ValueUSD = roundUSD(Cost(cu.NetInputTokens, cu.NetOutputTokens, price))
		rep.ByCategory = append(rep.ByCategory, *cu)
	}
	sort.Slice(rep.ByCategory, func(i, j int) bool { return rep.ByCategory[i].Category < rep.ByCategory[j].Category })

	rep.Notes = notes(savings, price)
	return rep
}

func notes(savings config.Savings, price config.ModelPrice) []string {
	n := []string{
		fmt.Sprintf("Claude token figures are estimates, not measured billing: about %d bytes per token for the material the local model read (workspace files and tool results) and wrote in place of Claude, minus what Claude spent writing each delegation request and reading its result.", ClaudeBytesPerToken),
		"Only completed tasks count as avoided work. Escalated, failed and rejected tasks count their overhead only, because Claude still does the work.",
		"Prompt caching and re-reading of material in later turns are not modelled; LocalPilot's tool descriptions in Claude's context are not attributed to tasks.",
	}
	rate := fmt.Sprintf("%s list prices ($%.2f input, $%.2f output per million tokens)", savings.ReferenceModel, price.Input, price.Output)
	if savings.Billing == config.BillingAPI {
		n = append(n, "Billing mode is api: estimated_savings_usd is API spend avoided at "+rate+".")
	} else {
		n = append(n, "Billing mode is subscription: api_equivalent_value_usd is what the avoided usage would cost at "+rate+". It is not money saved; on a subscription, delegation preserves usage capacity.")
	}
	return n
}

// Text renders the report for a terminal.
func (r Report) Text() string {
	var b strings.Builder
	window := "all time"
	if r.Period != "all" {
		window = "last " + map[string]string{"day": "24 hours", "week": "7 days", "month": "30 days"}[r.Period]
	}
	where := "all workspaces"
	if r.Scope == ScopeWorkspace {
		where = r.Workspace
	}
	fmt.Fprintf(&b, "LocalPilot savings report: %s, %s\n\n", window, where)

	c := r.Tasks
	fmt.Fprintf(&b, "Tasks:        %d (%d completed, %d escalated, %d failed, %d rejected)", c.Total, c.Completed, c.Escalated, c.Failed, c.Rejected)
	if c.Total-c.Rejected > 0 {
		fmt.Fprintf(&b, ", success rate %.0f%%", c.SuccessRate*100)
	}
	l := r.Local
	fmt.Fprintf(&b, "\nLocal model:  %s model calls, %s tool calls, %s input + %s output tokens, %.1fs average per task\n",
		agent.FormatCount(l.ModelCalls), agent.FormatCount(l.ToolCalls), agent.FormatCount(l.InputTokens), agent.FormatCount(l.OutputTokens), float64(l.AvgDurationMS)/1000)

	e := r.Claude
	fmt.Fprintf(&b, "\nClaude, estimated, priced as %s:\n", e.ReferenceModel)
	fmt.Fprintf(&b, "  avoided     %10s input  %10s output tokens\n", agent.FormatCount(e.InputTokensAvoided), agent.FormatCount(e.OutputTokensAvoided))
	fmt.Fprintf(&b, "  overhead    %10s input  %10s output tokens\n", agent.FormatCount(e.InputTokensOverhead), agent.FormatCount(e.OutputTokensOverhead))
	fmt.Fprintf(&b, "  net         %10s input  %10s output tokens\n", agent.FormatCount(e.NetInputTokens), agent.FormatCount(e.NetOutputTokens))
	if r.EstimatedSavingsUSD != nil {
		fmt.Fprintf(&b, "\nEstimated API spend avoided: %s\n", usd(*r.EstimatedSavingsUSD))
	} else {
		fmt.Fprintf(&b, "\nAPI-equivalent value: %s (subscription billing: usage capacity preserved, not money saved)\n", usd(r.APIEquivalentValueUSD))
	}

	if len(r.ByCategory) > 0 {
		b.WriteString("\nBy category:\n")
		for _, cu := range r.ByCategory {
			fmt.Fprintf(&b, "  %-16s %3d tasks, %3d completed, net %s input + %s output tokens, %s\n",
				cu.Category, cu.Tasks, cu.Completed, agent.FormatCount(cu.NetInputTokens), agent.FormatCount(cu.NetOutputTokens), usd(cu.ValueUSD))
		}
	}
	b.WriteString("\nNotes:\n")
	for _, n := range r.Notes {
		fmt.Fprintf(&b, "  - %s\n", n)
	}
	return b.String()
}

func usd(v float64) string {
	if v < 0 {
		return fmt.Sprintf("-$%.2f", -v)
	}
	return fmt.Sprintf("$%.2f", v)
}

func round2(v float64) float64 { return float64(int(v*100+0.5)) / 100 }
