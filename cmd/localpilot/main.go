// Copyright 2026 The LocalPilot Authors
// SPDX-License-Identifier: Apache-2.0

// Command localpilot is a local-first MCP server that lets Claude Code
// delegate bounded, read-only tasks to local LLMs.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/pcittadini/localpilot/internal/agent"
	"github.com/pcittadini/localpilot/internal/config"
	"github.com/pcittadini/localpilot/internal/mcp"
	"github.com/pcittadini/localpilot/internal/models"
	"github.com/pcittadini/localpilot/internal/tools"
	"github.com/pcittadini/localpilot/internal/workspace"
)

// version is overridden at build time with -ldflags "-X main.version=...".
var version = "0.1.0-dev"

const usage = `LocalPilot delegates bounded development tasks to local LLMs over MCP.

Usage:
  localpilot serve    [--config FILE] [--workspace DIR]   run the MCP server on stdio
  localpilot check    [--config FILE] [--workspace DIR]   verify Ollama and model capabilities
  localpilot delegate [--config FILE] [--workspace DIR] [--task FILE]
                                                         run one task (JSON on stdin or --task) and print the result
  localpilot version

Environment overrides: LOCALPILOT_CONFIG, LOCALPILOT_WORKSPACE, LOCALPILOT_OLLAMA_URL,
LOCALPILOT_MODEL, LOCALPILOT_LOG_LEVEL, LOCALPILOT_LOG_FILE.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch cmd, args := os.Args[1], os.Args[2:]; cmd {
	case "serve":
		err = runServe(args)
	case "check":
		err = runCheck(args)
	case "delegate":
		err = runDelegate(args)
	case "version", "--version":
		fmt.Println(version)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "localpilot:", err)
		os.Exit(1)
	}
}

const insecureWarning = "Ollama is reached over plain HTTP on another host: workspace source code sent to the model crosses the network unencrypted and unauthenticated. Use a trusted network, an SSH tunnel or a TLS proxy."

// app holds the wired components shared by every command.
type app struct {
	cfg      *config.Config
	log      *slog.Logger
	ws       *workspace.Root
	ollama   *models.Ollama
	registry *models.Registry
	tools    *tools.Registry
	runtime  *agent.Runtime
	closeLog func()
}

type commonFlags struct {
	config    string
	workspace string
}

func (c *commonFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&c.config, "config", "", "config file (default $LOCALPILOT_CONFIG or ~/.config/localpilot/config.yaml)")
	fs.StringVar(&c.workspace, "workspace", "", "workspace root (default $LOCALPILOT_WORKSPACE or the current directory)")
}

func newApp(flags commonFlags, jsonLogs bool) (*app, error) {
	cfg, err := config.Load(config.LoadOptions{Path: flags.config})
	if err != nil {
		return nil, err
	}
	if flags.workspace != "" {
		cfg.Workspace = flags.workspace
	}
	if cfg.Workspace == "" {
		if cfg.Workspace, err = os.Getwd(); err != nil {
			return nil, err
		}
	}

	log, closeLog, err := newLogger(cfg.Logging, jsonLogs)
	if err != nil {
		return nil, err
	}
	ws, err := workspace.New(cfg.Workspace, workspace.DefaultMaxFileSize)
	if err != nil {
		closeLog()
		return nil, err
	}
	ollama, err := models.NewOllama(cfg.Providers.Ollama.BaseURL, nil)
	if err != nil {
		closeLog()
		return nil, err
	}
	reg, err := models.NewRegistry(cfg, map[string]models.Provider{"ollama": ollama})
	if err != nil {
		closeLog()
		return nil, err
	}
	tp := cfg.Delegation.Tools
	toolReg, err := tools.New(context.Background(), ws, tools.Options{
		Allowed:        tp.Allowed,
		Denied:         tp.Denied,
		Timeout:        time.Duration(tp.TimeoutSeconds) * time.Second,
		MaxResultBytes: tp.MaxResultBytes,
	})
	if err != nil {
		closeLog()
		return nil, fmt.Errorf("delegation.tools: %w", err)
	}
	return &app{
		cfg:      cfg,
		log:      log,
		ws:       ws,
		ollama:   ollama,
		registry: reg,
		tools:    toolReg,
		runtime:  agent.NewRuntime(cfg, reg, ws, toolReg, log),
		closeLog: closeLog,
	}, nil
}

// newLogger writes to the configured file or stderr. Never stdout: it carries
// MCP protocol messages while serving.
func newLogger(cfg config.Logging, jsonLogs bool) (*slog.Logger, func(), error) {
	var level slog.Level
	if err := level.UnmarshalText([]byte(cfg.Level)); err != nil {
		return nil, nil, fmt.Errorf("log level: %w", err)
	}
	var w io.Writer = os.Stderr
	closeFn := func() {}
	if cfg.File != "" {
		f, err := os.OpenFile(cfg.File, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return nil, nil, fmt.Errorf("log file: %w", err)
		}
		w, closeFn = f, func() { f.Close() }
	}
	opts := &slog.HandlerOptions{Level: level}
	if jsonLogs {
		return slog.New(slog.NewJSONHandler(w, opts)), closeFn, nil
	}
	return slog.New(slog.NewTextHandler(w, opts)), closeFn, nil
}

func runServe(args []string) error {
	var flags commonFlags
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	flags.register(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	a, err := newApp(flags, true)
	if err != nil {
		return err
	}
	defer a.closeLog()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	spec, _ := a.registry.Get("")
	a.log.Info("localpilot starting",
		"version", version, "workspace", a.ws.Path(), "config", a.cfg.Source,
		"ollama", a.cfg.Providers.Ollama.BaseURL, "model", spec.Name, "provider_model", spec.Model,
		"tools", a.tools.Enabled())
	for name, why := range a.tools.Unavailable() {
		a.log.Warn("tool unavailable", "tool", name, "reason", why)
	}
	if a.ollama.Insecure() {
		a.log.Warn(insecureWarning, "ollama", a.cfg.Providers.Ollama.BaseURL)
	}

	// Verify the model in the background: a missing Ollama must not stop
	// Claude Code from discovering the server; tasks escalate instead.
	go func() {
		checkCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		for _, r := range checkModels(checkCtx, a) {
			if r.err != nil {
				a.log.Warn("model check failed", "model", r.spec.Name, "error", r.err)
			}
			for _, w := range r.warnings {
				a.log.Warn("model check", "model", r.spec.Name, "warning", w)
			}
		}
	}()

	server, err := mcp.NewServer(a.runtime, a.cfg, version, a.log)
	if err != nil {
		return err
	}
	err = mcp.ServeStdio(ctx, server)
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

type modelReport struct {
	spec     models.Spec
	info     models.ModelInfo
	err      error
	warnings []string
}

func checkModels(ctx context.Context, a *app) []modelReport {
	var reports []modelReport
	for _, spec := range a.registry.All() {
		r := modelReport{spec: spec}
		r.info, r.err = spec.Provider.Describe(ctx, spec.Model)
		if r.err == nil {
			if !r.info.Has(models.CapabilityTools) {
				r.warnings = append(r.warnings, "model does not report tool-calling support; it cannot use the agent tools")
			}
			if spec.Think != nil && *spec.Think && !r.info.Has(models.CapabilityThinking) {
				r.warnings = append(r.warnings, "think is enabled but the model does not support thinking")
			}
			if spec.ContextWindow == 0 {
				r.warnings = append(r.warnings, "context_window is not set; input budgets are not clamped to the model context")
			} else if r.info.ServedContext > 0 && r.info.ServedContext != spec.ContextWindow {
				r.warnings = append(r.warnings, fmt.Sprintf(
					"context_window %d differs from the model's num_ctx %d; Ollama reloads the model with %d",
					spec.ContextWindow, r.info.ServedContext, spec.ContextWindow))
			}
		}
		reports = append(reports, r)
	}
	return reports
}

func runCheck(args []string) error {
	var flags commonFlags
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	flags.register(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	a, err := newApp(flags, false)
	if err != nil {
		return err
	}
	defer a.closeLog()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	source := a.cfg.Source
	if source == "" {
		source = "(built-in defaults)"
	}
	fmt.Printf("LocalPilot %s\nconfig:    %s\nworkspace: %s\n", version, source, a.ws.Path())
	fmt.Printf("tools:     %s\n", strings.Join(a.tools.Enabled(), ", "))
	for name, why := range a.tools.Unavailable() {
		fmt.Printf("           %s unavailable: %s\n", name, why)
	}

	v, err := a.ollama.Version(ctx)
	if err != nil {
		fmt.Printf("ollama:    %s  UNREACHABLE\n", a.cfg.Providers.Ollama.BaseURL)
		return err
	}
	fmt.Printf("ollama:    %s (version %s)\n", a.cfg.Providers.Ollama.BaseURL, v)
	if a.ollama.Insecure() {
		fmt.Printf("  warning: %s\n", insecureWarning)
	}

	failed := false
	for _, r := range checkModels(ctx, a) {
		marker := ""
		if r.spec.Name == a.cfg.Models.Default {
			marker = " (default)"
		}
		fmt.Printf("\nmodel %s%s -> %s\n", r.spec.Name, marker, r.spec.Model)
		if r.err != nil {
			fmt.Printf("  ERROR: %v\n", r.err)
			failed = true
			continue
		}
		fmt.Printf("  capabilities: %s\n", strings.Join(r.info.Capabilities, ", "))
		fmt.Printf("  size: %s %s; context: configured %d, served %d, trained %d\n",
			r.info.ParameterSize, r.info.Quantization, r.spec.ContextWindow, r.info.ServedContext, r.info.TrainedContext)
		for _, w := range r.warnings {
			fmt.Printf("  warning: %s\n", w)
		}
	}
	if failed {
		return errors.New("one or more models are unavailable")
	}

	// Connection test: one tiny structured-output request on the default model.
	spec, _ := a.registry.Get("")
	start := time.Now()
	resp, err := spec.Provider.Chat(ctx, models.ChatRequest{
		Model:          spec.Model,
		Messages:       []models.Message{{Role: models.RoleUser, Content: `Reply with {"ok": true}.`}},
		ResponseSchema: json.RawMessage(`{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"]}`),
		Temperature:    spec.Temperature,
		MaxTokens:      32,
		ContextWindow:  spec.ContextWindow,
		Think:          spec.Think,
	})
	if err != nil {
		return fmt.Errorf("structured output test: %w", err)
	}
	var probe struct {
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal([]byte(resp.Message.Content), &probe); err != nil || !probe.OK {
		return fmt.Errorf("structured output test: unexpected reply %q", resp.Message.Content)
	}
	fmt.Printf("\nstructured output test: OK (%s, %d+%d tokens)\n",
		time.Since(start).Round(time.Millisecond), resp.Usage.InputTokens, resp.Usage.OutputTokens)
	return nil
}

func runDelegate(args []string) error {
	var flags commonFlags
	var taskFile string
	fs := flag.NewFlagSet("delegate", flag.ContinueOnError)
	flags.register(fs)
	fs.StringVar(&taskFile, "task", "", "task JSON file in delegate_task format (default: stdin)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	a, err := newApp(flags, false)
	if err != nil {
		return err
	}
	defer a.closeLog()

	var data []byte
	if taskFile != "" {
		data, err = os.ReadFile(taskFile)
	} else {
		data, err = io.ReadAll(os.Stdin)
	}
	if err != nil {
		return err
	}
	var in mcp.DelegateInput
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return fmt.Errorf("task JSON: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	res := a.runtime.Execute(ctx, in.Task())

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(res)
}
