// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 NVIDIA Corporation

package execution

import "fmt"

type JQParseError struct {
	Expression string
	Err        error
}

func (e *JQParseError) Error() string {
	return fmt.Sprintf("failed to parse JQ expression '%s': %v", e.Expression, e.Err)
}

func (e *JQParseError) Unwrap() error {
	return e.Err
}

type JQCompileError struct {
	Expression string
	Err        error
}

func (e *JQCompileError) Error() string {
	return fmt.Sprintf("failed to compile JQ expression '%s': %v", e.Expression, e.Err)
}

func (e *JQCompileError) Unwrap() error {
	return e.Err
}

// PathNotWritableError reports an expression that computes a value instead of
// addressing locations in the object, so nothing can be written through it.
type PathNotWritableError struct {
	Expression string
	Err        error
}

func (e *PathNotWritableError) Error() string {
	return fmt.Sprintf("expression '%s' does not address writable locations: %v", e.Expression, e.Err)
}

func (e *PathNotWritableError) Unwrap() error {
	return e.Err
}

type JQExecutionError struct {
	Expression string
	Err        error
}

func (e *JQExecutionError) Error() string {
	return fmt.Sprintf("JQ execution error for expression '%s': %v", e.Expression, e.Err)
}

func (e *JQExecutionError) Unwrap() error {
	return e.Err
}
