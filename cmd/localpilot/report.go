// Copyright 2026 The LocalPilot Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/pcittadini/localpilot/internal/config"
	"github.com/pcittadini/localpilot/internal/hook"
	"github.com/pcittadini/localpilot/internal/metrics"
	"github.com/pcittadini/localpilot/internal/storage"
	"github.com/pcittadini/localpilot/internal/workspace"
)

// runReport prints the savings report. It only reads the metrics database:
// no model, workspace access or project configuration is involved.
func runReport(args []string) error {
	var flags commonFlags
	var period, scope string
	var asJSON bool
	fs := flag.NewFlagSet("report", flag.ContinueOnError)
	flags.register(fs)
	fs.StringVar(&period, "period", "month", "day, week, month or all")
	fs.StringVar(&scope, "scope", metrics.ScopeAll, "all workspaces, or only the current workspace")
	fs.BoolVar(&asJSON, "json", false, "print the report as JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(config.LoadOptions{Path: flags.config})
	if err != nil {
		return err
	}
	if !cfg.Metrics.Enabled {
		return errors.New("metrics are disabled (metrics.enabled: false)")
	}

	dir := flags.workspace
	if dir == "" {
		dir = cfg.Workspace
	}
	if dir == "" {
		if dir, err = os.Getwd(); err != nil {
			return err
		}
	}
	// Resolve the workspace the way the server does, so the "workspace"
	// scope matches the project recorded with each task.
	ws, err := workspace.New(dir, workspace.DefaultMaxFileSize)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, err := storage.OpenReadOnly(ctx, cfg.Metrics.Path)
	if errors.Is(err, storage.ErrNotFound) {
		return fmt.Errorf("no metrics yet: %s does not exist (it is created when LocalPilot runs its first task)", cfg.Metrics.Path)
	}
	if err != nil {
		return err
	}
	defer store.Close()

	rep, err := metrics.NewReporter(store, cfg.Savings, ws.Path()).Report(ctx, period, scope)
	if err != nil {
		return err
	}
	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}
	fmt.Print(rep.Text())
	return nil
}

// runHook handles one Claude Code hook event on stdin. It never fails and
// never exits non-zero: a broken hook must not get in the way of the
// session, so every problem simply produces no message.
func runHook(args []string) {
	var flags commonFlags
	fs := flag.NewFlagSet("hook", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	flags.register(fs)
	if fs.Parse(args) != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	var lookup hook.Lookup
	if cfg, err := config.Load(config.LoadOptions{Path: flags.config}); err == nil && cfg.Metrics.Enabled {
		lookup = func(ctx context.Context, id string) (storage.TaskSummary, bool) {
			store, err := storage.OpenReadOnly(ctx, cfg.Metrics.Path)
			if err != nil {
				return storage.TaskSummary{}, false
			}
			defer store.Close()
			t, ok, err := store.TaskByID(ctx, id)
			return t, ok && err == nil
		}
	}
	hook.Run(ctx, os.Stdin, os.Stdout, lookup)
}
