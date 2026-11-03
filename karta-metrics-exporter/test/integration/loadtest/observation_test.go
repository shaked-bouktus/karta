// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 NVIDIA Corporation

package loadtest

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

type identity struct {
	UID types.UID
	RV  string
}
type publication struct {
	Valid bool
	At    time.Time
	Pod   *store.PodResult
	Root  *store.WorkloadResult
}
type invalidExample struct {
	UID                            types.UID
	RV, Kind, Reason, StatusReason string
	HasValue, SourcesValid         bool
}
type observation struct {
	store.PublicationStore
	mu                               sync.Mutex
	active                           bool
	invalidExamples                  []invalidExample
	health                           map[store.SourceKey]store.SourceHealth
	limit, overflow, invalid, writes int
	last                             time.Time
	receipts                         map[identity]time.Time
	definitions                      map[identity]time.Time
	publications                     map[types.UID][]publication
}
type sample struct {
	Kind                 string
	UID                  types.UID
	RV                   string
	Receipt, Publication time.Time
	Nanoseconds          int64
}
type distribution struct {
	Expected, Completed, Missing int
	P50, P90, P99                *int64
	Samples                      []sample
}

func newObservation(limit int) *observation {
	return &observation{PublicationStore: store.New(), limit: limit, last: time.Now(), health: make(map[store.SourceKey]store.SourceHealth)}
}
func (o *observation) begin() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.active = true
	o.overflow = 0
	o.invalid = 0
	o.invalidExamples = nil
	o.writes = 0
	o.receipts = make(map[identity]time.Time)
	o.definitions = make(map[identity]time.Time)
	o.publications = make(map[types.UID][]publication)
}
func (o *observation) event(ref store.ObjectRef, deleted bool) {
	now := time.Now()
	if deleted {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.active {
		return
	}
	key := identity{ref.UID, ref.ResourceVersion}
	if _, ok := o.receipts[key]; ok {
		return
	}
	if len(o.receipts) >= o.limit {
		o.overflow++
		return
	}
	o.receipts[key] = now
}
func (o *observation) definition(origin inventory.Origin) {
	now := time.Now()
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.active {
		return
	}
	key := identity{origin.UID, origin.ResourceVersion}
	if _, ok := o.definitions[key]; ok {
		return
	}
	if len(o.definitions) >= o.limit {
		o.overflow++
		return
	}
	o.definitions[key] = now
}
func (o *observation) UpsertPod(p *store.PodResult) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.PublicationStore.UpsertPod(p)
	o.save(p.Record.UID, publication{At: time.Now(), Pod: o.Pod(p.Record.UID)}, !p.HasValue || p.Error != "" || !o.sourcesValid(p.Sources))
}
func (o *observation) UpsertWorkload(w *store.WorkloadResult) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.PublicationStore.UpsertWorkload(w)
	o.save(w.Record.UID, publication{At: time.Now(), Root: o.Workload(w.Record.UID)}, !w.HasValue || w.Error != "" || w.StatusError != "" || !o.sourcesValid(w.Sources))
}
func (o *observation) save(uid types.UID, p publication, invalid bool) {
	p.Valid = !invalid
	o.last = p.At
	if !o.active {
		return
	}
	o.writes++
	if invalid {
		o.invalid++
		if len(o.invalidExamples) < 10 {
			var example invalidExample
			if p.Pod != nil {
				v := p.Pod
				example = invalidExample{UID: uid, RV: v.Input.ResourceVersion, Kind: "Pod", Reason: v.Error, HasValue: v.HasValue, SourcesValid: o.sourcesValid(v.Sources)}
			} else {
				v := p.Root
				example = invalidExample{UID: uid, RV: v.Record.Ref.ResourceVersion, Kind: v.Record.Ref.GVK.Kind, Reason: v.Error, StatusReason: v.StatusError, HasValue: v.HasValue, SourcesValid: o.sourcesValid(v.Sources)}
			}
			o.invalidExamples = append(o.invalidExamples, example)
		}
	}
	if o.writes > o.limit {
		o.overflow++
		return
	}
	o.publications[uid] = append(o.publications[uid], p)
}
func (o *observation) RemovePod(uid types.UID) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.PublicationStore.RemovePod(uid)
	o.touch()
}
func (o *observation) RemoveWorkload(uid types.UID) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.PublicationStore.RemoveWorkload(uid)
	o.touch()
}
func (o *observation) match(kind string, key identity, definition *identity, valid func(publication) bool) (sample, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	start := o.receipts[key]
	if definition != nil {
		start = o.definitions[*definition]
	}
	if start.IsZero() {
		return sample{}, fmt.Errorf("missing %s receipt %s/%s", kind, key.UID, key.RV)
	}
	for _, p := range o.publications[key.UID] {
		if p.Valid && !p.At.Before(start) && valid(p) {
			return sample{Kind: kind, UID: key.UID, RV: key.RV, Receipt: start, Publication: p.At, Nanoseconds: p.At.Sub(start).Nanoseconds()}, nil
		}
	}
	return sample{}, fmt.Errorf("missing valid %s publication %s/%s", kind, key.UID, key.RV)
}
func summarize(expected int, samples []sample) distribution {
	d := distribution{Expected: expected, Completed: len(samples), Missing: expected - len(samples), Samples: samples}
	if len(samples) == 0 {
		return d
	}
	values := make([]int64, len(samples))
	for i, s := range samples {
		values[i] = s.Nanoseconds
	}
	slices.Sort(values)
	percentile := func(p float64) *int64 {
		rank := int(math.Ceil(p * float64(len(values))))
		value := values[rank-1]
		return &value
	}
	d.P50 = percentile(.5)
	d.P90 = percentile(.9)
	d.P99 = percentile(.99)
	return d
}

func (o *observation) sourcesValid(sources []store.SourceKey) bool {
	for _, key := range sources {
		h, ok := o.health[key]
		if !ok || !h.Synced || (h.Error != "" && h.Error != store.ReasonNone) {
			return false
		}
	}
	return true
}
func (o *observation) SetSourceHealth(key store.SourceKey, h store.SourceHealth) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.PublicationStore.SetSourceHealth(key, h)
	o.health[key] = h
	o.touch()
}
func (o *observation) RemoveSourceHealth(key store.SourceKey) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.PublicationStore.RemoveSourceHealth(key)
	delete(o.health, key)
	o.touch()
}
func (o *observation) SetDiscoveryFailure(ref store.ObjectRef, reason string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.PublicationStore.SetDiscoveryFailure(ref, reason)
	o.touch()
	if o.active {
		o.invalid++
		if len(o.invalidExamples) < 10 {
			o.invalidExamples = append(o.invalidExamples, invalidExample{UID: ref.UID, RV: ref.ResourceVersion, Kind: ref.GVK.Kind, Reason: reason})
		}
	}
}
func (o *observation) RemoveDiscoveryFailure(uid types.UID) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.PublicationStore.RemoveDiscoveryFailure(uid)
	o.touch()
}
func (o *observation) touch() {
	o.last = time.Now()
	if o.active {
		o.writes++
	}
}
