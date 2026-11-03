// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 NVIDIA Corporation

package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/dsx-ai-factory/workload-map/karta-metrics-exporter/pkg/store"
)

func TestDiagnosticMissAccounting(t *testing.T) {
	for _, tc := range []struct {
		name     string
		budget   time.Duration
		expected string
	}{
		{"full", time.Second, "slots_full"},
		{"scheduler", time.Nanosecond, "scheduler_late"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			arrivals := make([]Arrival, 5)
			result := runLoad(t.Context(), 5, 100, 1, true, tc.budget, func(context.Context, int) error { time.Sleep(100 * time.Millisecond); return nil }, arrivals)
			started, missed := 0, 0
			for i, a := range arrivals {
				if a.Index != i || a.End.IsZero() {
					t.Fatalf("incomplete arrival: %+v", a)
				}
				if a.Reason == "started" {
					started++
					if a.WorkerStart.IsZero() {
						t.Fatal("missing worker start")
					}
				} else {
					missed++
					if a.Reason != tc.expected {
						t.Fatalf("unexpected reason: %+v", a)
					}
				}
			}
			if missed == 0 || missed != result.Missed || started != result.Started {
				t.Fatalf("trace accounting: %+v", result)
			}
		})
	}
}

func TestDiagnosticCanceledAndWorkerLate(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	arrivals := make([]Arrival, 3)
	result := runLoad(ctx, 3, 100, 1, true, time.Second, func(context.Context, int) error { t.Fatal("called after cancel"); return nil }, arrivals)
	for _, a := range arrivals {
		if a.Reason != "canceled" || a.End.IsZero() {
			t.Fatalf("canceled trace %+v", a)
		}
	}
	if result.Missed != 3 {
		t.Fatalf("bad cancellation: %+v", result)
	}
	now := time.Now()
	if got := workerRejection(t.Context(), true, now, now.Add(time.Second), time.Millisecond); got != "worker_late" {
		t.Fatal(got)
	}
	if got := workerRejection(ctx, true, now, now.Add(time.Second), time.Millisecond); got != "canceled" {
		t.Fatal(got)
	}
	if got := workerRejection(t.Context(), false, now, now.Add(time.Second), time.Millisecond); got != "" {
		t.Fatal(got)
	}
}

func TestDiagnosticOptions(t *testing.T) {
	cfg := options{Scenario: "workload", Roots: []int{10}, Rates: []float64{100}, MaxSamples: 10, DiagnosticAPIOnly: true}
	if err := cfg.validateDiagnostics(); err != nil {
		t.Fatal(err)
	}
	cfg.Scenario = "karta-update"
	if cfg.validateDiagnostics() == nil {
		t.Fatal("accepted nonworkload")
	}
	cfg.Scenario = "workload"
	cfg.Rates = nil
	if cfg.validateDiagnostics() == nil {
		t.Fatal("accepted missing rate")
	}
	cfg.Rates = []float64{100}
	cfg.MaxSamples = 9
	if cfg.validateDiagnostics() == nil {
		t.Fatal("accepted truncated arrivals")
	}
}

func TestDiagnosticHealthDelegatesAndBounds(t *testing.T) {
	target := store.New()
	r := NewRecorder(target, 2)
	key := store.SourceKey{}
	r.SetSourceHealth(key, store.SourceHealth{Synced: true})
	r.SetSourceHealth(key, store.SourceHealth{Error: "api_unavailable"})
	r.RemoveSourceHealth(key)
	trace := r.HealthTrace()
	if len(trace.Events) != 2 || trace.Overflow != 1 {
		t.Fatalf("bad health bounds: %+v", trace)
	}
	if trace.Events[0].Time.IsZero() || !trace.Events[0].Health.Synced || trace.Events[1].Health.Error != "api_unavailable" {
		t.Fatal(trace)
	}
	if len(target.Capture().Sources) != 0 {
		t.Fatal("remove was not delegated")
	}
	trace.Events[0].Health.Error = "mutated"
	if r.HealthTrace().Events[0].Health.Error != "" {
		t.Fatal("snapshot aliases recorder")
	}
	r.Begin()
	if len(r.HealthTrace().Events) != 2 {
		t.Fatal("phase reset lost health evidence")
	}
}

func TestDiagnosticAPIOnlyUsesRootPatchAndNoMeasurement(t *testing.T) {
	api := fake.NewClientset(&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "root", Namespace: "load"}})
	fixtures := []Fixture{{Name: "root", Namespace: "load"}}
	cfg := options{Rates: []float64{100}, Parallel: 1, MaxSamples: 10, MaxStartLag: time.Second, StageTimeout: time.Second}
	var phase atomic.Value
	dir := t.TempDir()
	report, err := runAPIOnly(t.Context(), cfg, api, NewAPIMeter(10), fixtures, &phase, dir)
	if err != nil {
		t.Fatal(err)
	}
	if report.Measurement != nil || report.Valid || report.Outcome != "passed_diagnostic_api_only_wave" {
		t.Fatalf("misleading report: %+v", report)
	}
	actions := api.Actions()
	if len(actions) != 1 || actions[0].GetVerb() != "patch" || actions[0].GetResource().Resource != "deployments" {
		t.Fatalf("unexpected actions: %+v", actions)
	}
	data, err := os.ReadFile(filepath.Join(dir, "diagnostic-api-only-root-update-arrivals.json"))
	if err != nil {
		t.Fatal(err)
	}
	var arrivals []Arrival
	if err := json.Unmarshal(data, &arrivals); err != nil {
		t.Fatal(err)
	}
	if len(arrivals) != 1 || arrivals[0].Reason != "started" || report.Load.Completed != 1 {
		t.Fatal(report, arrivals)
	}
}
