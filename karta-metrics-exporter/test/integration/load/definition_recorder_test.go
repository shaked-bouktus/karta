// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 NVIDIA Corporation

package main

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"

	inventory "github.com/dsx-ai-factory/workload-map/karta-metrics-exporter/pkg/kartadefinitioninventory"
	"github.com/dsx-ai-factory/workload-map/karta-metrics-exporter/pkg/store"
)

func definitionFixture(index int) Fixture {
	return Fixture{Namespace: "test", Name: fmt.Sprint(index), RootUID: types.UID(fmt.Sprintf("root-%d", index)), PodUID: types.UID(fmt.Sprintf("pod-%d", index)), RootVersion: "root-rv", PodVersion: "pod-rv"}
}

func definitionOrigin(version string) inventory.Origin {
	return inventory.Origin{Source: inventory.SourceCluster, Name: "load-definition", UID: "definition-uid", ResourceVersion: version}
}

func definitionResults(f Fixture) (*store.WorkloadResult, *store.PodResult) {
	refs := fixtureExpectations(f, false)
	root := &store.WorkloadResult{HasValue: true, Record: store.WorkloadRecord{UID: f.RootUID, Ref: refs[0].Ref, Karta: "load-definition", KartaSource: "cluster", OwnerResolution: store.Resolved, HasStatus: true, Phases: []string{"Running"}}}
	pod := &store.PodResult{HasValue: true, Input: refs[1].Ref, Record: store.PodRecord{UID: f.PodUID, Name: f.Name, Namespace: f.Namespace, Workload: refs[0].Ref, Karta: "load-definition", KartaSource: "cluster", OwnerResolution: store.Resolved, Component: "deployment-revised"}}
	return root, pod
}

func TestDefinitionRecorderRequiresNewMatchingOrigin(t *testing.T) {
	r := NewRecorder(store.New(), 2)
	f := definitionFixture(0)
	if err := r.ArmDefinition(definitionOrigin("old"), []Fixture{f}, "deployment-revised", "Running"); err != nil {
		t.Fatal(err)
	}
	root, pod := definitionResults(f)
	r.UpsertWorkload(root)
	r.UpsertPod(pod)
	for _, field := range []string{"same-version", "empty-version", "uid", "name", "source"} {
		origin := definitionOrigin("new")
		switch field {
		case "same-version":
			origin.ResourceVersion = "old"
		case "empty-version":
			origin.ResourceVersion = ""
		case "uid":
			origin.UID = "other"
		case "name":
			origin.Name = "other"
		case "source":
			origin.Source = inventory.SourceCatalog
		}
		r.ObserveDefinition(origin)
	}
	if got := r.MeasureDefinition(true); got.Observed || got.Completed != 0 || got.Expected != 2 || got.Missing != 2 || got.Roots != nil || got.Pods != nil {
		t.Fatalf("unobserved measurement: %+v", got)
	}
	if stats := r.Stats(); stats.Observed != 0 || stats.Completed != 0 || stats.Pending != 0 {
		t.Fatalf("unobserved progress: %+v", stats)
	}
	r.ObserveDefinition(definitionOrigin("new"))
	start := r.definition.start
	r.ObserveDefinition(definitionOrigin("later"))
	if !r.definition.start.Equal(start) || r.definition.observed != definitionOrigin("new") {
		t.Fatal("duplicate definition callback changed start")
	}
	if stats := r.Stats(); stats.Observed != 2 || stats.Pending != 2 {
		t.Fatalf("observed progress: %+v", stats)
	}
	r.UpsertWorkload(root)
	r.UpsertPod(pod)
	r.UpsertWorkload(root)
	r.UpsertPod(pod)
	got := r.MeasureDefinition(true)
	if !got.Observed || got.Definition != definitionOrigin("new") || got.Completed != 2 || got.Missing != 0 || len(got.Samples) != 2 || got.Roots.Count != 1 || got.Pods.Count != 1 {
		t.Fatalf("completed measurement: %+v", got)
	}
	for _, sample := range got.Samples {
		if sample.LatencyNS < 0 {
			t.Fatal("negative latency")
		}
	}
	if got := r.MeasureDefinition(false); len(got.Samples) != 0 || got.Roots.Count != 1 || got.Pods.Count != 1 {
		t.Fatalf("summary measurement: %+v", got)
	}
	r.Begin()
	if r.definition != nil || r.Stats() != (RecorderStats{}) || r.MeasureDefinition(true).Expected != 0 {
		t.Fatal("Begin did not reset definition recording")
	}
	ref := testRef("Pod", "normal", "1")
	r.ObserveEvent(ref, false)
	r.UpsertPod(podResult(ref))
	if r.Measure([]Expectation{{Ref: ref}}).Completed != 1 {
		t.Fatal("normal mode not restored")
	}
}

func TestDefinitionRecorderRejectsInvalidPublications(t *testing.T) {
	for _, kind := range []string{"root", "pod"} {
		for _, mismatch := range []string{"no-value", "error", "source", "definition", "uid", "version", "name", "namespace", "unresolved", "semantic"} {
			t.Run(kind+"/"+mismatch, func(t *testing.T) {
				r := NewRecorder(store.New(), 2)
				f := definitionFixture(0)
				if err := r.ArmDefinition(definitionOrigin("old"), []Fixture{f}, "deployment-revised", "Running"); err != nil {
					t.Fatal(err)
				}
				r.ObserveDefinition(definitionOrigin("new"))
				root, pod := definitionResults(f)
				switch mismatch {
				case "no-value":
					root.HasValue = false
					pod.HasValue = false
				case "error":
					root.StatusError = "invalid"
					pod.Error = "invalid"
				case "source":
					root.Record.KartaSource = "catalog"
					pod.Record.KartaSource = "catalog"
				case "definition":
					root.Record.Karta = "other"
					pod.Record.Karta = "other"
				case "uid":
					root.Record.UID = "other"
					pod.Record.UID = "other"
				case "version":
					root.Record.Ref.ResourceVersion = "other"
					pod.Input.ResourceVersion = "other"
				case "name":
					root.Record.Ref.Name = "other"
					pod.Input.Name = "other"
				case "namespace":
					root.Record.Ref.Namespace = "other"
					pod.Input.Namespace = "other"
				case "unresolved":
					root.Record.OwnerResolution = store.UnknownOwner
					pod.Record.OwnerResolution = store.UnknownOwner
				case "semantic":
					root.Record.Phases = []string{"Initializing"}
					pod.Record.Component = "deployment"
				}
				if kind == "root" {
					r.UpsertWorkload(root)
				} else {
					r.UpsertPod(pod)
				}
				if got := r.MeasureDefinition(true); got.Completed != 0 || got.Missing != 2 {
					t.Fatalf("invalid publication accepted: %+v", got)
				}
			})
		}
	}
	t.Run("pod-root-revision", func(t *testing.T) {
		r := NewRecorder(store.New(), 2)
		f := definitionFixture(0)
		if err := r.ArmDefinition(definitionOrigin("old"), []Fixture{f}, "deployment-revised", "Running"); err != nil {
			t.Fatal(err)
		}
		r.ObserveDefinition(definitionOrigin("new"))
		_, pod := definitionResults(f)
		pod.Record.Workload.ResourceVersion = "other"
		r.UpsertPod(pod)
		if r.MeasureDefinition(true).Completed != 0 {
			t.Fatal("wrong root mapping accepted")
		}
	})
}

func TestDefinitionRecorderBudgetAndInvalidFixtureDoNotArm(t *testing.T) {
	r := NewRecorder(store.New(), 1)
	if err := r.ArmDefinition(definitionOrigin("old"), []Fixture{definitionFixture(0)}, "deployment-revised", "Running"); err == nil {
		t.Fatal("expected sample budget error")
	}
	r.ObserveDefinition(definitionOrigin("new"))
	if r.definition != nil || r.Stats().Overflow != 1 || r.MeasureDefinition(true).Observed {
		t.Fatal("overflow armed observer")
	}
	r = NewRecorder(store.New(), 4)
	f := definitionFixture(0)
	if err := r.ArmDefinition(definitionOrigin("old"), []Fixture{f, f}, "deployment-revised", "Running"); err == nil || r.definition != nil {
		t.Fatal("duplicate fixtures armed observer")
	}
	f.PodVersion = ""
	if err := r.ArmDefinition(definitionOrigin("old"), []Fixture{f}, "deployment-revised", "Running"); err == nil || r.definition != nil {
		t.Fatal("unversioned fixture armed observer")
	}
}

func TestDefinitionRecorderCompletesAfterRealCommit(t *testing.T) {
	publications := &blockingPublication{PublicationStore: store.New(), entered: make(chan struct{}), release: make(chan struct{})}
	r := NewRecorder(publications, 2)
	f := definitionFixture(0)
	if err := r.ArmDefinition(definitionOrigin("old"), []Fixture{f}, "deployment-revised", "Running"); err != nil {
		t.Fatal(err)
	}
	r.ObserveDefinition(definitionOrigin("new"))
	_, pod := definitionResults(f)
	done := make(chan struct{})
	go func() { r.UpsertPod(pod); close(done) }()
	select {
	case <-publications.entered:
	case <-time.After(5 * time.Second):
		close(publications.release)
		t.Fatal("write did not start")
	}
	if r.MeasureDefinition(true).Completed != 0 {
		t.Error("completion preceded real Store commit")
	}
	close(publications.release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("write did not complete")
	}
	if r.MeasureDefinition(true).Completed != 1 {
		t.Fatal("committed write not recorded")
	}
}

func TestDefinitionRecorderConcurrentCompletion(t *testing.T) {
	const count = 50
	fixtures := make([]Fixture, count)
	for i := range fixtures {
		fixtures[i] = definitionFixture(i)
	}
	r := NewRecorder(store.New(), 2*count)
	if err := r.ArmDefinition(definitionOrigin("old"), fixtures, "deployment-revised", "Running"); err != nil {
		t.Fatal(err)
	}
	r.ObserveDefinition(definitionOrigin("new"))
	var wg sync.WaitGroup
	for _, f := range fixtures {
		wg.Go(func() {
			root, pod := definitionResults(f)
			r.ObserveEvent(pod.Input, false)
			r.UpsertWorkload(root)
			r.UpsertPod(pod)
			r.UpsertWorkload(root)
			r.UpsertPod(pod)
			r.ObserveDefinition(definitionOrigin("new"))
			r.Stats()
			r.MeasureDefinition(false)
		})
	}
	wg.Wait()
	got := r.MeasureDefinition(true)
	if got.Completed != 2*count || got.Missing != 0 || got.Roots.Count != count || got.Pods.Count != count || got.Overflow != 0 || r.Stats().Pending != 0 {
		t.Fatalf("concurrent measurement: %+v", got)
	}
}

func TestDefinitionRecorderUsesCapturedCommitOrder(t *testing.T) {
	r := NewRecorder(store.New(), 2)
	f := definitionFixture(0)
	if err := r.ArmDefinition(definitionOrigin("old"), []Fixture{f}, "deployment-revised", "Running"); err != nil {
		t.Fatal(err)
	}
	r.ObserveDefinition(definitionOrigin("new"))
	root, _ := definitionResults(f)
	start := r.definition.start
	r.completeDefinitionRoot(root, start.Add(-time.Nanosecond))
	if r.MeasureDefinition(true).Completed != 0 {
		t.Fatal("write committed before observation was counted")
	}
	// Callbacks may acquire the recorder lock in a different order from Store commits.
	r.completeDefinitionRoot(root, start.Add(10*time.Millisecond))
	r.completeDefinitionRoot(root, start.Add(5*time.Millisecond))
	got := r.MeasureDefinition(true)
	if got.Completed != 1 || got.Roots.P50MS != 5 {
		t.Fatalf("earliest captured commit lost: %+v", got)
	}
}
