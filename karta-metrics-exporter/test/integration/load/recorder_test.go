// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 NVIDIA Corporation

package main

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	"github.com/dsx-ai-factory/workload-map/karta-metrics-exporter/pkg/store"
)

type blockingPublication struct {
	store.PublicationStore
	entered chan struct{}
	release chan struct{}
}

func testRef(kind, uid, version string) store.ObjectRef {
	return store.ObjectRef{GVK: schema.GroupVersionKind{Kind: kind}, UID: types.UID(uid), ResourceVersion: version, Name: uid}
}

func podResult(ref store.ObjectRef) *store.PodResult {
	return &store.PodResult{Input: ref, HasValue: true, Record: store.PodRecord{UID: ref.UID, Workload: testRef("Deployment", "root", "1"), OwnerResolution: store.Resolved}}
}

func workloadResult(ref store.ObjectRef) *store.WorkloadResult {
	return &store.WorkloadResult{HasValue: true, Record: store.WorkloadRecord{UID: ref.UID, Ref: ref}}
}

func TestDistributionNearestRank(t *testing.T) {
	if distribution(nil) != nil {
		t.Fatal("empty distribution must be nil")
	}
	values := make([]int64, 101)
	for i := range values {
		values[i] = int64(101-i) * int64(time.Millisecond)
	}
	got := distribution(values)
	if got.Count != 101 || got.P50MS != 51 || got.P90MS != 91 || got.P99MS != 100 || got.MaxMS != 101 {
		t.Fatalf("wrong nearest ranks: %+v", got)
	}
}

func TestRecorderMatchesExactEventsAndPreservesEarliest(t *testing.T) {
	publications := store.New()
	r := NewRecorder(publications, 10)
	ref := testRef("Pod", "pod", "opaque-a")
	r.ObserveEvent(ref, false)
	key := eventKey{uid: ref.UID, version: ref.ResourceVersion}
	first := r.events[key].start
	r.ObserveEvent(ref, false)
	if !r.events[key].start.Equal(first) {
		t.Fatal("duplicate reset start")
	}
	r.UpsertPod(podResult(ref))
	end := r.events[key].end
	r.UpsertPod(podResult(ref))
	if !r.events[key].end.Equal(end) {
		t.Fatal("duplicate reset completion")
	}
	if publications.Pod(ref.UID) == nil {
		t.Fatal("publication was not delegated")
	}
	missing := testRef("Pod", "missing", "x")
	got := r.Measure([]Expectation{{Ref: ref}, {Ref: ref}, {Ref: missing}})
	if got.Expected != 2 || got.Completed != 1 || got.Observed != 1 || got.Missing != 1 || got.Latency == nil || len(got.Samples) != 1 || got.Samples[0].LatencyNS < 0 {
		t.Fatalf("unexpected measurement: %+v", got)
	}
	got.Samples[0].Ref.UID = "mutated"
	if r.Measure([]Expectation{{Ref: ref}}).Samples[0].Ref.UID != ref.UID {
		t.Fatal("snapshot aliases recorder")
	}
	r.Begin()
	if r.Stats() != (RecorderStats{}) || r.Measure(nil).Latency != nil {
		t.Fatal("phase not cleared")
	}
}

func TestRecorderInvalidAndStalePublications(t *testing.T) {
	for _, mode := range []string{"no-value", "error", "unresolved", "no-root", "stale-version", "wrong-uid"} {
		t.Run("pod/"+mode, func(t *testing.T) {
			r := NewRecorder(store.New(), 4)
			ref := testRef("Pod", "p", "new")
			r.ObserveEvent(ref, false)
			result := podResult(ref)
			switch mode {
			case "no-value":
				result.HasValue = false
			case "error":
				result.Error = store.ReasonEvaluationFailed
			case "unresolved":
				result.Record.OwnerResolution = store.UnknownOwner
			case "no-root":
				result.Record.Workload.UID = ""
			case "stale-version":
				result.Input.ResourceVersion = "old"
			case "wrong-uid":
				result.Record.UID = "other"
			}
			r.UpsertPod(result)
			if r.Stats().Completed != 0 {
				t.Fatal("invalid result completed event")
			}
			if r.Pod(result.Record.UID) == nil {
				t.Fatal("invalid result was not forwarded")
			}
		})
	}
	for _, mode := range []string{"no-value", "error", "status-error", "stale-version", "wrong-uid"} {
		t.Run("workload/"+mode, func(t *testing.T) {
			r := NewRecorder(store.New(), 4)
			ref := testRef("Deployment", "d", "new")
			r.ObserveEvent(ref, false)
			result := workloadResult(ref)
			switch mode {
			case "no-value":
				result.HasValue = false
			case "error":
				result.Error = store.ReasonEvaluationFailed
			case "status-error":
				result.StatusError = store.ReasonEvaluationFailed
			case "stale-version":
				result.Record.Ref.ResourceVersion = "old"
			case "wrong-uid":
				result.Record.UID = "other"
			}
			r.UpsertWorkload(result)
			if r.Stats().Completed != 0 {
				t.Fatal("invalid result completed event")
			}
			r.UpsertWorkload(workloadResult(ref))
			if r.Stats().Completed != 1 {
				t.Fatal("valid workload not completed")
			}
		})
	}
}

func TestRecorderDeletionUIDAndKindCorrelation(t *testing.T) {
	r := NewRecorder(store.New(), 10)
	pod := testRef("Pod", "p", "opaque-delete")
	deployment := testRef("Deployment", "d", "opaque")
	r.ObserveEvent(pod, false)
	r.ObserveEvent(pod, true)
	r.ObserveEvent(deployment, true)
	r.RemovePod("different-uid")
	r.RemoveWorkload(pod.UID)
	if r.Stats().Completed != 0 {
		t.Fatal("unrelated removal completed deletion")
	}
	r.RemovePod(pod.UID)
	r.RemovePod(pod.UID)
	r.RemoveWorkload(deployment.UID)
	got := r.Measure([]Expectation{{Ref: pod, Deleted: true}, {Ref: deployment, Deleted: true}})
	if got.Completed != 2 || got.Pending != 1 || got.Missing != 0 {
		t.Fatalf("unexpected deletion measurement: %+v", got)
	}
}

func TestRecorderBoundedOverflowAndUnmatched(t *testing.T) {
	r := NewRecorder(store.New(), 2)
	for i := range 5 {
		ref := testRef("Pod", fmt.Sprint(i), "v")
		r.ObserveEvent(ref, true)
		r.RemovePod(ref.UID)
	}
	r.ObserveEvent(testRef("ReplicaSet", "rs", "v"), false)
	if len(r.events) != 2 || len(r.deletions) != 2 {
		t.Fatal("retained maps exceed cap")
	}
	got := r.Measure([]Expectation{{Ref: testRef("Pod", "0", "v"), Deleted: true}, {Ref: testRef("Pod", "4", "v"), Deleted: true}})
	if got.Overflow != 3 || got.Completed != 1 || got.Missing != 1 || got.Unmatched != 1 || got.Observed != 2 {
		t.Fatalf("unexpected bounded measurement: %+v", got)
	}
	zero := NewRecorder(store.New(), -1)
	zero.ObserveEvent(testRef("Pod", "p", "v"), false)
	if zero.Stats().Overflow != 1 || len(zero.events) != 0 {
		t.Fatal("negative cap must retain nothing")
	}
}

func TestRecorderConcurrentPublication(t *testing.T) {
	const count = 200
	r := NewRecorder(store.New(), count)
	expected := make([]Expectation, count)
	var wg sync.WaitGroup
	for i := range count {
		ref := testRef("Pod", fmt.Sprint(i), "v")
		expected[i] = Expectation{Ref: ref}
		wg.Go(func() {
			r.ObserveEvent(ref, false)
			r.UpsertPod(podResult(ref))
			r.ObserveEvent(ref, false)
			r.UpsertPod(podResult(ref))
			r.Stats()
			r.Measure(nil)
			r.MeasureProgress(nil)
		})
	}
	wg.Wait()
	if writes := r.Stats().PublicationWrites; writes != 2*count {
		t.Fatalf("publication writes=%d, want %d", writes, 2*count)
	}
	got := r.Measure(expected)
	if got.Completed != count || got.Missing != 0 || got.Overflow != 0 || got.Pending != 0 {
		t.Fatalf("unexpected concurrent measurement: %+v", got)
	}
}

func TestRecorderDeletionExpectationWithoutVersion(t *testing.T) {
	r := NewRecorder(store.New(), 4)
	ref := testRef("Pod", "p", "tombstone-version")
	r.ObserveEvent(ref, true)
	r.RemovePod(ref.UID)
	expected := ref
	expected.ResourceVersion = ""
	got := r.Measure([]Expectation{{Ref: expected, Deleted: true}, {Ref: testRef("Pod", "other", ""), Deleted: true}})
	if got.Completed != 1 || got.Missing != 1 || got.Samples[0].Ref.ResourceVersion != "tombstone-version" {
		t.Fatalf("unexpected deletion correlation: %+v", got)
	}
	if aliases := r.Measure([]Expectation{{Ref: expected, Deleted: true}, {Ref: ref, Deleted: true}}); len(aliases.Samples) != 1 || aliases.Unmatched != 0 {
		t.Fatalf("deletion aliases duplicated a sample: %+v", aliases)
	}
	r.ObserveEvent(ref, false)
	r.UpsertPod(podResult(ref))
	if got := r.Measure([]Expectation{{Ref: expected}}); got.Completed != 0 {
		t.Fatal("empty update version must not be a wildcard")
	}
}

func (b *blockingPublication) UpsertPod(result *store.PodResult) {
	close(b.entered)
	<-b.release
	b.PublicationStore.UpsertPod(result)
}

func TestRecorderCompletesAfterStoreCommit(t *testing.T) {
	publications := &blockingPublication{PublicationStore: store.New(), entered: make(chan struct{}), release: make(chan struct{})}
	r := NewRecorder(publications, 1)
	ref := testRef("Pod", "p", "v")
	r.ObserveEvent(ref, false)
	done := make(chan struct{})
	go func() { r.UpsertPod(podResult(ref)); close(done) }()
	<-publications.entered
	before := r.Stats()
	close(publications.release)
	<-done
	if before.PublicationWrites != 0 || r.Stats().PublicationWrites != 1 || before.Completed != 0 || r.Stats().Completed != 1 || publications.Pod(ref.UID) == nil {
		t.Fatal("completion must follow underlying commit")
	}
}

func TestRecorderProgressMatchesFullMeasurement(t *testing.T) {
	r := NewRecorder(store.New(), 5)
	old := testRef("Pod", "p", "superseded")
	current := testRef("Pod", "p", "current")
	deleted := testRef("Deployment", "d", "tombstone")
	extra := testRef("Pod", "extra", "v")
	missing := testRef("Pod", "missing", "v")
	for _, ref := range []store.ObjectRef{old, current, extra, missing} {
		r.ObserveEvent(ref, false)
	}
	r.ObserveEvent(deleted, true)
	r.ObserveEvent(testRef("Pod", "overflow", "v"), false)
	r.UpsertPod(podResult(current))
	r.UpsertPod(podResult(extra))
	r.RemoveWorkload(deleted.UID)
	wildcard := deleted
	wildcard.ResourceVersion = ""
	cases := []struct {
		name     string
		expected []Expectation
	}{
		{name: "empty"},
		{name: "completed", expected: []Expectation{{Ref: current}, {Ref: wildcard, Deleted: true}}},
		{name: "missing", expected: []Expectation{{Ref: missing}, {Ref: testRef("Pod", "absent", "v")}}},
		{name: "duplicates", expected: []Expectation{{Ref: current}, {Ref: current}, {Ref: wildcard, Deleted: true}, {Ref: deleted, Deleted: true}}},
		{name: "exact versions", expected: []Expectation{{Ref: old}, {Ref: testRef("Pod", "p", "")}, {Ref: testRef("Deployment", "d", "wrong"), Deleted: true}}},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			progress := r.MeasureProgress(tt.expected)
			full := r.Measure(tt.expected)
			if progress.Latency != nil || progress.Samples != nil {
				t.Fatal("progress allocated detailed output")
			}
			if progress.Expected != full.Expected || progress.Observed != full.Observed || progress.Completed != full.Completed || progress.Missing != full.Missing || progress.Overflow != full.Overflow || progress.Pending != full.Pending || progress.Unmatched != full.Unmatched {
				t.Fatalf("progress %+v differs from full %+v", progress, full)
			}
			if progress.Overflow != 1 || progress.Pending != 2 {
				t.Fatalf("overflow or superseded/uncompleted callbacks lost: %+v", progress)
			}
		})
	}
	progress := r.MeasureProgress(cases[1].expected)
	if progress.Completed != 2 || progress.Missing != 0 || progress.Pending != 2 || progress.Unmatched != 1 {
		t.Fatalf("pending must not suppress expected convergence: %+v", progress)
	}
}

func TestRecorderCountsEveryPublicationWrite(t *testing.T) {
	r := NewRecorder(store.New(), 0)
	pod := podResult(testRef("Pod", "p", "v"))
	workload := workloadResult(testRef("Deployment", "d", "v"))
	for _, write := range []func(){
		func() { r.UpsertPod(pod) },
		func() { r.UpsertPod(pod) },
		func() { pod.HasValue = false; pod.Error = store.ReasonEvaluationFailed; r.UpsertPod(pod) },
		func() { r.UpsertWorkload(workload) },
		func() {
			workload.HasValue = false
			workload.StatusError = store.ReasonEvaluationFailed
			r.UpsertWorkload(workload)
		},
		func() { r.RemovePod("p") },
		func() { r.RemovePod("absent") },
		func() { r.RemoveWorkload("d") },
		func() { r.RemoveWorkload("absent") },
	} {
		before := r.Stats().PublicationWrites
		write()
		stats := r.Stats()
		if stats.PublicationWrites != before+1 || stats.Completed != 0 || stats.Observed != 0 {
			t.Fatalf("write without matched observation was not counted independently: %+v", stats)
		}
	}
	r.Begin()
	if r.Stats().PublicationWrites != 0 {
		t.Fatal("Begin did not reset publication writes")
	}
	r.RemovePod("absent")
	if r.Stats().PublicationWrites != 1 {
		t.Fatal("counter did not restart after Begin")
	}
}
