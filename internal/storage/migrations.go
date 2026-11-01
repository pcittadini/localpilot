// Copyright 2026 The LocalPilot Authors
// SPDX-License-Identifier: Apache-2.0

package storage

import (
	"context"
	"database/sql"
	"fmt"
)

// migrations are applied in order; PRAGMA user_version records how many
// have run. Never edit a released migration: append a new one.
var migrations = []string{
	// 1: initial schema. Times are UTC in timeFormat, so they sort as text.
	`
CREATE TABLE tasks (
    id                TEXT PRIMARY KEY,
    project           TEXT NOT NULL,
    category          TEXT NOT NULL,
    model             TEXT NOT NULL DEFAULT '',
    model_version     TEXT NOT NULL DEFAULT '',
    status            TEXT NOT NULL,
    escalation_reason TEXT NOT NULL DEFAULT '',
    policy_version    TEXT NOT NULL DEFAULT '',
    started_at        TEXT NOT NULL,
    completed_at      TEXT NOT NULL,
    duration_ms       INTEGER NOT NULL
);
CREATE INDEX tasks_started_at ON tasks(started_at);

CREATE TABLE usage (
    task_id                       TEXT PRIMARY KEY REFERENCES tasks(id) ON DELETE CASCADE,
    local_input_tokens            INTEGER NOT NULL,
    local_output_tokens           INTEGER NOT NULL,
    model_calls                   INTEGER NOT NULL,
    claude_input_tokens_avoided   INTEGER NOT NULL,
    claude_output_tokens_avoided  INTEGER NOT NULL,
    claude_input_tokens_overhead  INTEGER NOT NULL,
    claude_output_tokens_overhead INTEGER NOT NULL,
    reference_model               TEXT NOT NULL,
    estimated_savings_usd         REAL NOT NULL
);

CREATE TABLE tool_calls (
    id          TEXT PRIMARY KEY,
    task_id     TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    seq         INTEGER NOT NULL,
    tool_name   TEXT NOT NULL,
    status      TEXT NOT NULL,
    duration_ms INTEGER NOT NULL,
    success     INTEGER NOT NULL
);
CREATE INDEX tool_calls_task ON tool_calls(task_id);

CREATE TABLE model_calls (
    task_id       TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    seq           INTEGER NOT NULL,
    duration_ms   INTEGER NOT NULL,
    input_tokens  INTEGER NOT NULL,
    output_tokens INTEGER NOT NULL,
    outcome       TEXT NOT NULL,
    PRIMARY KEY (task_id, seq)
);
`,
}

// migrate applies pending migrations, one transaction each. Transactions
// start IMMEDIATE (see Open), so concurrent LocalPilot processes take turns
// and each re-reads the version before applying anything.
func migrate(ctx context.Context, db *sql.DB) error {
	for {
		done, err := migrateStep(ctx, db)
		if err != nil || done {
			return err
		}
	}
}

func migrateStep(ctx context.Context, db *sql.DB) (bool, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	var v int
	if err := tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v); err != nil {
		return false, err
	}
	switch {
	case v > len(migrations):
		return false, fmt.Errorf("schema version %d is newer than this LocalPilot supports (%d); upgrade LocalPilot", v, len(migrations))
	case v == len(migrations):
		return true, tx.Commit()
	}
	if _, err := tx.ExecContext(ctx, migrations[v]); err != nil {
		return false, fmt.Errorf("migration %d: %w", v+1, err)
	}
	// PRAGMA does not take bound parameters; v+1 is an integer we computed.
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", v+1)); err != nil {
		return false, fmt.Errorf("migration %d: %w", v+1, err)
	}
	return false, tx.Commit()
}
