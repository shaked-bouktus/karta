// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 NVIDIA Corporation

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime/pprof"
	"time"

	"k8s.io/client-go/tools/clientcmd"

	"github.com/dsx-ai-factory/workload-map/karta-metrics-exporter/pkg/app"
	"github.com/dsx-ai-factory/workload-map/karta-metrics-exporter/pkg/bootstrap"
	"github.com/dsx-ai-factory/workload-map/karta-metrics-exporter/pkg/config"
	"github.com/dsx-ai-factory/workload-map/karta-metrics-exporter/pkg/labelresolver"
	"github.com/dsx-ai-factory/workload-map/karta-metrics-exporter/pkg/store"
)

func runChild(ctx context.Context, kubeconfig string, maxSamples, workers int, scenario string) (resultErr error) {
	kubeConfig, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return fmt.Errorf("load child kubeconfig: %w", err)
	}
	cfg := config.DefaultConfig()
	cfg.KartaDefinitionSource.Catalog.Enabled = true
	cfg.KartaDefinitionSource.Catalog.Include = []string{"apps-deployment-v1"}
	cfg.KartaDefinitionSource.Cluster.Enabled = false
	if scenario == "karta-update" || scenario == "karta-concurrent" {
		cfg.KartaDefinitionSource.Catalog.Enabled = false
		cfg.KartaDefinitionSource.Cluster.Enabled = true
		cfg.KartaDefinitionSource.Cluster.Include = []string{liveDefinitionName}
		if scenario == "karta-concurrent" {
			cfg.KartaDefinitionSource.Cluster.Include = concurrentNames()
		}
	}
	if workers != 0 {
		cfg.Workers = workers
	}
	cfg.LeafWorkers, cfg.RootWorkers = 0, 0
	cfg.Rules = []labelresolver.LabelRule{
		{Name: RootMetricLabel, Metrics: []string{labelresolver.MetricPodWorkloadInfo, labelresolver.MetricWorkloadStatus}, Sources: []labelresolver.Source{labelresolver.SourceWorkload}, Expression: fmt.Sprintf("object.metadata.labels[%q]", RootRevisionLabel)},
		{Name: PodMetricLabel, Metrics: []string{labelresolver.MetricPodWorkloadInfo}, Sources: []labelresolver.Source{labelresolver.SourcePod}, Expression: fmt.Sprintf("object.metadata.labels[%q]", PodRevisionLabel)},
	}
	recorder := NewRecorder(store.New(), maxSamples)
	components, err := bootstrap.NewWithOptions(cfg, kubeConfig, bootstrap.Options{Publications: recorder, ObserveEvent: recorder.ObserveEvent, ObserveDefinition: recorder.ObserveDefinition})
	if err != nil {
		return fmt.Errorf("construct child exporter: %w", err)
	}
	runCtx, cancelRun := context.WithCancel(ctx)
	runDone := make(chan error, 1)
	go func() { runDone <- app.RunController(runCtx, components) }()
	var runtimeErr error
	defer func() {
		cancelRun()
		if runDone != nil {
			timer := time.NewTimer(15 * time.Second)
			defer timer.Stop()
			select {
			case err := <-runDone:
				runtimeErr = err
			case <-timer.C:
				runtimeErr = errors.New("exporter shutdown exceeded 15 seconds")
			}
		}
		resultErr = errors.Join(resultErr, runtimeErr)
	}()

	decodeCtx, cancelDecode := context.WithCancel(ctx)
	commands := make(chan Command)
	decodeErrors := make(chan error, 1)
	go func() {
		decoder := json.NewDecoder(os.Stdin)
		for {
			var command Command
			if err := decoder.Decode(&command); err != nil {
				decodeErrors <- err
				return
			}
			select {
			case commands <- command:
				if command.Operation == "stop" {
					return
				}
			case <-decodeCtx.Done():
				return
			}
		}
	}()
	// The decoder belongs to this child process. On cancellation, a blocking stdin
	// read may survive Close; joining it would prevent controller shutdown. A stop
	// command exits the decoder above, and process exit handles other blocked reads.
	defer cancelDecode()
	encoder := json.NewEncoder(os.Stdout)
	var expected Command
	var cohorts []ConcurrentCohort
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-runDone:
			runtimeErr = err
			if runtimeErr == nil {
				runtimeErr = errors.New("exporter stopped before child shutdown")
			}
			runDone = nil
		case err := <-decodeErrors:
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("decode child command: %w", err)
		case command := <-commands:
			response := Response{}
			switch command.Operation {
			case "expect-concurrent":
				cohorts = command.Cohorts
			case "arm-concurrent":
				cohorts = command.Cohorts
				if err := recorder.armConcurrent(cohorts); err != nil {
					response.Error = err.Error()
				}
			case "bind-concurrent":
				if err := recorder.bindConcurrent(command.Cohorts); err != nil {
					response.Error = err.Error()
				} else {
					cohorts = command.Cohorts
				}
			case "status-concurrent", "collect-concurrent":
				response.Ready = components.Runtime.Ready()
				response.Concurrent = recorder.measureConcurrent(command.Operation == "collect-concurrent")
				response.Valid = response.Ready
				if err := validateConcurrent(components.Store.Capture(), cohorts); err != nil {
					response.Valid = false
					response.Validation = err.Error()
				}
				for _, m := range response.Concurrent {
					if !m.Observed || m.Missing != 0 || m.Overflow != 0 {
						response.Valid = false
						response.Validation = "concurrent measurement incomplete"
					}
				}
			case "health-trace":
				trace := recorder.HealthTrace()
				response.HealthTrace = &trace
			case "begin":
				recorder.Begin()
				expected = Command{}
			case "expect":
				expected = command
			case "arm-definition":
				if command.Definition == nil || command.DefinitionOrigin == nil {
					response.Error = "definition expectation and previous origin are required"
				} else if err := recorder.ArmDefinition(*command.DefinitionOrigin, command.Fixtures, command.Definition.Component, command.Definition.Phase); err != nil {
					response.Error = err.Error()
				} else {
					expected = command
				}
			case "progress":
				// Readiness takes the controller state lock. Progress must not wait
				// behind inventory scans while observing an overloaded exporter.
				response.Stats = recorder.Stats()
			case "status", "collect":
				response.Ready = components.Runtime.Ready()
				response.Stats = recorder.Stats()
				view := components.Store.Capture()
				response.Pods, response.Workloads = len(view.Pods), len(view.Workloads)
				var missing, overflow int
				definitionObserved := true
				if expected.DefinitionOrigin != nil {
					measurement := recorder.MeasureDefinition(command.Operation == "collect")
					response.DefinitionMeasurement = &measurement
					missing, overflow = measurement.Missing, measurement.Overflow
					definitionObserved = measurement.Observed && measurement.Definition == *expected.DefinitionOrigin
				} else {
					var measurement Measurement
					if command.Operation == "collect" {
						measurement = recorder.Measure(expected.Expected)
					} else {
						measurement = recorder.MeasureProgress(expected.Expected)
					}
					response.Measurement = &measurement
					missing, overflow = measurement.Missing, measurement.Overflow
				}
				count := len(expected.Fixtures)
				if expected.Deleted {
					count = 0
				}
				switch {
				case !response.Ready:
					response.Validation = "exporter is not ready"
				case response.Pods != count || response.Workloads != count:
					response.Validation = fmt.Sprintf("publication counts: pods=%d workloads=%d expected=%d each", response.Pods, response.Workloads, count)
				case !definitionObserved:
					response.Validation = "expected Karta resource version was not observed"
				case missing != 0 || overflow != 0:
					response.Validation = fmt.Sprintf("incomplete measurement: missing=%d overflow=%d", missing, overflow)
				default:
					var err error
					if expected.Definition != nil {
						err = validateFixturesWithDefinition(view, expected.Fixtures, expected.RootRevision, expected.PodRevision, expected.Deleted, *expected.Definition)
					} else {
						err = validateFixtures(view, expected.Fixtures, expected.RootRevision, expected.PodRevision, expected.Deleted)
					}
					if err != nil {
						response.Validation = err.Error()
					} else {
						response.Valid = true
					}
				}
			case "heap":
				profile, err := os.OpenFile(command.ProfilePath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
				if err != nil {
					response.Error = fmt.Sprintf("create heap profile: %v", err)
				} else {
					err = pprof.Lookup("heap").WriteTo(profile, 0)
					err = errors.Join(err, profile.Close())
					if err != nil {
						response.Error = fmt.Sprintf("write heap profile: %v", err)
					}
				}
			case "stop":
				if err := encoder.Encode(response); err != nil {
					return fmt.Errorf("encode child response: %w", err)
				}
				return nil
			default:
				response.Error = fmt.Sprintf("unknown operation %q", command.Operation)
			}
			if runtimeErr != nil {
				response.Error = runtimeErr.Error()
				response.Valid = false
				response.Ready = false
			}
			if err := encoder.Encode(response); err != nil {
				return fmt.Errorf("encode child response: %w", err)
			}
		}
	}
}
