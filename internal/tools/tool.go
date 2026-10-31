// Copyright 2026 The LocalPilot Authors
// SPDX-License-Identifier: Apache-2.0

// Package tools implements the read-only capabilities a local model may call
// while working on a delegated task.
//
// Every tool is confined to the workspace, validates its arguments against a
// JSON schema before running, bounds its output, and never executes a shell.
package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/jsonschema-go/jsonschema"
)

// ErrInvalidArguments wraps argument validation failures.
var ErrInvalidArguments = errors.New("invalid arguments")

// Output is what a tool returns to the model.
type Output struct {
	Text string
	// Viewed lists the file lines this output showed the model. Evidence the
	// model cites later must overlap lines it has actually seen.
	Viewed []Span
}

// Span is an inclusive, 1-based line range of a workspace file.
type Span struct {
	File  string
	Start int
	End   int
}

// Tool is one callable capability.
type Tool struct {
	Name        string
	Description string
	// Parameters is the JSON Schema of the argument object.
	Parameters json.RawMessage

	resolved *jsonschema.Resolved
	invoke   func(ctx context.Context, raw json.RawMessage) (target string, out Output, err error)
}

// define builds a Tool whose arguments decode into A. The schema is inferred
// from A's struct tags; adjust may add bounds the tags cannot express.
// target summarizes the arguments for traces (e.g. the path read).
func define[A any](name, description string, adjust func(*jsonschema.Schema),
	target func(A) string, run func(context.Context, A) (Output, error)) Tool {

	schema, err := jsonschema.For[A](nil)
	if err != nil {
		panic(fmt.Sprintf("tool %s: schema: %v", name, err))
	}
	if adjust != nil {
		adjust(schema)
	}
	resolved, err := schema.Resolve(nil)
	if err != nil {
		panic(fmt.Sprintf("tool %s: resolve schema: %v", name, err))
	}
	params, err := json.Marshal(schema)
	if err != nil {
		panic(fmt.Sprintf("tool %s: marshal schema: %v", name, err))
	}

	t := Tool{Name: name, Description: description, Parameters: params, resolved: resolved}
	t.invoke = func(ctx context.Context, raw json.RawMessage) (string, Output, error) {
		args, err := decode[A](resolved, raw)
		if err != nil {
			return "", Output{}, err
		}
		tgt := target(args)
		out, err := run(ctx, args)
		return tgt, out, err
	}
	return t
}

// decode validates raw against the schema, then decodes it strictly.
func decode[A any](resolved *jsonschema.Resolved, raw json.RawMessage) (A, error) {
	var args A
	if len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		raw = json.RawMessage(`{}`)
	}
	var generic any
	if err := json.Unmarshal(raw, &generic); err != nil {
		return args, fmt.Errorf("%w: not a JSON object: %v", ErrInvalidArguments, err)
	}
	if err := resolved.Validate(generic); err != nil {
		return args, fmt.Errorf("%w: %v", ErrInvalidArguments, err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&args); err != nil {
		return args, fmt.Errorf("%w: %v", ErrInvalidArguments, err)
	}
	return args, nil
}

// intRange bounds an integer property.
func intRange(s *jsonschema.Schema, prop string, minimum, maximum float64) {
	p := s.Properties[prop]
	p.Minimum, p.Maximum = &minimum, &maximum
}

// strLength bounds a string property.
func strLength(s *jsonschema.Schema, prop string, minimum, maximum int) {
	p := s.Properties[prop]
	p.MinLength, p.MaxLength = &minimum, &maximum
}
