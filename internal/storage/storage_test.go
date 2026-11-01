// Copyright 2026 The LocalPilot Authors
// SPDX-License-Identifier: Apache-2.0

package storage

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

func sample(id string, started time.Time) Task {
	return Task{
		ID: id, Project: "/work/app", Category: "code_analysis", Model: "qwen-local", ModelVersion: "qwen3.5:9b",
		Status: "completed", PolicyVersion: "abc123", StartedAt: started, CompletedAt: started.Add(1500 * time.Millisecond),
		DurationMS: 1500,
		Usage: Usage{LocalInputTokens: 900, LocalOutputTokens: 120, ModelCalls: 2,
			ClaudeInputAvoided: 700, ClaudeOutputAvoided: 60, ClaudeInputOverhead: 200, ClaudeOutputOverhead: 40,
			ReferenceModel: "claude-opus-5-5", EstimatedSavingsUSD: 0.0024},
		ToolCalls:  []ToolCall{{Tool: "read_file", Status: "ok", DurationMS: 3, Success: true}, {Tool: "shell", Status: "denied"}},
		ModelCalls: []ModelCall{{DurationMS: 800, InputTokens: 400, OutputTokens: 40, Outcome: "tool_calls"}, {DurationMS: 700, InputTokens: 500, OutputTokens: 80, Outcome: "answer"}},
	}
}

func openTemp(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "data", "metrics.db")
	s, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}

func TestOpenCreatesPrivateFiles(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permissions")
	}
	s, path := openTemp(t)
	if err := s.InsertTask(context.Background(), sample("t1", time.Now())); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{filepath.Dir(path), path, path + "-wal"} {
		info, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm()&0o077 != 0 {
			t.Errorf("%s has mode %v; must not be readable by others", p, info.Mode().Perm())
		}
	}
}

func TestOpenTightensExistingPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permissions")
	}
	path := filepath.Join(t.TempDir(), "metrics.db")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", info.Mode().Perm())
	}
}

func TestInsertAndRead(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	start := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	want := sample("t1", start)
	if err := s.InsertTask(ctx, want); err != nil {
		t.Fatal(err)
	}

	got, ok, err := s.TaskByID(ctx, "t1")
	if err != nil || !ok {
		t.Fatalf("TaskByID: %v %v", ok, err)
	}
	if got.Category != want.Category || got.ModelVersion != "qwen3.5:9b" || got.Usage != want.Usage || got.ToolCallCount != 2 ||
		!got.StartedAt.Equal(start) || !got.CompletedAt.Equal(want.CompletedAt) {
		t.Errorf("got %+v", got)
	}
	if _, ok, err := s.TaskByID(ctx, "missing"); ok || err != nil {
		t.Errorf("missing task: %v %v", ok, err)
	}

	var modelCalls int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM model_calls WHERE task_id = 't1'`).Scan(&modelCalls); err != nil || modelCalls != 2 {
		t.Errorf("model calls = %d, %v", modelCalls, err)
	}

	// IDs are unique: a second insert fails and leaves nothing half-written.
	if err := s.InsertTask(ctx, want); err == nil {
		t.Error("duplicate task inserted")
	}
	var tools int
	s.db.QueryRow(`SELECT COUNT(*) FROM tool_calls`).Scan(&tools)
	if tools != 2 {
		t.Errorf("tool calls after failed insert = %d, want 2", tools)
	}
}

func TestListTasksFilters(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	for i := range 4 {
		task := sample(fmt.Sprintf("t%d", i), base.Add(time.Duration(i)*24*time.Hour))
		if i == 3 {
			task.Project = "/work/other"
		}
		if err := s.InsertTask(ctx, task); err != nil {
			t.Fatal(err)
		}
	}
	all, err := s.ListTasks(ctx, Filter{})
	if err != nil || len(all) != 4 || all[0].ID != "t0" {
		t.Fatalf("all = %d tasks, %v", len(all), err)
	}
	recent, _ := s.ListTasks(ctx, Filter{Since: base.Add(36 * time.Hour)})
	if len(recent) != 2 || recent[0].ID != "t2" {
		t.Errorf("since = %+v", recent)
	}
	// Times stored in UTC compare correctly whatever the caller's zone.
	local := base.Add(36 * time.Hour).In(time.FixedZone("UTC+9", 9*3600))
	if inZone, _ := s.ListTasks(ctx, Filter{Since: local}); len(inZone) != 2 {
		t.Errorf("since in another zone = %d tasks", len(inZone))
	}
	project, _ := s.ListTasks(ctx, Filter{Project: "/work/other"})
	if len(project) != 1 || project[0].ID != "t3" {
		t.Errorf("project = %+v", project)
	}
}

func TestPurgeCascades(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	s.InsertTask(ctx, sample("old", base))
	s.InsertTask(ctx, sample("new", base.Add(48*time.Hour)))

	n, err := s.Purge(ctx, base.Add(24*time.Hour))
	if err != nil || n != 1 {
		t.Fatalf("purged %d, %v", n, err)
	}
	for _, table := range []string{"usage", "tool_calls", "model_calls"} {
		var left int
		s.db.QueryRow(`SELECT COUNT(*) FROM ` + table + ` WHERE task_id = 'old'`).Scan(&left)
		if left != 0 {
			t.Errorf("%s keeps %d rows of a purged task", table, left)
		}
	}
	if _, ok, _ := s.TaskByID(ctx, "new"); !ok {
		t.Error("recent task purged")
	}
}

func TestMigrationsAreIdempotent(t *testing.T) {
	s, path := openTemp(t)
	s.InsertTask(context.Background(), sample("t1", time.Now()))
	s.Close()

	again, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	var v int
	again.db.QueryRow("PRAGMA user_version").Scan(&v)
	if v != len(migrations) {
		t.Errorf("user_version = %d", v)
	}
	if _, ok, _ := again.TaskByID(context.Background(), "t1"); !ok {
		t.Error("data lost on reopen")
	}
}

func TestNewerSchemaIsRefused(t *testing.T) {
	s, path := openTemp(t)
	s.db.Exec(fmt.Sprintf("PRAGMA user_version = %d", len(migrations)+1))
	s.Close()
	if _, err := Open(context.Background(), path); err == nil {
		t.Error("opened a database from a newer LocalPilot")
	}
	if _, err := OpenReadOnly(context.Background(), path); err == nil {
		t.Error("read a database from a newer LocalPilot")
	}
}

func TestConcurrentOpenAndInsert(t *testing.T) {
	// One process per Claude Code session may share the file.
	path := filepath.Join(t.TempDir(), "metrics.db")
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := range 8 {
		wg.Go(func() {
			s, err := Open(context.Background(), path)
			if err != nil {
				errs <- err
				return
			}
			defer s.Close()
			errs <- s.InsertTask(context.Background(), sample(fmt.Sprintf("t%d", i), time.Now()))
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
	s, err := OpenReadOnly(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if all, _ := s.ListTasks(context.Background(), Filter{}); len(all) != 8 {
		t.Errorf("tasks = %d, want 8", len(all))
	}
}

func TestOpenReadOnly(t *testing.T) {
	ctx := context.Background()
	missing := filepath.Join(t.TempDir(), "none.db")
	if _, err := OpenReadOnly(ctx, missing); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing file: %v", err)
	}
	if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Error("a read-only open created the file")
	}

	s, path := openTemp(t)
	s.InsertTask(ctx, sample("t1", time.Now()))
	ro, err := OpenReadOnly(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	if _, ok, _ := ro.TaskByID(ctx, "t1"); !ok {
		t.Error("read-only store cannot read")
	}
	if err := ro.InsertTask(ctx, sample("t2", time.Now())); err == nil {
		t.Error("read-only store accepted a write")
	}
}

func TestPathValidation(t *testing.T) {
	for _, p := range []string{"", "relative/metrics.db", "/tmp/a?mode=memory", "/tmp/a#b"} {
		if _, err := Open(context.Background(), p); err == nil {
			t.Errorf("Open(%q) succeeded", p)
		}
	}
}
