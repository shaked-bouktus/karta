// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 NVIDIA Corporation

package main

import (
	"math"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"k8s.io/apimachinery/pkg/types"

	"github.com/dsx-ai-factory/workload-map/karta-metrics-exporter/pkg/store"
)

type Recorder struct {
	concurrent  *concurrentRecorder
	healthMu    sync.Mutex
	healthTrace HealthTrace
	store.PublicationStore
	mu                sync.Mutex
	maxSamples        int
	events            map[eventKey]*eventTiming
	deletions         map[types.UID][]eventKey
	completed         int
	overflow          int
	publicationWrites atomic.Uint64
	definition        *definitionRun
}

type eventKey struct {
	uid     types.UID
	version string
	deleted bool
}
type eventTiming struct {
	ref   store.ObjectRef
	start time.Time
	end   time.Time
}
type Expectation struct {
	Ref     store.ObjectRef `json:"ref"`
	Deleted bool            `json:"deleted"`
}
type Sample struct {
	Ref       store.ObjectRef `json:"ref"`
	Deleted   bool            `json:"deleted"`
	LatencyNS int64           `json:"latency_ns"`
}
type Distribution struct {
	Count int     `json:"count"`
	P50MS float64 `json:"p50_ms"`
	P90MS float64 `json:"p90_ms"`
	P99MS float64 `json:"p99_ms"`
	MaxMS float64 `json:"max_ms"`
}
type Measurement struct {
	Expected  int           `json:"expected"`
	Observed  int           `json:"observed"`
	Completed int           `json:"completed"`
	Missing   int           `json:"missing"`
	Overflow  int           `json:"overflow"`
	Pending   int           `json:"pending"`
	Unmatched int           `json:"unmatched"`
	Latency   *Distribution `json:"latency"`
	Samples   []Sample      `json:"samples"`
}
type RecorderStats struct {
	PublicationWrites uint64 `json:"publication_writes"`
	Observed          int    `json:"observed"`
	Completed         int    `json:"completed"`
	Pending           int    `json:"pending"`
	Overflow          int    `json:"overflow"`
}

func NewRecorder(publications store.PublicationStore, maxSamples int) *Recorder {
	r := &Recorder{PublicationStore: publications, maxSamples: max(0, maxSamples)}
	r.Begin()
	return r
}

// Begin requires the previous phase to be drained before new events are admitted.
func (r *Recorder) Begin() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.beginLocked()
}

func (r *Recorder) beginLocked() {
	r.definition = nil
	r.concurrent = nil
	r.events = make(map[eventKey]*eventTiming)
	r.deletions = make(map[types.UID][]eventKey)
	r.completed, r.overflow = 0, 0
	r.publicationWrites.Store(0)
}

func (r *Recorder) ObserveEvent(ref store.ObjectRef, deleted bool) {
	if ref.GVK.Kind != "Pod" && ref.GVK.Kind != "Deployment" {
		return
	}
	start := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.definition != nil || r.concurrent != nil {
		return
	}
	key := eventKey{ref.UID, ref.ResourceVersion, deleted}
	if old, ok := r.events[key]; ok {
		if start.Before(old.start) {
			old.start = start
		}
		return
	}
	if len(r.events) >= r.maxSamples {
		r.overflow++
		return
	}
	r.events[key] = &eventTiming{ref: ref, start: start}
	if deleted {
		r.deletions[ref.UID] = append(r.deletions[ref.UID], key)
	}
}

func (r *Recorder) UpsertPod(result *store.PodResult) {
	r.PublicationStore.UpsertPod(result)
	end := time.Now()
	r.publicationWrites.Add(1)
	r.completeConcurrentPod(result, end)
	r.completeDefinitionPod(result, end)
	if result.Input.GVK.Kind == "Pod" && result.HasValue && result.Error == "" && result.Record.OwnerResolution == store.Resolved && result.Record.Workload.UID != "" && result.Input.UID == result.Record.UID {
		r.complete(eventKey{uid: result.Input.UID, version: result.Input.ResourceVersion}, end)
	}
}

func (r *Recorder) UpsertWorkload(result *store.WorkloadResult) {
	r.PublicationStore.UpsertWorkload(result)
	end := time.Now()
	r.publicationWrites.Add(1)
	r.completeConcurrentRoot(result, end)
	r.completeDefinitionRoot(result, end)
	if result.Record.Ref.GVK.Kind == "Deployment" && result.HasValue && result.Error == "" && result.StatusError == "" && result.Record.Ref.UID == result.Record.UID {
		r.complete(eventKey{uid: result.Record.Ref.UID, version: result.Record.Ref.ResourceVersion}, end)
	}
}

func (r *Recorder) complete(key eventKey, end time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if event, ok := r.events[key]; ok && event.end.IsZero() && !end.Before(event.start) {
		event.end = end
		r.completed++
	}
}

func (r *Recorder) RemovePod(uid types.UID) {
	r.PublicationStore.RemovePod(uid)
	end := time.Now()
	r.publicationWrites.Add(1)
	r.completeDeletion(uid, "Pod", end)
}

func (r *Recorder) RemoveWorkload(uid types.UID) {
	r.PublicationStore.RemoveWorkload(uid)
	end := time.Now()
	r.publicationWrites.Add(1)
	r.completeDeletion(uid, "Deployment", end)
}

func (r *Recorder) completeDeletion(uid types.UID, kind string, end time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	// Removal has only a UID. Match observed deletion versions without ordering opaque RVs.
	for _, key := range r.deletions[uid] {
		event := r.events[key]
		if event.ref.GVK.Kind == kind && event.end.IsZero() && !end.Before(event.start) {
			event.end = end
			r.completed++
		}
	}
}

func (r *Recorder) Stats() RecorderStats {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.definition != nil {
		observed := 0
		if !r.definition.start.IsZero() {
			observed = len(r.definition.events)
		}
		return RecorderStats{PublicationWrites: r.publicationWrites.Load(), Observed: observed, Completed: r.completed, Pending: observed - r.completed, Overflow: r.overflow}
	}
	return RecorderStats{PublicationWrites: r.publicationWrites.Load(), Observed: len(r.events), Completed: r.completed, Pending: len(r.events) - r.completed, Overflow: r.overflow}
}

func (r *Recorder) Measure(expected []Expectation) Measurement {
	result := r.measure(expected, true)
	values := make([]int64, len(result.Samples))
	for i, sample := range result.Samples {
		values[i] = sample.LatencyNS
	}
	result.Latency = distribution(values)
	return result
}

// MeasureProgress omits sample allocation and percentiles for convergence polling.
// Pending includes superseded callbacks and is not a convergence gate.
func (r *Recorder) MeasureProgress(expected []Expectation) Measurement {
	return r.measure(expected, false)
}

func (r *Recorder) measure(expected []Expectation, includeSamples bool) Measurement {
	r.mu.Lock()
	defer r.mu.Unlock()
	result := Measurement{Observed: len(r.events), Overflow: r.overflow, Pending: len(r.events) - r.completed}
	// The expectation slice belongs to the caller; this map is transient and is not retained.
	seen := make(map[eventKey]struct{}, len(expected))
	sampled := make(map[eventKey]struct{})
	for _, item := range expected {
		key := eventKey{item.Ref.UID, item.Ref.ResourceVersion, item.Deleted}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		result.Expected++
		event := r.events[key]
		if item.Deleted && item.Ref.ResourceVersion == "" {
			// Delete responses do not expose the watch tombstone's resource version.
			for _, deletionKey := range r.deletions[item.Ref.UID] {
				candidate := r.events[deletionKey]
				if !candidate.end.IsZero() && (event == nil || candidate.start.Before(event.start)) {
					event = candidate
				}
			}
		}
		if event != nil && !event.end.IsZero() {
			actual := eventKey{event.ref.UID, event.ref.ResourceVersion, item.Deleted}
			if _, exists := sampled[actual]; exists {
				continue
			}
			sampled[actual] = struct{}{}
			result.Completed++
			if includeSamples {
				result.Samples = append(result.Samples, Sample{Ref: event.ref, Deleted: key.deleted, LatencyNS: event.end.Sub(event.start).Nanoseconds()})
			}
		}
	}
	result.Missing = result.Expected - result.Completed
	result.Unmatched = r.completed - result.Completed
	return result
}

func distribution(values []int64) *Distribution {
	if len(values) == 0 {
		return nil
	}
	slices.Sort(values)
	percentile := func(p float64) float64 {
		return float64(values[int(math.Ceil(p*float64(len(values))))-1]) / float64(time.Millisecond)
	}
	return &Distribution{Count: len(values), P50MS: percentile(.5), P90MS: percentile(.9), P99MS: percentile(.99), MaxMS: float64(values[len(values)-1]) / float64(time.Millisecond)}
}
