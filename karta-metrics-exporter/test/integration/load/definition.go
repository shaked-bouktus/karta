// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 NVIDIA Corporation

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	inventory "github.com/dsx-ai-factory/workload-map/karta-metrics-exporter/pkg/kartadefinitioninventory"
	v1alpha1 "github.com/dsx-ai-factory/workload-map/karta/sdk/pkg/api/runai/v1alpha1"
	"github.com/dsx-ai-factory/workload-map/karta/sdk/pkg/catalog"
)

type definitionPhaseReport struct {
	Name               string                 `json:"name"`
	Outcome            string                 `json:"outcome"`
	Error              string                 `json:"error,omitempty"`
	Valid              bool                   `json:"valid"`
	Validation         string                 `json:"validation,omitempty"`
	PreviousDefinition inventory.Origin       `json:"previous_definition"`
	UpdatedDefinition  inventory.Origin       `json:"updated_definition"`
	ExpectedDefinition DefinitionExpectation  `json:"expected_definition"`
	QuiescenceSeconds  float64                `json:"pre_load_quiescence_seconds"`
	QuiescenceWrites   uint64                 `json:"pre_load_publication_writes"`
	MutationSeconds    float64                `json:"mutation_seconds"`
	ConvergenceSeconds float64                `json:"mutation_to_validated_store_seconds"`
	API                APIStats               `json:"api"`
	Measurement        *DefinitionMeasurement `json:"definition_to_store"`
}

const liveDefinitionName = "load-deployment"
const revisedComponent = "deployment-revised"

func newLiveDefinition() (*v1alpha1.Karta, error) {
	definition, err := catalog.Get(schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"})
	if err != nil {
		return nil, err
	}
	definition.Name = liveDefinitionName
	return definition, nil
}

func reviseLiveDefinition(definition *v1alpha1.Karta) {
	root := &definition.Spec.StructureDefinition.RootComponent
	previousName := root.Name
	root.Name = revisedComponent
	for index := range definition.Spec.StructureDefinition.ChildComponents {
		component := &definition.Spec.StructureDefinition.ChildComponents[index]
		if component.OwnerRef != nil && *component.OwnerRef == previousName {
			component.OwnerRef = ptr.To(revisedComponent)
		}
	}
	mappings := &root.StatusDefinition.StatusMappings
	mappings.Running = mappings.Initializing
	mappings.Initializing = nil
}

func runDefinitionPhase(ctx context.Context, cfg options, child *exporterProcess, api client.Client, definition *v1alpha1.Karta, meter *APIMeter, fixtures []Fixture, currentPhase *atomic.Value, dir string) (report definitionPhaseReport, resultErr error) {
	report = definitionPhaseReport{Name: "karta-update", Outcome: "incomplete", ExpectedDefinition: DefinitionExpectation{Name: liveDefinitionName, Source: "cluster", Component: revisedComponent, Phase: "Running"}}
	defer func() {
		if resultErr != nil {
			report.Error = resultErr.Error()
			if report.Outcome == "incomplete" {
				report.Outcome = "infrastructure_or_budget_limit"
			}
		}
	}()
	currentPhase.Store("karta-update-settle")
	settleStart := time.Now()
	writes, err := waitPublicationQuiet(ctx, cfg.StageTimeout, cfg.QuietPeriod, child)
	report.QuiescenceSeconds, report.QuiescenceWrites = time.Since(settleStart).Seconds(), writes
	if err != nil {
		report.Outcome = "quiescence_limit"
		return report, err
	}
	report.PreviousDefinition = inventory.Origin{Source: inventory.SourceCluster, Name: definition.Name, UID: definition.UID, ResourceVersion: definition.ResourceVersion}
	expectation := Command{Operation: "arm-definition", Fixtures: fixtures, RootRevision: "0", PodRevision: "0", Definition: &report.ExpectedDefinition, DefinitionOrigin: &report.PreviousDefinition}
	if _, err := child.Request(ctx, expectation); err != nil {
		return report, err
	}
	phaseCtx, cancelPhase := context.WithTimeout(ctx, cfg.StageTimeout)
	defer cancelPhase()
	progressCtx, stopProgress := context.WithCancel(ctx)
	progressDone := make(chan error, 1)
	go func() {
		progressDone <- monitorProgress(progressCtx, child, filepath.Join(dir, "karta-update-progress.jsonl"))
	}()
	defer func() {
		stopProgress()
		if err := <-progressDone; err != nil {
			report.Outcome = "instrumentation_limit"
			resultErr = errors.Join(resultErr, err)
		}
	}()
	currentPhase.Store("karta-update")
	fmt.Fprintf(os.Stderr, "karta-update: one definition, %d roots and %d Pods\n", len(fixtures), len(fixtures))
	reviseLiveDefinition(definition)
	meter.Begin()
	mutationStart := time.Now()
	updateErr := api.Update(phaseCtx, definition)
	report.MutationSeconds = time.Since(mutationStart).Seconds()
	report.API = meter.Snapshot()
	var response Response
	var expectationErr, convergenceErr error
	if updateErr == nil {
		report.UpdatedDefinition = inventory.Origin{Source: inventory.SourceCluster, Name: definition.Name, UID: definition.UID, ResourceVersion: definition.ResourceVersion}
		expectation.Operation, expectation.DefinitionOrigin = "expect", &report.UpdatedDefinition
		expectCtx, cancelExpect := context.WithTimeout(ctx, 30*time.Second)
		_, expectationErr = child.Request(expectCtx, expectation)
		cancelExpect()
		if expectationErr == nil {
			response, convergenceErr = waitDefinitionConvergence(phaseCtx, child, 2*len(fixtures))
		}
	}
	report.ConvergenceSeconds = time.Since(mutationStart).Seconds()
	if ctx.Err() == nil {
		// The polling phase may expire while the child is still available for partial results.
		collectCtx, cancelCollect := context.WithTimeout(ctx, 30*time.Second)
		collected, collectErr := child.Request(collectCtx, Command{Operation: "collect"})
		cancelCollect()
		if collectErr == nil {
			response = collected
		} else {
			convergenceErr = errors.Join(convergenceErr, collectErr)
		}
	}
	report.Valid, report.Validation, report.Measurement = response.Valid, response.Validation, response.DefinitionMeasurement
	if updateErr != nil || expectationErr != nil {
		report.Valid = false
	}
	switch {
	case ctx.Err() != nil:
		report.Outcome, resultErr = "infrastructure_or_budget_limit", context.Cause(ctx)
	case updateErr != nil || report.API.Failed != 0:
		report.Outcome, resultErr = "api_error", errors.Join(errors.New("karta update API request failed"), updateErr, convergenceErr)
	case expectationErr != nil:
		report.Outcome, resultErr = "instrumentation_limit", errors.Join(fmt.Errorf("set updated definition expectation: %w", expectationErr), convergenceErr)
	case report.API.Overflow != 0 || (report.Measurement != nil && report.Measurement.Overflow != 0):
		report.Outcome, resultErr = "instrumentation_limit", errors.New("definition measurement sample budget exceeded")
	case convergenceErr != nil || !report.Valid:
		report.Outcome = "convergence_limit"
		resultErr = errors.Join(fmt.Errorf("karta update did not converge: %s", response.Validation), convergenceErr)
	default:
		report.Outcome = "passed_definition_update"
	}
	if report.Measurement != nil {
		if err := writeJSON(filepath.Join(dir, "karta-update-samples.json"), report.Measurement.Samples); err != nil {
			report.Outcome, resultErr = "artifact_error", errors.Join(resultErr, err)
		}
		report.Measurement.Samples = nil
		for _, result := range []struct {
			name    string
			latency *Distribution
		}{{"roots", report.Measurement.Roots}, {"pods", report.Measurement.Pods}} {
			if result.latency != nil {
				fmt.Fprintf(os.Stderr, "karta-update %s: p50=%.2fms p90=%.2fms p99=%.2fms max=%.2fms samples=%d\n", result.name, result.latency.P50MS, result.latency.P90MS, result.latency.P99MS, result.latency.MaxMS, result.latency.Count)
			}
		}
		fmt.Fprintf(os.Stderr, "karta-update: completed=%d/%d validated=%.2fs outcome=%s\n", report.Measurement.Completed, report.Measurement.Expected, report.ConvergenceSeconds, report.Outcome)
	}
	if ctx.Err() == nil {
		profileCtx, cancelProfile := context.WithTimeout(ctx, 30*time.Second)
		_, err := child.Request(profileCtx, Command{Operation: "heap", ProfilePath: filepath.Join(dir, "karta-update.heap")})
		cancelProfile()
		if err != nil {
			report.Outcome, resultErr = "artifact_error", errors.Join(resultErr, err)
		}
	}
	return report, resultErr
}

func waitDefinitionConvergence(ctx context.Context, child *exporterProcess, expected int) (Response, error) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	var response Response
	for {
		if ctx.Err() != nil {
			return response, context.Cause(ctx)
		}
		// Expiring the phase between polls preserves the child for partial collection.
		// A separate watchdog bounds each request without interrupting its protocol.
		requestCtx, cancelRequest := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		progress, err := child.Request(requestCtx, Command{Operation: "progress"})
		cancelRequest()
		if err != nil {
			return response, err
		}
		if ctx.Err() != nil {
			return response, context.Cause(ctx)
		}
		if progress.Stats.Overflow != 0 {
			return response, errors.New("definition recorder overflow")
		}
		if progress.Stats.Observed == expected && progress.Stats.Completed == expected {
			requestCtx, cancelRequest := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			response, err = child.Request(requestCtx, Command{Operation: "status"})
			cancelRequest()
			if ctx.Err() != nil {
				return response, errors.Join(err, context.Cause(ctx))
			}
			if err != nil || response.Valid {
				return response, err
			}
		}
		select {
		case <-ctx.Done():
			return response, context.Cause(ctx)
		case <-ticker.C:
		}
	}
}
