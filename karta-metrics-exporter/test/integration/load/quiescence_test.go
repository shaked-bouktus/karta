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

func TestPublicationQuietProcessHelper(t *testing.T) {
	mode := os.Getenv("KARTA_LOAD_QUIET_HELPER")
	if mode == "" {
		return
	}
	decoder, encoder := json.NewDecoder(os.Stdin), json.NewEncoder(os.Stdout)
	var polls uint64
	for {
		var command Command
		if err := decoder.Decode(&command); err != nil {
			os.Exit(0)
		}
		response := Response{}
		if command.Operation == "progress" {
			if mode == "blocked" {
				for {
					time.Sleep(time.Hour)
				}
			}
			polls++
			writes := polls * 10
			if mode == "settles" {
				writes = min(writes, 40)
			}
			response.Stats.PublicationWrites = writes
		}
		if err := encoder.Encode(response); err != nil {
			os.Exit(2)
		}
		if command.Operation == "stop" {
			os.Exit(0)
		}
	}
}

func startQuietTestExporter(t *testing.T, mode string) *exporterProcess {
	t.Helper()
	t.Setenv("KARTA_LOAD_QUIET_HELPER", mode)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	helper := filepath.Join(dir, "exporter")
	script := "#!/bin/sh\nexec '" + strings.ReplaceAll(executable, "'", "'\\''") + "' -test.run=^TestPublicationQuietProcessHelper$ -- \"$@\"\n"
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
	if _, err := process.Request(ctx, Command{Operation: "status"}); err != nil {
		t.Fatal(err)
	}
	return process
}

func TestWaitPublicationQuietWaitsForStableWrites(t *testing.T) {
	process := startQuietTestExporter(t, "settles")
	quiet := 100 * time.Millisecond
	started := time.Now()
	delta, err := waitPublicationQuiet(t.Context(), 10*time.Second, quiet, process)
	if err != nil {
		t.Fatal(err)
	}
	if delta != 30 {
		t.Fatalf("publication delta = %d, want 30 from 10 to 40", delta)
	}
	if elapsed := time.Since(started); elapsed < quiet {
		t.Fatalf("returned after %s before quiet period %s", elapsed, quiet)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	response, err := process.Request(ctx, Command{Operation: "progress"})
	if err != nil || response.Stats.PublicationWrites != 40 {
		t.Fatalf("final progress = %+v, error = %v", response, err)
	}
	if err := process.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestWaitPublicationQuietCancellationReapsChild(t *testing.T) {
	for _, mode := range []string{"continuous", "blocked"} {
		t.Run(mode, func(t *testing.T) {
			process := startQuietTestExporter(t, mode)
			ctx, cancel := context.WithTimeout(t.Context(), 150*time.Millisecond)
			defer cancel()
			_, err := waitPublicationQuiet(ctx, 10*time.Second, time.Second, process)
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("wait error = %v, want deadline exceeded", err)
			}
			closed := make(chan error, 1)
			go func() { closed <- process.Close() }()
			select {
			case <-closed:
				// Cancellation can interrupt either a poll or the wait between polls.
			case <-time.After(5 * time.Second):
				_ = process.cmd.Process.Kill()
				t.Fatal("child cleanup exceeded bound")
			}
			select {
			case <-process.done:
			default:
				t.Fatal("child was not reaped")
			}
		})
	}
}
