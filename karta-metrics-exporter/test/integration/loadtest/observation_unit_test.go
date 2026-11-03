// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 NVIDIA Corporation

package loadtest

import (
	"sync"
	"testing"
	"time"

	inventory "github.com/dsx-ai-factory/workload-map/karta-metrics-exporter/pkg/kartadefinitioninventory"
	"github.com/dsx-ai-factory/workload-map/karta-metrics-exporter/pkg/store"
)

func TestNearestRankAndEmptyDistribution(t *testing.T) {
	if d := summarize(3, nil); d.P50 != nil || d.P90 != nil || d.P99 != nil || d.Missing != 3 {
		t.Fatalf("empty: %+v", d)
	}
	d := summarize(5, []sample{{Nanoseconds: 100}, {Nanoseconds: 1}, {Nanoseconds: 10}, {Nanoseconds: 4}})
	if *d.P50 != 4 || *d.P90 != 100 || *d.P99 != 100 || d.Completed != 4 || d.Missing != 1 {
		t.Fatalf("nearest rank: %+v", d)
	}
}
func TestReceiptAndPublicationBeforeAPIResponseRemainMatchable(t *testing.T) {
	o := newObservation(100)
	o.begin()
	ref := store.ObjectRef{UID: "root", ResourceVersion: "opaque-z"}
	o.event(ref, false)
	o.UpsertWorkload(&store.WorkloadResult{HasValue: true, Record: store.WorkloadRecord{UID: ref.UID, Ref: ref}})
	// The API response becomes available only after both hooks have executed.
	returned := identity{ref.UID, ref.ResourceVersion}
	s, err := o.match("root", returned, nil, func(p publication) bool { return p.Root != nil && p.Root.Record.Ref.ResourceVersion == returned.RV })
	if err != nil || s.Nanoseconds < 0 || o.Workload(ref.UID) == nil {
		t.Fatalf("sample=%+v error=%v", s, err)
	}
}
func TestStaleInvalidAndWrongUIDCannotSatisfySample(t *testing.T) {
	o := newObservation(100)
	o.begin()
	ref := store.ObjectRef{UID: "root", ResourceVersion: "a"}
	o.event(ref, false)
	valid := func(p publication) bool {
		return p.Root != nil && p.Root.HasValue && p.Root.Error == "" && p.Root.Record.Ref.ResourceVersion == "a"
	}
	o.UpsertWorkload(&store.WorkloadResult{HasValue: true, Record: store.WorkloadRecord{UID: "other", Ref: ref}})
	stale := ref
	stale.ResourceVersion = "z"
	o.UpsertWorkload(&store.WorkloadResult{HasValue: true, Record: store.WorkloadRecord{UID: ref.UID, Ref: stale}})
	o.UpsertWorkload(&store.WorkloadResult{HasValue: true, Error: "evaluation_failed", Record: store.WorkloadRecord{UID: ref.UID, Ref: ref}})
	if _, err := o.match("root", identity{ref.UID, ref.ResourceVersion}, nil, valid); err == nil {
		t.Fatal("invalid publication matched")
	}
	if o.invalid != 1 {
		t.Fatalf("invalid count %d", o.invalid)
	}
}
func TestDefinitionFanoutSharesExactReceipt(t *testing.T) {
	o := newObservation(100)
	o.begin()
	key := identity{"definition", "opaque-rv"}
	o.definition(inventory.Origin{UID: key.UID, ResourceVersion: key.RV})
	o.UpsertWorkload(&store.WorkloadResult{HasValue: true, Record: store.WorkloadRecord{UID: "root"}})
	o.UpsertPod(&store.PodResult{HasValue: true, Record: store.PodRecord{UID: "pod"}})
	a, err := o.match("root", identity{"root", "unchanged-root"}, &key, func(p publication) bool { return p.Root != nil })
	if err != nil {
		t.Fatal(err)
	}
	b, err := o.match("pod", identity{"pod", "unchanged-pod"}, &key, func(p publication) bool { return p.Pod != nil })
	if err != nil {
		t.Fatal(err)
	}
	if !a.Receipt.Equal(b.Receipt) {
		t.Fatal("fanout receipt differs")
	}
	wrong := identity{key.UID, "different"}
	if _, err := o.match("pod", identity{"pod", "unchanged-pod"}, &wrong, func(publication) bool { return true }); err == nil {
		t.Fatal("wrong definition RV matched")
	}
}
func TestObserverBoundAndConcurrentWrites(t *testing.T) {
	o := newObservation(2)
	o.begin()
	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() {
			o.event(store.ObjectRef{UID: "pod", ResourceVersion: "rv"}, false)
			o.UpsertPod(&store.PodResult{HasValue: true, Record: store.PodRecord{UID: "pod"}})
		})
	}
	wg.Wait()
	if o.overflow != 8 || len(o.publications["pod"]) != 2 || o.writes != 10 {
		t.Fatalf("overflow=%d writes=%d", o.overflow, o.writes)
	}
	before := o.last
	time.Sleep(time.Millisecond)
	o.RemovePod("pod")
	if !o.last.After(before) || o.Pod("pod") != nil {
		t.Fatal("removal not delegated or counted")
	}
}

func TestUnhealthyPublicationCannotCompleteEvenAfterRecovery(t *testing.T) {
	o := newObservation(100)
	o.begin()
	key := store.SourceKey{}
	ref := store.ObjectRef{UID: "root", ResourceVersion: "rv"}
	o.event(ref, false)
	o.SetSourceHealth(key, store.SourceHealth{Synced: false})
	p := &store.WorkloadResult{HasValue: true, Record: store.WorkloadRecord{UID: ref.UID, Ref: ref}, Sources: []store.SourceKey{key}}
	o.UpsertWorkload(p)
	o.SetSourceHealth(key, store.SourceHealth{Synced: true})
	if _, err := o.match("root", identity{ref.UID, ref.ResourceVersion}, nil, func(publication) bool { return true }); err == nil {
		t.Fatal("unhealthy write completed a sample")
	}
	o.UpsertWorkload(p)
	if _, err := o.match("root", identity{ref.UID, ref.ResourceVersion}, nil, func(publication) bool { return true }); err != nil {
		t.Fatal(err)
	}
}
