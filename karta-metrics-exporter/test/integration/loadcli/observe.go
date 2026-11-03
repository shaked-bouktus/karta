// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 NVIDIA Corporation

package main

import (
	"fmt"
	"math"
	"slices"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/types"

	inventory "github.com/dsx-ai-factory/workload-map/karta-metrics-exporter/pkg/kartadefinitioninventory"
	"github.com/dsx-ai-factory/workload-map/karta-metrics-exporter/pkg/store"
)

type key struct {
	UID types.UID
	RV  string
}
type target struct {
	Ref                                                     store.ObjectRef
	Root                                                    store.ObjectRef
	Definition, Component, Phase, RootRevision, PodRevision string
}
type sample struct {
	Kind               string
	UID                types.UID
	RV                 string
	Receipt, Published time.Time
	Nanoseconds        int64
}
type distribution struct {
	Expected, Completed, Missing int
	P50, P90, P99                *int64
	Samples                      []sample
}
type invalidExample struct {
	UID                                     types.UID
	RV, Kind, Operation, Error, StatusError string
	HasValue, SourcesValid                  bool
	SourceReason                            string
}
type observer struct {
	store.PublicationStore
	mu                            sync.Mutex
	last                          time.Time
	writes, invalid, overflow     int
	phaseInvalid, measuredInvalid int
	limit                         int
	active                        bool
	examples                      []invalidExample
	targets                       map[types.UID]target
	receipts, ends                map[key]time.Time
	definitions                   map[key]time.Time
	health                        map[store.SourceKey]store.SourceHealth
}

func newObserver(limit int) *observer {
	return &observer{PublicationStore: store.New(), last: time.Now(), limit: limit, health: map[store.SourceKey]store.SourceHealth{}}
}
func (o *observer) arm(targets map[types.UID]target) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.targets = targets
	o.receipts = map[key]time.Time{}
	o.ends = map[key]time.Time{}
	o.definitions = map[key]time.Time{}
	o.active = true
	o.phaseInvalid = 0
	o.examples = nil
}
func (o *observer) finish() (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.active = false
	if o.phaseInvalid > 0 {
		return o.phaseInvalid, fmt.Errorf("%d invalid publications during measured phase", o.phaseInvalid)
	}
	return 0, nil
}

func (o *observer) recordInvalid(example invalidExample) {
	o.invalid++
	if o.active {
		o.phaseInvalid++
		o.measuredInvalid++
		if len(o.examples) < 10 {
			o.examples = append(o.examples, example)
		}
	}
}
func (o *observer) invalidExamples() []invalidExample {
	o.mu.Lock()
	defer o.mu.Unlock()
	return slices.Clone(o.examples)
}

func (o *observer) save(m map[key]time.Time, k key, now time.Time) {
	if _, ok := m[k]; ok {
		return
	}
	if len(m) >= o.limit {
		o.overflow++
		return
	}
	m[k] = now
}
func (o *observer) event(ref store.ObjectRef, deleted bool) {
	now := time.Now()
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.active && !deleted {
		if _, ok := o.targets[ref.UID]; ok {
			o.save(o.receipts, key{ref.UID, ref.ResourceVersion}, now)
		}
	}
}
func (o *observer) definition(origin inventory.Origin) {
	now := time.Now()
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.active {
		o.save(o.definitions, key{origin.UID, origin.ResourceVersion}, now)
	}
}
func (o *observer) SetSourceHealth(k store.SourceKey, h store.SourceHealth) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.PublicationStore.SetSourceHealth(k, h)
	o.health[k] = h
	o.last = time.Now()
	o.writes++
}
func (o *observer) RemoveSourceHealth(k store.SourceKey) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.PublicationStore.RemoveSourceHealth(k)
	delete(o.health, k)
	o.last = time.Now()
	o.writes++
}
func (o *observer) SetDiscoveryFailure(ref store.ObjectRef, reason string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.PublicationStore.SetDiscoveryFailure(ref, reason)
	o.last = time.Now()
	o.writes++
	o.recordInvalid(invalidExample{
		UID: ref.UID, RV: ref.ResourceVersion, Kind: ref.GVK.Kind,
		Operation: "discovery", Error: reason,
		SourceReason: "not evaluated for discovery failure",
	})
}
func (o *observer) RemoveDiscoveryFailure(uid types.UID) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.PublicationStore.RemoveDiscoveryFailure(uid)
	o.last = time.Now()
	o.writes++
}
func (o *observer) UpsertWorkload(r *store.WorkloadResult) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.PublicationStore.UpsertWorkload(r)
	now := time.Now()
	o.last = now
	o.writes++
	valid, _ := (store.CapturedView{Sources: o.health}).Valid(r.Sources, r.Error)
	if !r.HasValue || !valid || r.StatusError != "" {
		sourcesValid, sourceReason := (store.CapturedView{Sources: o.health}).Valid(r.Sources, "")
		o.recordInvalid(invalidExample{
			UID: r.Record.UID, RV: r.Record.Ref.ResourceVersion, Kind: r.Record.Ref.GVK.Kind,
			Operation: "workload", Error: r.Error, StatusError: r.StatusError,
			HasValue: r.HasValue, SourcesValid: sourcesValid, SourceReason: sourceReason,
		})
		return
	}
	if t, ok := o.targets[r.Record.UID]; o.active && ok && matchRoot(r, t, false) {
		o.save(o.ends, key{r.Record.UID, r.Record.Ref.ResourceVersion}, now)
	}
}
func (o *observer) UpsertPod(r *store.PodResult) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.PublicationStore.UpsertPod(r)
	now := time.Now()
	o.last = now
	o.writes++
	valid, _ := (store.CapturedView{Sources: o.health}).Valid(r.Sources, r.Error)
	if !r.HasValue || !valid {
		sourcesValid, sourceReason := (store.CapturedView{Sources: o.health}).Valid(r.Sources, "")
		o.recordInvalid(invalidExample{
			UID: r.Input.UID, RV: r.Input.ResourceVersion, Kind: r.Input.GVK.Kind,
			Operation: "pod", Error: r.Error, HasValue: r.HasValue,
			SourcesValid: sourcesValid, SourceReason: sourceReason,
		})
		return
	}
	if t, ok := o.targets[r.Record.UID]; o.active && ok && matchPod(r, t, false) {
		o.save(o.ends, key{r.Input.UID, r.Input.ResourceVersion}, now)
	}
}
func (o *observer) RemovePod(uid types.UID) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.PublicationStore.RemovePod(uid)
	o.last = time.Now()
	o.writes++
}
func (o *observer) RemoveWorkload(uid types.UID) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.PublicationStore.RemoveWorkload(uid)
	o.last = time.Now()
	o.writes++
}
func matchRef(actual, want store.ObjectRef, exact bool) bool {
	return actual.UID == want.UID && actual.Name == want.Name && actual.Namespace == want.Namespace && actual.GVK == want.GVK && (!exact || actual.ResourceVersion == want.ResourceVersion)
}
func matchRoot(r *store.WorkloadResult, t target, exact bool) bool {
	return r != nil &&
		r.HasValue &&
		r.Error == "" &&
		r.StatusError == "" &&
		matchRef(r.Record.Ref, t.Ref, exact) &&
		r.Record.UID == t.Ref.UID &&
		r.Record.Karta == t.Definition &&
		r.Record.KartaSource == "cluster" &&
		r.Record.OwnerResolution == store.Resolved &&
		r.Record.HasStatus &&
		slices.Equal(r.Record.Phases, []string{t.Phase}) &&
		len(r.Record.StatusLabels) == 1 &&
		r.Record.StatusLabels["root_revision"] == t.RootRevision
}
func matchPod(r *store.PodResult, t target, exact bool) bool {
	return r != nil &&
		r.HasValue &&
		r.Error == "" &&
		matchRef(r.Input, t.Ref, exact) &&
		matchRef(r.Record.Workload, t.Root, true) &&
		r.Record.UID == t.Ref.UID &&
		r.Record.Name == t.Ref.Name &&
		r.Record.Namespace == t.Ref.Namespace &&
		r.Record.OwnerResolution == store.Resolved &&
		r.Record.Karta == t.Definition &&
		r.Record.KartaSource == "cluster" &&
		r.Record.Component == t.Component &&
		r.Record.Instance == "" &&
		r.Record.Group == "" &&
		len(r.Record.Labels) == 2 &&
		r.Record.Labels["root_revision"] == t.RootRevision &&
		r.Record.Labels["pod_revision"] == t.PodRevision
}
func (o *observer) results(targets map[types.UID]target, definition *key) (distribution, distribution, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	var roots, pods distribution
	for _, t := range targets {
		d := &roots
		if t.Ref.GVK.Kind == "Pod" {
			d = &pods
		}
		d.Expected++
		k := key{t.Ref.UID, t.Ref.ResourceVersion}
		start := o.receipts[k]
		if definition != nil {
			start = o.definitions[*definition]
		}
		end := o.ends[k]
		if start.IsZero() || end.IsZero() || end.Before(start) {
			continue
		}
		d.Samples = append(d.Samples, sample{Kind: t.Ref.GVK.Kind, UID: k.UID, RV: k.RV, Receipt: start, Published: end, Nanoseconds: end.Sub(start).Nanoseconds()})
	}
	summarize(&roots)
	summarize(&pods)
	if roots.Missing+pods.Missing > 0 {
		return roots, pods, fmt.Errorf("missing %d required observations", roots.Missing+pods.Missing)
	}
	return roots, pods, nil
}
func (o *observer) pending(targets map[types.UID]target, definition *key) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	missing := 0
	for _, t := range targets {
		k := key{t.Ref.UID, t.Ref.ResourceVersion}
		start := o.receipts[k]
		if definition != nil {
			start = o.definitions[*definition]
		}
		end := o.ends[k]
		if start.IsZero() || end.IsZero() || end.Before(start) {
			missing++
		}
	}
	if missing > 0 {
		return fmt.Errorf("missing %d required observations", missing)
	}
	return nil
}

func summarize(d *distribution) {
	slices.SortFunc(d.Samples, func(a, b sample) int {
		if a.Nanoseconds < b.Nanoseconds {
			return -1
		}
		if a.Nanoseconds > b.Nanoseconds {
			return 1
		}
		return 0
	})
	d.Completed = len(d.Samples)
	d.Missing = d.Expected - d.Completed
	if d.Completed == 0 {
		return
	}
	values := []**int64{&d.P50, &d.P90, &d.P99}
	for i, p := range []float64{.5, .9, .99} {
		v := d.Samples[int(math.Ceil(p*float64(d.Completed)))-1].Nanoseconds
		*values[i] = &v
	}
}
