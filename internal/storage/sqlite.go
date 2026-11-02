// Copyright 2026 The LocalPilot Authors
// SPDX-License-Identifier: Apache-2.0

// Package storage persists task metrics in a local SQLite database.
//
// The database holds counts, timings, statuses and token estimates. It never
// holds prompts, objectives, source code, file paths read by the model or
// tool output: those stay in memory for the duration of a task.
package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite" // registers the pure-Go "sqlite" driver
)

// Store is an open metrics database.
type Store struct {
	db   *sql.DB
	path string
}

// ErrNotFound is returned when the database file does not exist and the
// store was opened read-only.
var ErrNotFound = errors.New("metrics database does not exist")

// Open opens (creating if needed) the database at path and applies pending
// migrations. The file and its directory are private to the user.
func Open(ctx context.Context, path string) (*Store, error) {
	if err := checkPath(path); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("metrics directory: %w", err)
	}
	// Create the file ourselves so it is never world-readable, whatever the
	// umask. SQLite gives its -wal and -shm files the same permissions.
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("metrics database: %w", err)
	}
	info, err := f.Stat()
	f.Close()
	if err != nil {
		return nil, fmt.Errorf("metrics database: %w", err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		if err := os.Chmod(path, 0o600); err != nil {
			return nil, fmt.Errorf("metrics database permissions: %w", err)
		}
	}

	s, err := open(path, "_txlock=immediate")
	if err != nil {
		return nil, err
	}
	if err := enableWAL(ctx, s.db); err != nil {
		s.Close()
		return nil, fmt.Errorf("metrics database %s: %w", path, err)
	}
	if err := migrate(ctx, s.db); err != nil {
		s.Close()
		return nil, fmt.Errorf("metrics database %s: %w", path, err)
	}
	return s, nil
}

// OpenReadOnly opens an existing database for queries only. It never creates
// the file or migrates it, so it is safe for short-lived readers such as
// Claude Code hooks.
func OpenReadOnly(ctx context.Context, path string) (*Store, error) {
	if err := checkPath(path); err != nil {
		return nil, err
	}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	} else if err != nil {
		return nil, err
	}
	s, err := open(path, "_pragma=query_only(1)")
	if err != nil {
		return nil, err
	}
	var v int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v); err != nil {
		s.Close()
		return nil, fmt.Errorf("metrics database %s: %w", path, err)
	}
	if v != len(migrations) {
		s.Close()
		return nil, fmt.Errorf("metrics database %s has schema version %d, this LocalPilot reads version %d", path, v, len(migrations))
	}
	return s, nil
}

func open(path, params string) (*Store, error) {
	// Each pooled connection gets these settings. A single connection
	// serializes writes within the process; busy_timeout waits for other
	// LocalPilot processes (one per Claude Code session) sharing the file.
	dsn := path + "?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&" + params
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("metrics database: %w", err)
	}
	db.SetMaxOpenConns(1)
	return &Store{db: db, path: path}, nil
}

// enableWAL switches the database to write-ahead logging, so readers (the
// hook, reports) never block the server. Changing the journal mode can fail
// with SQLITE_BUSY without waiting on busy_timeout while another process
// opens the file, so it is retried for a bounded time.
func enableWAL(ctx context.Context, db *sql.DB) error {
	deadline := time.Now().Add(5 * time.Second)
	for {
		var mode string
		err := db.QueryRowContext(ctx, "PRAGMA journal_mode=WAL").Scan(&mode)
		if err == nil || !isBusy(err) || time.Now().After(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func isBusy(err error) bool {
	var coded interface{ Code() int }
	return errors.As(err, &coded) && coded.Code()&0xff == 5 // SQLITE_BUSY
}

// checkPath rejects paths the driver would misread as URI parameters.
func checkPath(path string) error {
	switch {
	case path == "":
		return errors.New("metrics database path is empty")
	case !filepath.IsAbs(path):
		return fmt.Errorf("metrics database path must be absolute: %q", path)
	case strings.ContainsAny(path, "?#"):
		return fmt.Errorf("metrics database path must not contain '?' or '#': %q", path)
	}
	return nil
}

// Path returns the database file.
func (s *Store) Path() string { return s.path }

// Close releases the database.
func (s *Store) Close() error { return s.db.Close() }
