// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 NVIDIA Corporation

package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync/atomic"
	"time"

	"k8s.io/client-go/kubernetes"

	"github.com/dsx-ai-factory/workload-map/karta-metrics-exporter/pkg/store"
)

type Arrival struct {
	Index           int       `json:"index"`
	Scheduled       time.Time `json:"scheduled"`
	Dispatch        time.Time `json:"dispatch"`
	WorkerStart     time.Time `json:"worker_start"`
	End             time.Time `json:"end"`
	StartLagNS      int64     `json:"start_lag_ns"`
	Reason          string    `json:"reason"`
	Inflight        int       `json:"inflight_at_dispatch_or_rejection"`
	InflightAtStart int       `json:"inflight_at_worker_start"`
	APIError        bool      `json:"api_error"`
}

type HealthEvent struct {
	Time    time.Time          `json:"time"`
	Source  store.SourceKey    `json:"source"`
	Health  store.SourceHealth `json:"health"`
	Removed bool               `json:"removed"`
}

type HealthTrace struct {
	Events   []HealthEvent `json:"events"`
	Overflow int           `json:"overflow"`
}

func workerRejection(ctx context.Context, paced bool, scheduled, began time.Time, budget time.Duration) string {
	if ctx.Err() != nil {
		return "canceled"
	}
	if paced && exceedsStartLag(scheduled, began, budget) {
		return "worker_late"
	}
	return ""
}

func (cfg options) validateDiagnostics() error {
	if cfg.DiagnosticRootOnly || cfg.DiagnosticAPIOnly {
		if cfg.Scenario != "workload" {
			return errors.New("diagnostic modes require workload scenario")
		}
		if len(cfg.Rates) != 1 {
			return errors.New("diagnostic modes require exactly one rate")
		}
	}
	for _, roots := range cfg.Roots {
		if roots > cfg.MaxSamples {
			return errors.New("arrival diagnostics require max-samples >= roots")
		}
	}
	return nil
}

func (r *Recorder) SetSourceHealth(key store.SourceKey, health store.SourceHealth) {
	at := time.Now()
	r.PublicationStore.SetSourceHealth(key, health)
	r.recordHealth(HealthEvent{Time: at, Source: key, Health: health})
}

func (r *Recorder) RemoveSourceHealth(key store.SourceKey) {
	at := time.Now()
	r.PublicationStore.RemoveSourceHealth(key)
	r.recordHealth(HealthEvent{Time: at, Source: key, Removed: true})
}

func (r *Recorder) recordHealth(event HealthEvent) {
	r.healthMu.Lock()
	defer r.healthMu.Unlock()
	if len(r.healthTrace.Events) < r.maxSamples {
		r.healthTrace.Events = append(r.healthTrace.Events, event)
	} else {
		r.healthTrace.Overflow++
	}
}

func (r *Recorder) HealthTrace() HealthTrace {
	r.healthMu.Lock()
	defer r.healthMu.Unlock()
	return HealthTrace{Events: append([]HealthEvent(nil), r.healthTrace.Events...), Overflow: r.healthTrace.Overflow}
}

func runAPIOnly(ctx context.Context, cfg options, api kubernetes.Interface, meter *APIMeter, fixtures []Fixture, phase *atomic.Value, dir string) (report phaseReport, resultErr error) {
	report = phaseReport{Name: "diagnostic-api-only-root-update", OfferedRate: cfg.Rates[0], Validation: "No exporter started; Store correctness and reflector latency are not measured", Outcome: "incomplete"}
	phase.Store("diagnostic-api-only-root-update")
	meter.Begin()
	arrivals := make([]Arrival, min(len(fixtures), cfg.MaxSamples))
	loadCtx, cancel := context.WithTimeout(ctx, cfg.StageTimeout)
	defer cancel()
	report.Load = runLoad(loadCtx, len(fixtures), cfg.Rates[0], cfg.Parallel, true, cfg.MaxStartLag, func(ctx context.Context, i int) error { return updateRoot(ctx, api, &fixtures[i], "1") }, arrivals)
	report.API = meter.Snapshot()
	if report.Load.DurationSeconds > 0 {
		report.AchievedRate = float64(report.Load.Completed) / report.Load.DurationSeconds
	}
	switch {
	case report.Load.Errors > 0 || report.API.Failed > 0:
		report.Outcome = "api_or_generator_error"
		resultErr = fmt.Errorf("API/generator errors: %s", report.Load.FirstError)
	case report.Load.Missed > 0:
		report.Outcome = "generator_limited"
		resultErr = fmt.Errorf("generator missed %d/%d arrivals", report.Load.Missed, report.Load.Scheduled)
	case report.API.Overflow > 0:
		report.Outcome = "instrumentation_limit"
		resultErr = errors.New("API sample budget exceeded")
	default:
		report.Outcome = "passed_diagnostic_api_only_wave"
	}
	resultErr = errors.Join(resultErr, writeJSON(filepath.Join(dir, "diagnostic-api-only-root-update-arrivals.json"), arrivals))
	if resultErr != nil {
		report.Error = resultErr.Error()
	}
	return report, resultErr
}
