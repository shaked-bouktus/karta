// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 NVIDIA Corporation

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestExporterProcessHelper(t *testing.T) {
	if os.Getenv("KARTA_LOAD_PROCESS_HELPER") != "1" {
		return
	}
	decoder, encoder := json.NewDecoder(os.Stdin), json.NewEncoder(os.Stdout)
	fmt.Fprintln(os.Stderr, "helper exporter log")
	for {
		var command Command
		if err := decoder.Decode(&command); err != nil {
			os.Exit(0)
		}
		response := Response{Validation: command.Operation, Ready: true}
		switch command.Operation {
		case "hang":
			for {
				time.Sleep(time.Hour)
			}
		case "fail":
			response.Error = "command failure"
		}
		if err := encoder.Encode(response); err != nil {
			os.Exit(2)
		}
		if command.Operation == "stop" {
			os.Exit(0)
		}
	}
}

func startTestExporter(t *testing.T) (*exporterProcess, string) {
	t.Helper()
	t.Setenv("KARTA_LOAD_PROCESS_HELPER", "1")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	helper := filepath.Join(dir, "exporter")
	// The shell only redirects the requested child invocation into this test helper.
	script := "#!/bin/sh\nexec '" + strings.ReplaceAll(executable, "'", "'\\''") + "' -test.run=^TestExporterProcessHelper$ -- \"$@\"\n"
	if err := os.WriteFile(helper, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(dir, "exporter.log")
	process, err := startExporter(t.Context(), helper, "unused", logPath, 100, 2, "workload")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = process.Close() })
	return process, logPath
}

func TestExporterProcessSerializesRequestsAndCloses(t *testing.T) {
	process, logPath := startTestExporter(t)
	if process.PID() <= 0 {
		t.Fatal("missing child PID")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	for i := range 6 {
		wg.Go(func() {
			operation := fmt.Sprintf("request-%d", i)
			response, err := process.Request(ctx, Command{Operation: operation})
			if err != nil || response.Validation != operation {
				t.Errorf("response=%+v error=%v", response, err)
			}
		})
	}
	wg.Wait()
	if _, err := process.Request(ctx, Command{Operation: "fail"}); err == nil || err.Error() != "command failure" {
		t.Fatalf("command error=%v", err)
	}
	if response, err := process.Request(ctx, Command{Operation: "status"}); err != nil || !response.Ready {
		t.Fatalf("request after command failure: %+v, %v", response, err)
	}
	if err := process.Close(); err != nil {
		t.Fatal(err)
	}
	if err := process.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := process.Request(ctx, Command{Operation: "status"}); err == nil {
		t.Fatal("request after Close succeeded")
	}
	info, err := os.Stat(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("log mode=%v", info.Mode().Perm())
	}
	data, err := os.ReadFile(logPath)
	if err != nil || !strings.Contains(string(data), "helper exporter log") {
		t.Fatalf("log=%q error=%v", data, err)
	}
	select {
	case <-process.done:
	default:
		t.Fatal("Close did not reap child")
	}
}

func TestExporterProcessRequestTimeoutKillsAndReaps(t *testing.T) {
	process, _ := startTestExporter(t)
	readyCtx, readyCancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer readyCancel()
	if _, err := process.Request(readyCtx, Command{Operation: "status"}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if _, err := process.Request(ctx, Command{Operation: "hang"}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("request error=%v", err)
	}
	select {
	case <-process.done:
	default:
		t.Fatal("timed-out child was not reaped")
	}
	first, second := process.Close(), process.Close()
	if first == nil || second == nil || first.Error() != second.Error() {
		t.Fatalf("Close errors=%v, %v", first, second)
	}
}

func TestStartExporterRejectsMissingExecutable(t *testing.T) {
	dir := t.TempDir()
	if _, err := startExporter(t.Context(), filepath.Join(dir, "missing"), "unused", filepath.Join(dir, "log"), 1, 1, "workload"); err == nil {
		t.Fatal("expected launch error")
	}
}
