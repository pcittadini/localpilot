// Copyright 2026 The LocalPilot Authors
// SPDX-License-Identifier: Apache-2.0

package policy

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"

	"go.yaml.in/yaml/v3"

	"github.com/pcittadini/localpilot/internal/config"
	"github.com/pcittadini/localpilot/internal/workspace"
)

// ProjectConfigPath is where a workspace may narrow the global policy.
const ProjectConfigPath = ".localpilot/config.yaml"

const maxProjectConfigBytes = 64 << 10

// ProjectConfig is the subset of configuration a project may set. Every
// field can only narrow the global configuration. Anything else (providers,
// models, logging, workspace) is not part of the schema, so strict decoding
// rejects it: a repository must never redirect where its code is sent.
type ProjectConfig struct {
	Version    int               `yaml:"version"`
	Delegation projectDelegation `yaml:"delegation"`
}

type projectDelegation struct {
	Defaults struct {
		Enabled *bool `yaml:"enabled"`
	} `yaml:"defaults"`
	Budgets  config.Budgets    `yaml:"budgets"`
	Policies []projectCategory `yaml:"policies"`
	Tools    projectTools      `yaml:"tools"`
}

type projectCategory struct {
	Category string         `yaml:"category"`
	Enabled  *bool          `yaml:"enabled"`
	Model    string         `yaml:"model"`
	Tools    *[]string      `yaml:"tools"`
	Budgets  config.Budgets `yaml:"budgets"`
}

type projectTools struct {
	Allowed        *[]string `yaml:"allowed"`
	Denied         []string  `yaml:"denied"`
	TimeoutSeconds int       `yaml:"timeout_seconds"`
	MaxResultBytes int       `yaml:"max_result_bytes"`
}

// LoadProject reads the workspace's project configuration. It returns
// (nil, nil) when there is none. Any other failure must disable delegation:
// ignoring a broken file would silently drop the restrictions it intended.
func LoadProject(ws *workspace.Root) (*ProjectConfig, error) {
	data, _, err := ws.ReadFile(ProjectConfigPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return ParseProject(data)
}

// ParseProject decodes a project configuration strictly.
func ParseProject(data []byte) (*ProjectConfig, error) {
	if len(data) > maxProjectConfigBytes {
		return nil, fmt.Errorf("file exceeds %d bytes", maxProjectConfigBytes)
	}
	var p ProjectConfig
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&p); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if p.Version != 0 && p.Version != 1 {
		return nil, fmt.Errorf("version: unsupported value %d (want 1)", p.Version)
	}
	return &p, nil
}

// Apply returns the global configuration narrowed by the project
// configuration, plus a warning for every project setting that was ignored
// because it would have widened the global policy. The global configuration
// is not modified.
func Apply(global *config.Config, project *ProjectConfig) (*config.Config, []string) {
	eff := global.Clone()
	if project == nil {
		return eff, nil
	}
	var warnings []string
	warn := func(format string, args ...any) { warnings = append(warnings, fmt.Sprintf(format, args...)) }
	pd := project.Delegation

	if v := pd.Defaults.Enabled; v != nil {
		switch {
		case !*v:
			eff.Delegation.Defaults.Enabled = false
		case !global.Delegation.Defaults.Enabled:
			warn("delegation.defaults.enabled: cannot enable delegation that is disabled globally")
		}
	}

	// Delegation-wide budgets cap every category, including categories
	// whose global settings grant more.
	type budgetField struct {
		name    string
		project int
		get     func(*config.Budgets) *int
	}
	fields := []budgetField{
		{"max_input_tokens", pd.Budgets.MaxInputTokens, func(b *config.Budgets) *int { return &b.MaxInputTokens }},
		{"max_output_tokens", pd.Budgets.MaxOutputTokens, func(b *config.Budgets) *int { return &b.MaxOutputTokens }},
		{"max_tool_calls", pd.Budgets.MaxToolCalls, func(b *config.Budgets) *int { return &b.MaxToolCalls }},
		{"max_iterations", pd.Budgets.MaxIterations, func(b *config.Budgets) *int { return &b.MaxIterations }},
		{"timeout_seconds", pd.Budgets.TimeoutSeconds, func(b *config.Budgets) *int { return &b.TimeoutSeconds }},
	}
	for _, f := range fields {
		if f.project == 0 {
			continue
		}
		cur := f.get(&eff.Delegation.Budgets)
		if f.project < 0 || f.project > *cur {
			warn("delegation.budgets.%s: %d ignored; a project can only lower it (global %d)", f.name, f.project, *cur)
			continue
		}
		*cur = f.project
		for i := range eff.Delegation.Policies {
			if v := f.get(&eff.Delegation.Policies[i].Budgets); *v > f.project {
				*v = f.project
			}
		}
	}

	for _, pc := range pd.Policies {
		i := slices.IndexFunc(eff.Delegation.Policies, func(c config.CategoryPolicy) bool { return c.Category == pc.Category })
		if i < 0 {
			warn("delegation.policies: category %q is not configured globally; ignored", pc.Category)
			continue
		}
		ec := &eff.Delegation.Policies[i]
		prefix := "delegation.policies[" + pc.Category + "]"

		if pc.Enabled != nil {
			switch {
			case !*pc.Enabled:
				ec.Enabled = false
			case !ec.Enabled:
				warn("%s.enabled: cannot enable a category that is disabled globally", prefix)
			}
		}
		if pc.Model != "" {
			if _, ok := eff.Model(pc.Model); ok {
				ec.Model = pc.Model
			} else {
				warn("%s.model: %q is not a globally configured model; ignored", prefix, pc.Model)
			}
		}
		if pc.Tools != nil {
			base := eff.Delegation.Tools.Allowed
			if ec.Tools != nil {
				base = *ec.Tools
			}
			kept := []string{}
			for _, t := range *pc.Tools {
				if slices.Contains(base, t) {
					kept = append(kept, t)
				} else {
					warn("%s.tools: %q is not permitted for this category globally; ignored", prefix, t)
				}
			}
			ec.Tools = &kept
		}
		for _, f := range []budgetField{
			{"max_input_tokens", pc.Budgets.MaxInputTokens, func(b *config.Budgets) *int { return &b.MaxInputTokens }},
			{"max_output_tokens", pc.Budgets.MaxOutputTokens, func(b *config.Budgets) *int { return &b.MaxOutputTokens }},
			{"max_tool_calls", pc.Budgets.MaxToolCalls, func(b *config.Budgets) *int { return &b.MaxToolCalls }},
			{"max_iterations", pc.Budgets.MaxIterations, func(b *config.Budgets) *int { return &b.MaxIterations }},
			{"timeout_seconds", pc.Budgets.TimeoutSeconds, func(b *config.Budgets) *int { return &b.TimeoutSeconds }},
		} {
			if f.project == 0 {
				continue
			}
			v := f.get(&ec.Budgets)
			current := *v
			if current == 0 {
				current = *f.get(&eff.Delegation.Budgets)
			}
			if f.project < 0 || f.project > current {
				warn("%s.budgets.%s: %d ignored; a project can only lower it (effective %d)", prefix, f.name, f.project, current)
				continue
			}
			*v = f.project
		}
	}

	if pd.Tools.Allowed != nil {
		kept := []string{}
		for _, t := range *pd.Tools.Allowed {
			if slices.Contains(eff.Delegation.Tools.Allowed, t) {
				kept = append(kept, t)
			} else {
				warn("delegation.tools.allowed: %q is not allowed globally; ignored", t)
			}
		}
		eff.Delegation.Tools.Allowed = kept
	}
	for _, t := range pd.Tools.Denied {
		if !slices.Contains(eff.Delegation.Tools.Denied, t) {
			eff.Delegation.Tools.Denied = append(eff.Delegation.Tools.Denied, t)
		}
	}
	tighten := func(name string, project int, dst *int) {
		switch {
		case project == 0:
		case project < 0 || project > *dst:
			warn("delegation.tools.%s: %d ignored; a project can only lower it (global %d)", name, project, *dst)
		default:
			*dst = project
		}
	}
	tighten("timeout_seconds", pd.Tools.TimeoutSeconds, &eff.Delegation.Tools.TimeoutSeconds)
	tighten("max_result_bytes", pd.Tools.MaxResultBytes, &eff.Delegation.Tools.MaxResultBytes)
	if eff.Delegation.Tools.MaxResultBytes < 1000 {
		warn("delegation.tools.max_result_bytes: raised to the minimum of 1000")
		eff.Delegation.Tools.MaxResultBytes = 1000
	}
	return eff, warnings
}
