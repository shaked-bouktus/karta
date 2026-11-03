// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 NVIDIA Corporation

package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDefinitionWaitProcessHelper(t *testing.T) {
	mode := os.Getenv("KARTA_LOAD_DEFINITION_WAIT_HELPER")
	if mode == "" {
		return
	}
	decoder, encoder := json.NewDecoder(os.Stdin), json.NewEncoder(os.Stdout)
	polls, statuses := 0, 0
	for {
		var command Command
		if err := decoder.Decode(&command); err != nil {
			os.Exit(0)
		}
		response := Response{Ready: true}
		switch command.Operation {
		case "progress":
			polls++
			if mode == "delayed" {
				time.Sleep(150 * time.Millisecond)
			}
			response.Stats = RecorderStats{Observed: 2, Completed: 2}
		case "status":
			statuses++
			response.Valid = mode != "semantic" || statuses >= 2
			if !response.Valid {
				response.Validation = "definition semantics have not converged"
			}
		}
		response.Pods, response.Workloads = polls, statuses
		if err := encoder.Encode(response); err != nil {
			os.Exit(2)
		}
		if command.Operation == "stop" {
			os.Exit(0)
		}
	}
}

func startDefinitionWaitExporter(t *testing.T, mode string) *exporterProcess {
	t.Helper()
	t.Setenv("KARTA_LOAD_DEFINITION_WAIT_HELPER", mode)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	helper := filepath.Join(dir, "exporter")
	script := "#!/bin/sh\nexec '" + strings.ReplaceAll(executable, "'", "'\\''") + "' -test.run=^TestDefinitionWaitProcessHelper$ -- \"$@\"\n"
	if err := os.WriteFile(helper, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	process, err := startExporter(t.Context(), helper, "unused", filepath.Join(dir, "exporter.log"), 100, 2, "workload")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = process.Close() })
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if _, err := process.Request(ctx, Command{Operation: "ready"}); err != nil {
		t.Fatal(err)
	}
	return process
}

func TestWaitDefinitionConvergenceDeadlinePreservesChild(t *testing.T) {
	process := startDefinitionWaitExporter(t, "delayed")
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	_, err := waitDefinitionConvergence(ctx, process, 2)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("wait error = %v, want deadline exceeded", err)
	}
	requestCtx, cancelRequest := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancelRequest()
	response, err := process.Request(requestCtx, Command{Operation: "status"})
	if err != nil {
		t.Fatalf("child did not survive phase deadline: %v", err)
	}
	if response.Pods != 1 || response.Workloads != 1 {
		t.Fatalf("polls = %d, statuses = %d; want one progress poll and only the post-deadline status", response.Pods, response.Workloads)
	}
	closed := make(chan error, 1)
	go func() { closed <- process.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("graceful child close: %v", err)
		}
	case <-time.After(5 * time.Second):
		_ = process.cmd.Process.Kill()
		t.Fatal("child cleanup exceeded bound")
	}
	select {
	case <-process.done:
	default:
		t.Fatal("child was not reaped")
	}
}

func TestWaitDefinitionConvergenceRequiresSemanticValidation(t *testing.T) {
	process := startDefinitionWaitExporter(t, "semantic")
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	response, err := waitDefinitionConvergence(ctx, process, 2)
	if err != nil {
		t.Fatal(err)
	}
	if !response.Valid || response.Workloads != 2 || response.Pods != 2 {
		t.Fatalf("returned before semantic convergence: %+v", response)
	}
}
