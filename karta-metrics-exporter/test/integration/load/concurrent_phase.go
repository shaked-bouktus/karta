// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 NVIDIA Corporation

package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	inventory "github.com/dsx-ai-factory/workload-map/karta-metrics-exporter/pkg/kartadefinitioninventory"
	v1alpha1 "github.com/dsx-ai-factory/workload-map/karta/sdk/pkg/api/runai/v1alpha1"
)

type concurrentPhaseReport struct {
	Name               string                  `json:"name"`
	Outcome            string                  `json:"outcome"`
	Error              string                  `json:"error,omitempty"`
	Valid              bool                    `json:"valid"`
	Validation         string                  `json:"validation,omitempty"`
	WorkloadsUnchanged bool                    `json:"workloads_unchanged"`
	BarrierRelease     time.Time               `json:"barrier_release"`
	Mutations          []concurrentMutation    `json:"mutations"`
	Measurements       []ConcurrentMeasurement `json:"measurements"`
	QuiescenceSeconds  float64                 `json:"pre_load_quiescence_seconds"`
	ConvergenceSeconds float64                 `json:"mutation_to_validated_store_seconds"`
	API                APIStats                `json:"api"`
}

type concurrentMutation struct {
	Name    string           `json:"name"`
	Start   time.Time        `json:"start"`
	End     time.Time        `json:"end"`
	Updated inventory.Origin `json:"updated"`
	Error   string           `json:"error,omitempty"`
}

func updateConcurrent(ctx context.Context, definitions []*v1alpha1.Karta, update func(context.Context, *v1alpha1.Karta) error) (time.Time, []concurrentMutation) {
	gate := make(chan struct{})
	results := make([]concurrentMutation, len(definitions))
	var ready, done sync.WaitGroup
	ready.Add(len(definitions))
	for i, d := range definitions {
		done.Go(func() {
			ready.Done()
			<-gate
			result := concurrentMutation{Name: d.Name, Start: time.Now()}
			err := update(ctx, d)
			result.End = time.Now()
			if err != nil {
				result.Error = err.Error()
			} else {
				result.Updated = inventory.Origin{Source: inventory.SourceCluster, Name: d.Name, UID: d.UID, ResourceVersion: d.ResourceVersion}
			}
			results[i] = result
		})
	}
	ready.Wait()
	released := time.Now()
	close(gate)
	done.Wait()
	return released, results
}

func runConcurrentPhase(ctx context.Context, cfg options, child *exporterProcess, api client.Client, definitions []*v1alpha1.Karta, meter *APIMeter, fixtures []Fixture, currentPhase *atomic.Value, dir string) (report concurrentPhaseReport, resultErr error) {
	report = concurrentPhaseReport{Name: "karta-concurrent", Outcome: "incomplete"}
	defer func() {
		if resultErr != nil {
			report.Error = resultErr.Error()
			if report.Outcome == "incomplete" {
				report.Outcome = "concurrent_limit"
			}
		}
	}()
	currentPhase.Store("karta-concurrent-settle")
	settle := time.Now()
	if _, err := waitPublicationQuiet(ctx, cfg.StageTimeout, cfg.QuietPeriod, child); err != nil {
		return report, err
	}
	report.QuiescenceSeconds = time.Since(settle).Seconds()
	cohorts := concurrentCohorts(definitions, fixtures, true)
	if _, err := child.Request(ctx, Command{Operation: "arm-concurrent", Cohorts: cohorts}); err != nil {
		return report, err
	}
	for _, d := range definitions {
		root := &d.Spec.StructureDefinition.RootComponent
		root.Name += "-revised"
		root.StatusDefinition.StatusMappings.Running = root.StatusDefinition.StatusMappings.Initializing
		root.StatusDefinition.StatusMappings.Initializing = nil
		if err := v1alpha1.NewKartaValidator(d).Validate(); err != nil {
			return report, err
		}
	}
	currentPhase.Store("karta-concurrent")
	phaseCtx, cancel := context.WithTimeout(ctx, cfg.StageTimeout)
	defer cancel()
	meter.Begin()
	report.BarrierRelease, report.Mutations = updateConcurrent(phaseCtx, definitions, func(ctx context.Context, d *v1alpha1.Karta) error { return api.Update(ctx, d) })
	report.API = meter.Snapshot()
	for i, m := range report.Mutations {
		cohorts[i].Updated = m.Updated
		if m.Error != "" {
			resultErr = errors.Join(resultErr, fmt.Errorf("update %s: %s", m.Name, m.Error))
		}
	}
	if resultErr == nil {
		if _, err := child.Request(phaseCtx, Command{Operation: "bind-concurrent", Cohorts: cohorts}); err != nil {
			resultErr = err
		} else {
			_, resultErr = waitConcurrent(phaseCtx, cfg.StageTimeout, child)
		}
	}
	report.ConvergenceSeconds = time.Since(report.BarrierRelease).Seconds()
	collectCtx, cancelCollect := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancelCollect()
	response, err := child.Request(collectCtx, Command{Operation: "collect-concurrent"})
	resultErr = errors.Join(resultErr, err)
	report.Valid, report.Validation, report.Measurements = response.Valid, response.Validation, response.Concurrent
	health, healthErr := child.Request(collectCtx, Command{Operation: "health-trace"})
	if healthErr == nil {
		healthErr = writeJSON(filepath.Join(dir, "karta-concurrent-source-health.json"), health.HealthTrace)
	}
	resultErr = errors.Join(resultErr, healthErr)
	for i := range report.Measurements {
		m := &report.Measurements[i]
		resultErr = errors.Join(resultErr, writeJSON(filepath.Join(dir, m.Definition.Name+"-concurrent-samples.json"), m.Samples))
		m.Samples = nil
	}
	if resultErr == nil && report.Valid {
		currentPhase.Store("karta-concurrent-identity-validation")
		verifyCtx, cancelVerify := context.WithTimeout(ctx, cfg.StageTimeout)
		err := verifyConcurrentIdentities(verifyCtx, api, fixtures)
		cancelVerify()
		resultErr = errors.Join(resultErr, err)
		report.WorkloadsUnchanged = err == nil
	}
	if report.API.Overflow != 0 || report.API.Failed != 0 {
		resultErr = errors.Join(resultErr, errors.New("concurrent API meter failed or overflowed"))
	}
	if !report.Valid {
		resultErr = errors.Join(resultErr, fmt.Errorf("concurrent Store validation failed: %s", report.Validation))
	}
	if resultErr == nil {
		report.Outcome = "passed_concurrent_definitions"
	}
	return report, resultErr
}

func waitConcurrent(ctx context.Context, budget time.Duration, child *exporterProcess) (Response, error) {
	phaseCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	var response Response
	for {
		if phaseCtx.Err() != nil {
			return response, context.Cause(phaseCtx)
		}
		requestCtx, cancelRequest := context.WithTimeout(context.WithoutCancel(phaseCtx), 30*time.Second)
		next, err := child.Request(requestCtx, Command{Operation: "status-concurrent"})
		cancelRequest()
		response = next
		if err != nil {
			return response, err
		}
		if phaseCtx.Err() != nil {
			return response, context.Cause(phaseCtx)
		}
		if response.Valid {
			return response, nil
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-phaseCtx.Done():
			timer.Stop()
			return response, context.Cause(phaseCtx)
		case <-timer.C:
		}
	}
}
