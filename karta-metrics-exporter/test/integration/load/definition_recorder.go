// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 NVIDIA Corporation

package main

import (
	"errors"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/types"

	inventory "github.com/dsx-ai-factory/workload-map/karta-metrics-exporter/pkg/kartadefinitioninventory"
	"github.com/dsx-ai-factory/workload-map/karta-metrics-exporter/pkg/store"
)

type DefinitionMeasurement struct {
	Definition inventory.Origin `json:"definition"`
	Observed   bool             `json:"observed"`
	Expected   int              `json:"expected"`
	Completed  int              `json:"completed"`
	Missing    int              `json:"missing"`
	Overflow   int              `json:"overflow"`
	Roots      *Distribution    `json:"roots"`
	Pods       *Distribution    `json:"pods"`
	Samples    []Sample         `json:"samples"`
}

type definitionRun struct {
	previous, observed inventory.Origin
	component, phase   string
	start              time.Time
	events             map[store.ObjectRef]*definitionTiming
}

type definitionTiming struct {
	root store.ObjectRef
	end  time.Time
}

func (r *Recorder) ArmDefinition(previous inventory.Origin, fixtures []Fixture, component, phase string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.beginLocked()
	if previous.Source != inventory.SourceCluster || previous.UID == "" || previous.Name == "" || previous.ResourceVersion == "" || component == "" || phase == "" {
		return errors.New("definition recording requires a versioned cluster origin and semantic targets")
	}
	if len(fixtures) > r.maxSamples/2 {
		r.overflow = 2*len(fixtures) - r.maxSamples
		return fmt.Errorf("definition fanout requires %d samples, budget is %d", 2*len(fixtures), r.maxSamples)
	}
	next := &definitionRun{previous: previous, component: component, phase: phase, events: make(map[store.ObjectRef]*definitionTiming, 2*len(fixtures))}
	seen := make(map[types.UID]bool, 2*len(fixtures))
	for _, fixture := range fixtures {
		refs := fixtureExpectations(fixture, false)
		for _, item := range refs {
			ref := item.Ref
			if ref.UID == "" || ref.Name == "" || ref.ResourceVersion == "" || seen[ref.UID] {
				return errors.New("definition recording requires unique versioned fixture identities")
			}
			seen[ref.UID] = true
			next.events[ref] = &definitionTiming{root: refs[0].Ref}
		}
	}
	r.definition = next
	return nil
}

func (r *Recorder) ObserveDefinition(origin inventory.Origin) {
	start := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.concurrent != nil {
		r.concurrent.observe(origin, start)
		return
	}
	d := r.definition
	if d == nil || !d.start.IsZero() || origin.Source != d.previous.Source || origin.Name != d.previous.Name || origin.UID != d.previous.UID || origin.ResourceVersion == "" || origin.ResourceVersion == d.previous.ResourceVersion {
		return
	}
	d.start, d.observed = start, origin
}

func (r *Recorder) completeDefinitionPod(result *store.PodResult, end time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	d := r.definition
	if d == nil || !result.HasValue || result.Error != "" || result.Record.OwnerResolution != store.Resolved || result.Record.Karta != d.previous.Name || result.Record.KartaSource != string(inventory.SourceCluster) || result.Record.Component != d.component || result.Record.UID != result.Input.UID || result.Record.Name != result.Input.Name || result.Record.Namespace != result.Input.Namespace {
		return
	}
	event := d.events[result.Input]
	if event == nil || result.Input.GVK.Kind != "Pod" || result.Record.Workload != event.root {
		return
	}
	r.recordDefinitionCompletion(event, end)
}

func (r *Recorder) completeDefinitionRoot(result *store.WorkloadResult, end time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	d := r.definition
	if d == nil || !result.HasValue || result.Error != "" || result.StatusError != "" || result.Record.OwnerResolution != store.Resolved || result.Record.Karta != d.previous.Name || result.Record.KartaSource != string(inventory.SourceCluster) || !result.Record.HasStatus || len(result.Record.Phases) != 1 || result.Record.Phases[0] != d.phase || result.Record.UID != result.Record.Ref.UID || result.Record.Ref.GVK.Kind != "Deployment" {
		return
	}
	if event := d.events[result.Record.Ref]; event != nil {
		r.recordDefinitionCompletion(event, end)
	}
}

// The caller holds mu. Use captured commit times even if callbacks acquire mu out of order.
func (r *Recorder) recordDefinitionCompletion(event *definitionTiming, end time.Time) {
	if r.definition.start.IsZero() || end.Before(r.definition.start) {
		return
	}
	if event.end.IsZero() {
		event.end = end
		r.completed++
	} else if end.Before(event.end) {
		event.end = end
	}
}

func (r *Recorder) MeasureDefinition(includeSamples bool) DefinitionMeasurement {
	r.mu.Lock()
	result := DefinitionMeasurement{Overflow: r.overflow}
	d := r.definition
	if d == nil {
		r.mu.Unlock()
		return result
	}
	result.Definition, result.Observed = d.observed, !d.start.IsZero()
	result.Expected, result.Completed = len(d.events), r.completed
	result.Missing = result.Expected - result.Completed
	var roots, pods []int64
	for ref, event := range d.events {
		if event.end.IsZero() {
			continue
		}
		latency := event.end.Sub(d.start).Nanoseconds()
		if ref.GVK.Kind == "Deployment" {
			roots = append(roots, latency)
		} else {
			pods = append(pods, latency)
		}
		if includeSamples {
			result.Samples = append(result.Samples, Sample{Ref: ref, LatencyNS: latency})
		}
	}
	r.mu.Unlock()
	result.Roots, result.Pods = distribution(roots), distribution(pods)
	return result
}
