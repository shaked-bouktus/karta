// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 NVIDIA Corporation

package execution

import "context"

//go:generate go run go.uber.org/mock/mockgen -source=interface.go -destination=runner_mock.go -package=execution Runner
type Evaluator interface {
	// Evaluate evaluates a JQ expression and returns the results.
	Evaluate(ctx context.Context, expression string) ([]any, error)
	// GetObject returns the object as a golang basic type.
	GetObject() (any, error)
}

type Assigner interface {
	// Assign assigns a value to a given expression. e.g .name = "updated"
	Assign(ctx context.Context, expression string, value any) error
	// AssignZip assigns an array of values to a given array expression using zip operation. e.g .items[] = ["a", "b", "c"]
	// The length of the values array must match the length of the array expression.
	AssignZip(ctx context.Context, expression string, values []any) error
	// ResolvePaths resolves the concrete locations an expression addresses, without mutating.
	// An expression that computes a value instead of addressing one fails with PathNotWritableError.
	ResolvePaths(ctx context.Context, expression string) ([][]any, error)
	// SpliceValues sets each resolved path to the corresponding value, committed as one atomic update.
	SpliceValues(ctx context.Context, paths [][]any, values []any) error
}

type Runner interface {
	Evaluator
	Assigner
}
