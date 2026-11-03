// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 NVIDIA Corporation

package main

import (
	"context"
	"fmt"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	inventory "github.com/dsx-ai-factory/workload-map/karta-metrics-exporter/pkg/kartadefinitioninventory"
	"github.com/dsx-ai-factory/workload-map/karta-metrics-exporter/pkg/store"
)

func rootFixture() (target, *store.WorkloadResult) {
	ref := store.ObjectRef{GVK: deployGVK, Name: "root", Namespace: "load-cli", UID: "root-uid", ResourceVersion: "opaque-response"}
	t := target{Ref: ref, Root: ref, Definition: "load-cli", Phase: "Running", RootRevision: "1", PodRevision: "1", Component: "deployment"}
	r := &store.WorkloadResult{HasValue: true, Record: store.WorkloadRecord{UID: ref.UID, Ref: ref, Karta: t.Definition, KartaSource: "cluster", OwnerResolution: store.Resolved, HasStatus: true, Phases: []string{"Running"}, StatusLabels: map[string]string{"root_revision": "1"}}}
	return t, r
}
func TestEarlyObservationsOpaqueVersionAndRealStore(t *testing.T) {
	expected, r := rootFixture()
	o := newObserver(8)
	o.arm(map[types.UID]target{expected.Ref.UID: expected})
	o.event(r.Record.Ref, false)
	o.UpsertWorkload(r)
	if o.Workload(expected.Ref.UID) == nil {
		t.Fatal("real store was not written")
	}
	// Only now does the hypothetical Update response expose its opaque RV.
	roots, _, err := o.results(map[types.UID]target{expected.Ref.UID: expected}, nil)
	if err != nil || roots.Completed != 1 || roots.Samples[0].Nanoseconds < 0 {
		t.Fatalf("early observations lost: %+v %v", roots, err)
	}
	expected.Ref.ResourceVersion = "different-opaque-response"
	if _, _, err = o.results(map[types.UID]target{expected.Ref.UID: expected}, nil); err == nil {
		t.Fatal("stale RV completed a sample")
	}
}
func TestInvalidAndStaleSemanticsCannotComplete(t *testing.T) {
	for _, mutate := range []func(*store.WorkloadResult){
		func(r *store.WorkloadResult) { r.Error = store.ReasonEvaluationFailed },
		func(r *store.WorkloadResult) { r.StatusError = store.ReasonEvaluationFailed },
		func(r *store.WorkloadResult) { r.HasValue = false },
		func(r *store.WorkloadResult) { r.Record.StatusLabels["root_revision"] = "0" },
		func(r *store.WorkloadResult) { r.Record.Phases = []string{"Completed"} },
		func(r *store.WorkloadResult) { r.Record.KartaSource = "catalog" },
	} {
		expected, r := rootFixture()
		o := newObserver(8)
		o.arm(map[types.UID]target{expected.Ref.UID: expected})
		o.event(r.Record.Ref, false)
		mutate(r)
		o.UpsertWorkload(r)
		if o.pending(map[types.UID]target{expected.Ref.UID: expected}, nil) == nil {
			t.Fatal("invalid write completed")
		}
	}
	expected, r := rootFixture()
	source := store.SourceKey{Resource: schema.GroupVersionResource{Version: "v1", Resource: "pods"}}
	r.Sources = []store.SourceKey{source}
	o := newObserver(8)
	o.arm(map[types.UID]target{expected.Ref.UID: expected})
	o.event(r.Record.Ref, false)
	o.UpsertWorkload(r)
	if o.invalid != 1 {
		t.Fatal("missing source health was accepted")
	}
}
func TestOneDefinitionReceiptSharedByRootAndPod(t *testing.T) {
	root, r := rootFixture()
	pod := root
	pod.Ref = store.ObjectRef{GVK: schema.GroupVersionKind{Version: "v1", Kind: "Pod"}, Namespace: root.Ref.Namespace, Name: "pod", UID: "pod-uid", ResourceVersion: "unchanged-pod"}
	targets := map[types.UID]target{root.Ref.UID: root, pod.Ref.UID: pod}
	o := newObserver(8)
	o.arm(targets)
	origin := inventory.Origin{Source: inventory.SourceCluster, Name: "load-cli", UID: "definition", ResourceVersion: "new-definition"}
	o.definition(origin)
	o.UpsertWorkload(r)
	o.UpsertPod(&store.PodResult{HasValue: true, Input: pod.Ref, Record: store.PodRecord{UID: pod.Ref.UID, Name: pod.Ref.Name, Namespace: pod.Ref.Namespace, Workload: root.Ref, Karta: root.Definition, KartaSource: "cluster", OwnerResolution: store.Resolved, Component: root.Component, Labels: map[string]string{"root_revision": "1", "pod_revision": "1"}}})
	roots, pods, err := o.results(targets, &key{origin.UID, origin.ResourceVersion})
	if err != nil || roots.Completed != 1 || pods.Completed != 1 || !roots.Samples[0].Receipt.Equal(pods.Samples[0].Receipt) {
		t.Fatalf("fanout: roots=%+v pods=%+v err=%v", roots, pods, err)
	}
	if o.pending(targets, &key{origin.UID, "old-definition"}) == nil {
		t.Fatal("wrong definition RV accepted")
	}
}
func TestNearestRankAndEmpty(t *testing.T) {
	empty := distribution{}
	summarize(&empty)
	if empty.P50 != nil || empty.P90 != nil || empty.P99 != nil {
		t.Fatal("empty percentiles")
	}
	d := distribution{Expected: 20}
	for i := int64(20); i > 0; i-- {
		d.Samples = append(d.Samples, sample{Nanoseconds: i})
	}
	summarize(&d)
	if *d.P50 != 10 || *d.P90 != 18 || *d.P99 != 20 || d.Missing != 0 {
		t.Fatalf("wrong nearest ranks: %+v", d)
	}
}
func TestQuietIncludesUnmatchedWritesAndOverflow(t *testing.T) {
	o := newObserver(1)
	expected, r := rootFixture()
	o.arm(map[types.UID]target{expected.Ref.UID: expected})
	o.event(expected.Ref, false)
	next := expected.Ref
	next.ResourceVersion = "second"
	o.event(next, false)
	if o.overflow != 1 {
		t.Fatal("overflow not counted")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				o.UpsertWorkload(r)
			}
		}
	}()
	if quiet(ctx, o, 20*time.Millisecond) == nil {
		t.Fatal("quiet gate ignored writes")
	}
	<-done
	if o.writes == 0 {
		t.Fatal("publication writes not counted")
	}
}

func TestInvalidPublicationsRetainTotalsAndFailOnlyMeasuredPhase(t *testing.T) {
	expected, r := rootFixture()
	r.HasValue = false
	o := newObserver(8)
	o.UpsertWorkload(r)
	o.arm(map[types.UID]target{expected.Ref.UID: expected})
	if count, err := o.finish(); count != 0 || err != nil {
		t.Fatalf("startup invalid leaked into phase: %d %v", count, err)
	}
	o.UpsertWorkload(r)
	o.arm(map[types.UID]target{expected.Ref.UID: expected})
	o.UpsertWorkload(r)
	if count, err := o.finish(); count != 1 || err == nil {
		t.Fatalf("measured invalid did not fail: %d %v", count, err)
	}
	o.UpsertWorkload(r)
	if o.invalid != 4 || o.measuredInvalid != 1 {
		t.Fatalf("totals lost: all=%d measured=%d", o.invalid, o.measuredInvalid)
	}
	o.arm(map[types.UID]target{expected.Ref.UID: expected})
	if count, err := o.finish(); count != 0 || err != nil {
		t.Fatalf("previous phase leaked: %d %v", count, err)
	}
}

func TestEveryStoreWriterResetsQuiet(t *testing.T) {
	expected, root := rootFixture()
	source := store.SourceKey{Resource: schema.GroupVersionResource{Version: "v1", Resource: "pods"}}
	writes := map[string]func(*observer){
		"upsert workload": func(o *observer) { o.UpsertWorkload(root) },
		"remove workload": func(o *observer) { o.RemoveWorkload(expected.Ref.UID) },
		"upsert pod": func(o *observer) {
			o.UpsertPod(&store.PodResult{Input: expected.Ref, Record: store.PodRecord{UID: expected.Ref.UID}})
		},
		"remove pod":               func(o *observer) { o.RemovePod(expected.Ref.UID) },
		"set health":               func(o *observer) { o.SetSourceHealth(source, store.SourceHealth{Synced: true}) },
		"remove health":            func(o *observer) { o.RemoveSourceHealth(source) },
		"set discovery failure":    func(o *observer) { o.SetDiscoveryFailure(expected.Ref, store.ReasonUnresolvedOwner) },
		"remove discovery failure": func(o *observer) { o.RemoveDiscoveryFailure(expected.Ref.UID) },
	}
	for name, write := range writes {
		t.Run(name, func(t *testing.T) {
			o := newObserver(8)
			before := time.Now()
			o.last = before.Add(-time.Hour)
			write(o)
			if o.writes != 1 || o.last.Before(before) {
				t.Fatalf("write did not reset quiet: writes=%d last=%v", o.writes, o.last)
			}
		})
	}
}

func TestDiscoveryFailureDelegatesAndPersistsInPhaseErrors(t *testing.T) {
	expected, _ := rootFixture()
	o := newObserver(8)
	o.SetDiscoveryFailure(expected.Ref, store.ReasonUnresolvedOwner)
	if failures := o.Capture().Failures; len(failures) != 1 || failures[0].Input != expected.Ref {
		t.Fatal("discovery failure did not reach real Store")
	}
	o.RemoveDiscoveryFailure(expected.Ref.UID)
	o.arm(map[types.UID]target{expected.Ref.UID: expected})
	o.SetDiscoveryFailure(expected.Ref, store.ReasonUnresolvedOwner)
	o.RemoveDiscoveryFailure(expected.Ref.UID)
	if len(o.Capture().Failures) != 0 {
		t.Fatal("discovery failure removal did not reach real Store")
	}
	if count, err := o.finish(); count != 1 || err == nil {
		t.Fatalf("recovered measured discovery failure was lost: %d %v", count, err)
	}
	if o.invalid != 2 || o.measuredInvalid != 1 {
		t.Fatalf("discovery failure accounting: total=%d measured=%d", o.invalid, o.measuredInvalid)
	}
}

func podFixture() (target, *store.PodResult) {
	root, _ := rootFixture()
	pod := root
	pod.Ref = store.ObjectRef{GVK: schema.GroupVersionKind{Version: "v1", Kind: "Pod"}, Namespace: root.Ref.Namespace, Name: "pod", UID: "pod-uid", ResourceVersion: "pod-response"}
	return pod, &store.PodResult{HasValue: true, Input: pod.Ref, Record: store.PodRecord{UID: pod.Ref.UID, Name: pod.Ref.Name, Namespace: pod.Ref.Namespace, Workload: root.Ref, Karta: root.Definition, KartaSource: "cluster", OwnerResolution: store.Resolved, Component: root.Component, Labels: map[string]string{"root_revision": "1", "pod_revision": "1"}}}
}

func TestPodSampleRejectsStaleOwnerVersionBeforeCorrectWrite(t *testing.T) {
	expected, r := podFixture()
	targets := map[types.UID]target{expected.Ref.UID: expected}
	o := newObserver(8)
	o.arm(targets)
	o.event(r.Input, false)
	r.Record.Workload.ResourceVersion = "stale-owner"
	o.UpsertPod(r)
	if o.pending(targets, nil) == nil {
		t.Fatal("stale owner RV completed sample")
	}
	r.Record.Workload.ResourceVersion = expected.Root.ResourceVersion
	correctStart := time.Now()
	o.UpsertPod(r)
	_, pods, err := o.results(targets, nil)
	if err != nil || pods.Completed != 1 || pods.Samples[0].Published.Before(correctStart) {
		t.Fatalf("correct endpoint missing: %+v %v", pods, err)
	}
}

func TestPodRejectsUnexpectedInstanceAndGroup(t *testing.T) {
	for _, field := range []string{"instance", "group"} {
		t.Run(field, func(t *testing.T) {
			expected, r := podFixture()
			targets := map[types.UID]target{expected.Ref.UID: expected}
			if field == "instance" {
				r.Record.Instance = "unexpected"
			} else {
				r.Record.Group = "unexpected"
			}
			if matchPod(r, expected, false) || matchPod(r, expected, true) {
				t.Fatal("unexpected resolved field accepted")
			}
			o := newObserver(8)
			o.arm(targets)
			o.event(r.Input, false)
			o.UpsertPod(r)
			if o.pending(targets, nil) == nil {
				t.Fatal("unexpected resolved field completed sample")
			}
		})
	}
}

func TestInvalidExamplesBoundedWithoutTruncatingCounts(t *testing.T) {
	expected, r := rootFixture()
	o := newObserver(32)
	o.arm(map[types.UID]target{expected.Ref.UID: expected})
	r.Error = store.ReasonEvaluationFailed
	for i := 0; i < 12; i++ {
		r.Record.Ref.ResourceVersion = fmt.Sprint(i)
		o.UpsertWorkload(r)
	}
	examples := o.invalidExamples()
	if len(examples) != 10 || o.phaseInvalid != 12 || o.measuredInvalid != 12 {
		t.Fatalf("examples=%d phase=%d measured=%d", len(examples), o.phaseInvalid, o.measuredInvalid)
	}
	for i, e := range examples {
		if e.UID != r.Record.UID || e.RV != fmt.Sprint(i) || e.Kind != "Deployment" || e.Error != store.ReasonEvaluationFailed || !e.HasValue || !e.SourcesValid {
			t.Fatalf("incomplete example: %+v", e)
		}
	}
	if _, err := o.finish(); err == nil {
		t.Fatal("diagnostics weakened failure")
	}
	o.arm(map[types.UID]target{expected.Ref.UID: expected})
	r.Error = ""
	r.Sources = []store.SourceKey{{Resource: schema.GroupVersionResource{Version: "v1", Resource: "pods"}}}
	o.UpsertWorkload(r)
	examples = o.invalidExamples()
	if len(examples) != 1 || examples[0].SourcesValid || examples[0].SourceReason != store.ReasonAPIUnavailable {
		t.Fatalf("source diagnostic/reset: %+v", examples)
	}
	_, pod := podFixture()
	pod.HasValue = false
	o.UpsertPod(pod)
	o.SetDiscoveryFailure(expected.Ref, store.ReasonUnresolvedOwner)
	examples = o.invalidExamples()
	if len(examples) != 3 || examples[1].Kind != "Pod" || examples[1].HasValue || examples[2].Operation != "discovery" || examples[2].Error != store.ReasonUnresolvedOwner {
		t.Fatalf("pod/discovery diagnostics: %+v", examples)
	}
}
