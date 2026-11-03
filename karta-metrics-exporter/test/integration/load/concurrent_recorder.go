// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 NVIDIA Corporation

package main

import (
	"errors"
	"time"

	inventory "github.com/dsx-ai-factory/workload-map/karta-metrics-exporter/pkg/kartadefinitioninventory"
	"github.com/dsx-ai-factory/workload-map/karta-metrics-exporter/pkg/store"
)

type ConcurrentCohort struct {
	Previous inventory.Origin      `json:"previous"`
	Updated  inventory.Origin      `json:"updated"`
	Expected DefinitionExpectation `json:"expected"`
	Fixtures []Fixture             `json:"fixtures"`
}

type ConcurrentMeasurement struct {
	DefinitionMeasurement
	Receipt time.Time `json:"receipt"`
}

type concurrentRecorder struct {
	runs                  map[string]*concurrentRun
	used, limit, overflow int
}

type concurrentRun struct {
	cohort   ConcurrentCohort
	receipts map[string]time.Time
	events   map[store.ObjectRef]*concurrentTiming
}

type concurrentTiming struct {
	root    store.ObjectRef
	commits []time.Time
}

func (r *Recorder) armConcurrent(cohorts []ConcurrentCohort) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	next := &concurrentRecorder{runs: make(map[string]*concurrentRun), limit: r.maxSamples}
	seen := make(map[string]bool)
	for _, c := range cohorts {
		if c.Previous.Source != inventory.SourceCluster || c.Previous.UID == "" || c.Previous.ResourceVersion == "" || c.Previous.Name == "" || c.Expected.Name != c.Previous.Name || c.Expected.Component == "" || c.Expected.Phase == "" || len(c.Fixtures) == 0 {
			return errors.New("invalid concurrent cohort")
		}
		if next.runs[c.Previous.Name] != nil {
			return errors.New("duplicate concurrent Karta")
		}
		c.Updated = inventory.Origin{}
		run := &concurrentRun{cohort: c, receipts: make(map[string]time.Time), events: make(map[store.ObjectRef]*concurrentTiming)}
		for _, f := range c.Fixtures {
			refs := fixtureExpectations(f, false)
			for _, e := range refs {
				if e.Ref.UID == "" || e.Ref.ResourceVersion == "" || seen[string(e.Ref.UID)] {
					return errors.New("concurrent fixtures require unique versioned identities")
				}
				seen[string(e.Ref.UID)] = true
				run.events[e.Ref] = &concurrentTiming{root: refs[0].Ref}
				next.used++
			}
		}
		next.runs[c.Previous.Name] = run
	}
	if len(cohorts) < 2 || next.used > next.limit {
		return errors.New("concurrent recorder requires multiple cohorts within sample budget")
	}
	r.beginLocked()
	r.concurrent = next
	return nil
}

func (c *concurrentRecorder) reserve() bool {
	if c.used >= c.limit {
		c.overflow++
		return false
	}
	c.used++
	return true
}

func (c *concurrentRecorder) observe(origin inventory.Origin, at time.Time) {
	run := c.runs[origin.Name]
	if run == nil || origin.Source != run.cohort.Previous.Source || origin.UID != run.cohort.Previous.UID || origin.ResourceVersion == "" || origin.ResourceVersion == run.cohort.Previous.ResourceVersion {
		return
	}
	if old, ok := run.receipts[origin.ResourceVersion]; ok {
		if at.Before(old) {
			run.receipts[origin.ResourceVersion] = at
		}
		return
	}
	if c.reserve() {
		run.receipts[origin.ResourceVersion] = at
	}
}

func (r *Recorder) bindConcurrent(cohorts []ConcurrentCohort) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.concurrent == nil || len(cohorts) != len(r.concurrent.runs) {
		return errors.New("concurrent recorder not armed or cohort count changed")
	}
	seen := make(map[string]bool)
	for _, c := range cohorts {
		run := r.concurrent.runs[c.Updated.Name]
		if run == nil || seen[c.Updated.Name] || c.Updated.Source != run.cohort.Previous.Source || c.Updated.UID != run.cohort.Previous.UID || c.Updated.ResourceVersion == "" || c.Updated.ResourceVersion == run.cohort.Previous.ResourceVersion {
			return errors.New("updated Karta origin mismatch")
		}
		seen[c.Updated.Name] = true
	}
	for _, c := range cohorts {
		r.concurrent.runs[c.Updated.Name].cohort.Updated = c.Updated
	}
	return nil
}

func (r *Recorder) completeConcurrentPod(result *store.PodResult, at time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.concurrent == nil {
		return
	}
	run := r.concurrent.runs[result.Record.Karta]
	if run == nil || !result.HasValue || result.Error != "" || result.Record.KartaSource != "cluster" || result.Record.OwnerResolution != store.Resolved || result.Record.Component != run.cohort.Expected.Component || result.Record.UID != result.Input.UID || result.Record.Name != result.Input.Name || result.Record.Namespace != result.Input.Namespace {
		return
	}
	event := run.events[result.Input]
	if event == nil || result.Input.GVK.Kind != "Pod" || result.Record.Workload != event.root || result.Record.Labels[RootMetricLabel] != "0" || result.Record.Labels[PodMetricLabel] != "0" {
		return
	}
	if r.concurrent.reserve() {
		event.commits = append(event.commits, at)
	}
}

func (r *Recorder) completeConcurrentRoot(result *store.WorkloadResult, at time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.concurrent == nil {
		return
	}
	run := r.concurrent.runs[result.Record.Karta]
	if run == nil || !result.HasValue || result.Error != "" || result.StatusError != "" || result.Record.KartaSource != "cluster" || result.Record.OwnerResolution != store.Resolved || !result.Record.HasStatus || len(result.Record.Phases) != 1 || result.Record.Phases[0] != run.cohort.Expected.Phase || result.Record.UID != result.Record.Ref.UID || result.Record.StatusLabels[RootMetricLabel] != "0" {
		return
	}
	event := run.events[result.Record.Ref]
	if event == nil || event.root != result.Record.Ref {
		return
	}
	if r.concurrent.reserve() {
		event.commits = append(event.commits, at)
	}
}

func (r *Recorder) measureConcurrent(samples bool) []ConcurrentMeasurement {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.concurrent == nil {
		return nil
	}
	var results []ConcurrentMeasurement
	for _, name := range concurrentNames() {
		run := r.concurrent.runs[name]
		if run == nil {
			continue
		}
		receipt, observed := run.receipts[run.cohort.Updated.ResourceVersion]
		m := ConcurrentMeasurement{DefinitionMeasurement: DefinitionMeasurement{Definition: run.cohort.Updated, Observed: observed, Expected: len(run.events), Overflow: r.concurrent.overflow}, Receipt: receipt}
		if m.Definition.Name == "" {
			m.Definition = run.cohort.Previous
		}
		var roots, pods []int64
		if observed {
			for ref, event := range run.events {
				var first time.Time
				for _, at := range event.commits {
					if !at.Before(receipt) && (first.IsZero() || at.Before(first)) {
						first = at
					}
				}
				if first.IsZero() {
					continue
				}
				m.Completed++
				if samples {
					elapsed := first.Sub(receipt).Nanoseconds()
					if ref.GVK.Kind == "Pod" {
						pods = append(pods, elapsed)
					} else {
						roots = append(roots, elapsed)
					}
					m.Samples = append(m.Samples, Sample{Ref: ref, LatencyNS: elapsed})
				}
			}
		}
		m.Missing = m.Expected - m.Completed
		if samples {
			m.Roots = distribution(roots)
			m.Pods = distribution(pods)
		}
		results = append(results, m)
	}
	return results
}
