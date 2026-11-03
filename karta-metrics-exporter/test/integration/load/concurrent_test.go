// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 NVIDIA Corporation

package main

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"

	inventory "github.com/dsx-ai-factory/workload-map/karta-metrics-exporter/pkg/kartadefinitioninventory"
	"github.com/dsx-ai-factory/workload-map/karta-metrics-exporter/pkg/store"
	v1alpha1 "github.com/dsx-ai-factory/workload-map/karta/sdk/pkg/api/runai/v1alpha1"
)

func testConcurrentCohorts() []ConcurrentCohort {
	var cohorts []ConcurrentCohort
	for i, kind := range concurrentKinds {
		f := definitionFixture(i)
		f.RootKind = kind
		previous := inventory.Origin{Source: inventory.SourceCluster, Name: concurrentNames()[i], UID: types.UID(fmt.Sprint("karta-", i)), ResourceVersion: "old"}
		updated := previous
		updated.ResourceVersion = "expected"
		cohorts = append(cohorts, ConcurrentCohort{Previous: previous, Updated: updated, Expected: DefinitionExpectation{Name: previous.Name, Source: "cluster", Component: fmt.Sprint("component-", i), Phase: "Running"}, Fixtures: []Fixture{f}})
	}
	return cohorts
}

func concurrentResults(c ConcurrentCohort) (*store.WorkloadResult, *store.PodResult) {
	root, pod := definitionResults(c.Fixtures[0])
	root.Record.Karta = c.Expected.Name
	pod.Record.Karta = c.Expected.Name
	pod.Record.Component = c.Expected.Component
	root.Record.StatusLabels = map[string]string{RootMetricLabel: "0"}
	pod.Record.Labels = map[string]string{RootMetricLabel: "0", PodMetricLabel: "0"}
	return root, pod
}

func TestConcurrentExactVersionAndIndependentStarts(t *testing.T) {
	r := NewRecorder(store.New(), 100)
	cohorts := testConcurrentCohorts()
	if err := r.armConcurrent(cohorts); err != nil {
		t.Fatal(err)
	}
	base := time.Now()
	for i, c := range cohorts {
		receipt := base.Add(time.Duration(i) * time.Second)
		old := c.Previous
		r.concurrent.observe(old, receipt.Add(-time.Second))
		other := c.Updated
		other.ResourceVersion = "different"
		r.concurrent.observe(other, receipt.Add(-time.Millisecond))
		root, pod := concurrentResults(c)
		r.completeConcurrentRoot(root, receipt.Add(-time.Microsecond))
		r.completeConcurrentPod(pod, receipt.Add(-time.Microsecond))
		r.concurrent.observe(c.Updated, receipt)
		r.completeConcurrentRoot(root, receipt.Add(10*time.Millisecond))
		r.completeConcurrentPod(pod, receipt.Add(20*time.Millisecond))
	}
	for _, m := range r.measureConcurrent(true) {
		if m.Observed || m.Completed != 0 {
			t.Fatal("unbound origins accepted", m)
		}
	}
	if err := r.bindConcurrent(cohorts); err != nil {
		t.Fatal(err)
	}
	for i, m := range r.measureConcurrent(true) {
		if !m.Observed || m.Completed != 2 || m.Missing != 0 || !m.Receipt.Equal(base.Add(time.Duration(i)*time.Second)) || m.Roots.P50MS != 10 || m.Pods.P50MS != 20 {
			t.Fatalf("incorrect independent measurement: %+v", m)
		}
	}
}

func TestConcurrentRejectsCrossCohortAndStalePublications(t *testing.T) {
	for _, mismatch := range []string{"karta", "root-rv", "pod-rv", "owner-rv", "component", "phase", "uid", "labels"} {
		t.Run(mismatch, func(t *testing.T) {
			r := NewRecorder(store.New(), 100)
			cohorts := testConcurrentCohorts()
			if err := r.armConcurrent(cohorts); err != nil {
				t.Fatal(err)
			}
			if err := r.bindConcurrent(cohorts); err != nil {
				t.Fatal(err)
			}
			for _, c := range cohorts {
				r.ObserveDefinition(c.Updated)
			}
			root, pod := concurrentResults(cohorts[0])
			switch mismatch {
			case "karta":
				root.Record.Karta = cohorts[1].Expected.Name
				pod.Record.Karta = cohorts[1].Expected.Name
			case "root-rv":
				root.Record.Ref.ResourceVersion = "stale"
			case "pod-rv":
				pod.Input.ResourceVersion = "stale"
			case "owner-rv":
				pod.Record.Workload.ResourceVersion = "stale"
			case "component":
				pod.Record.Component = "old"
			case "phase":
				root.Record.Phases = []string{"Initializing"}
			case "uid":
				root.Record.UID = "other"
				pod.Record.UID = "other"
			case "labels":
				root.Record.StatusLabels[RootMetricLabel] = "bad"
				pod.Record.Labels[PodMetricLabel] = "bad"
			}
			r.UpsertWorkload(root)
			r.UpsertPod(pod)
			ms := r.measureConcurrent(true)
			if ms[0].Completed == 2 || ms[1].Completed != 0 || ms[2].Completed != 0 {
				t.Fatalf("invalid attribution %+v", ms)
			}
		})
	}
}

func TestConcurrentBudgetAndRace(t *testing.T) {
	cohorts := testConcurrentCohorts()
	if err := NewRecorder(store.New(), 5).armConcurrent(cohorts); err == nil {
		t.Fatal("accepted undersized sample budget")
	}
	r := NewRecorder(store.New(), 12)
	if err := r.armConcurrent(cohorts); err != nil {
		t.Fatal(err)
	}
	if err := r.bindConcurrent(cohorts); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for _, c := range cohorts {
		wg.Go(func() {
			for j := 0; j < 10; j++ {
				r.ObserveDefinition(c.Updated)
				root, pod := concurrentResults(c)
				r.UpsertWorkload(root)
				r.UpsertPod(pod)
				_ = r.measureConcurrent(false)
			}
		})
	}
	wg.Wait()
	if r.concurrent.used > 12 || r.concurrent.overflow == 0 {
		t.Fatalf("unbounded recorder %+v", r.concurrent)
	}
	bad := testConcurrentCohorts()
	bad[1].Updated.UID = "wrong"
	if err := r.bindConcurrent(bad); err == nil {
		t.Fatal("accepted wrong definition identity")
	}
}

func TestConcurrentMutationBarrier(t *testing.T) {
	definitions, err := newConcurrentDefinitions()
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{}, len(definitions))
	release := make(chan struct{})
	go func() {
		for range definitions {
			<-entered
		}
		close(release)
	}()
	at, results := updateConcurrent(t.Context(), definitions, func(_ context.Context, d *v1alpha1.Karta) error {
		entered <- struct{}{}
		<-release
		d.ResourceVersion = "next"
		return nil
	})
	latestStart := at
	earliestEnd := time.Now()
	for _, m := range results {
		if m.Start.Before(at) || m.End.Before(m.Start) || m.Updated.ResourceVersion != "next" {
			t.Fatal(m)
		}
		if m.Start.After(latestStart) {
			latestStart = m.Start
		}
		if m.End.Before(earliestEnd) {
			earliestEnd = m.End
		}
	}
	if latestStart.After(earliestEnd) {
		t.Fatal("barrier did not overlap requests")
	}
}

func TestConcurrentFixtureOwnershipAndDefinitions(t *testing.T) {
	definitions, err := newConcurrentDefinitions()
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range definitions {
		root := &d.Spec.StructureDefinition.RootComponent
		root.Name += "-revised"
		root.StatusDefinition.StatusMappings.Running = root.StatusDefinition.StatusMappings.Initializing
		root.StatusDefinition.StatusMappings.Initializing = nil
		if err := v1alpha1.NewKartaValidator(d).Validate(); err != nil {
			t.Fatal(err)
		}
	}
	api := fake.NewClientset()
	for i, kind := range concurrentKinds {
		f, err := createConcurrentFixture(t.Context(), api, "load", fmt.Sprint(i), kind)
		if err != nil {
			t.Fatal(err)
		}
		if f.RootKind != kind {
			t.Fatal(f)
		}
		pod, err := api.CoreV1().Pods("load").Get(t.Context(), f.Name, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		expected := kind
		if kind == "Deployment" {
			expected = "ReplicaSet"
		}
		if len(pod.OwnerReferences) != 1 || pod.OwnerReferences[0].Kind != expected {
			t.Fatal(pod.OwnerReferences)
		}
	}
}

func TestWorkloadPhaseValidation(t *testing.T) {
	for _, phase := range []string{"all", "root-update", "pod-update", "delete", "live-create"} {
		cfg := validTestOptions(t)
		cfg.WorkloadPhase = phase
		if err := cfg.validate(); err != nil {
			t.Fatal(err)
		}
	}
	cfg := validTestOptions(t)
	cfg.WorkloadPhase = "unknown"
	if cfg.validate() == nil {
		t.Fatal("accepted invalid phase")
	}
}

func TestConcurrentRejectsUnrelatedDefinitionReceipts(t *testing.T) {
	for _, field := range []string{"uid", "name", "source", "old-rv", "other-rv"} {
		t.Run(field, func(t *testing.T) {
			r := NewRecorder(store.New(), 100)
			cohorts := testConcurrentCohorts()
			if err := r.armConcurrent(cohorts); err != nil {
				t.Fatal(err)
			}
			if err := r.bindConcurrent(cohorts); err != nil {
				t.Fatal(err)
			}
			origin := cohorts[0].Updated
			switch field {
			case "uid":
				origin.UID = "other"
			case "name":
				origin.Name = cohorts[1].Updated.Name
			case "source":
				origin.Source = inventory.SourceCatalog
			case "old-rv":
				origin.ResourceVersion = cohorts[0].Previous.ResourceVersion
			case "other-rv":
				origin.ResourceVersion = "unrelated"
			}
			r.ObserveDefinition(origin)
			root, pod := concurrentResults(cohorts[0])
			r.UpsertWorkload(root)
			r.UpsertPod(pod)
			for _, m := range r.measureConcurrent(true) {
				if m.Observed || m.Completed != 0 {
					t.Fatalf("wrong receipt accepted: %+v", m)
				}
			}
		})
	}
}

func TestConcurrentSampleBudgetBoundary(t *testing.T) {
	for _, tc := range []struct {
		scenario      string
		roots, budget int
		valid         bool
	}{
		{"karta-concurrent", 3, 14, false}, {"karta-concurrent", 3, 15, true},
		{"karta-concurrent", 50000, 200002, false}, {"karta-concurrent", 50000, 200003, true},
		{"karta-update", 3, 5, false}, {"karta-update", 3, 6, true},
		{"karta-concurrent", math.MaxInt/4 + 1, math.MaxInt, false},
	} {
		t.Run(fmt.Sprintf("%s/%d/%d", tc.scenario, tc.roots, tc.budget), func(t *testing.T) {
			cfg := validTestOptions(t)
			cfg.Scenario = tc.scenario
			cfg.Roots = []int{tc.roots}
			cfg.MaxSamples = tc.budget
			cfg.CRDDirectory = t.TempDir()
			if err := os.WriteFile(filepath.Join(cfg.CRDDirectory, "run.ai_kartas.yaml"), nil, 0600); err != nil {
				t.Fatal(err)
			}
			if err := cfg.validate(); (err == nil) != tc.valid {
				t.Fatalf("valid=%v error=%v", tc.valid, err)
			}
		})
	}
}

func TestConcurrentStatusOmitsDistributions(t *testing.T) {
	r := NewRecorder(store.New(), 100)
	cohorts := testConcurrentCohorts()
	if err := r.armConcurrent(cohorts); err != nil {
		t.Fatal(err)
	}
	if err := r.bindConcurrent(cohorts); err != nil {
		t.Fatal(err)
	}
	for _, c := range cohorts {
		r.ObserveDefinition(c.Updated)
		root, pod := concurrentResults(c)
		r.UpsertWorkload(root)
		r.UpsertPod(pod)
	}
	status, collection := r.measureConcurrent(false), r.measureConcurrent(true)
	for i, s := range status {
		c := collection[i]
		if s.Expected != c.Expected || s.Completed != c.Completed || s.Missing != c.Missing || s.Observed != c.Observed || s.Overflow != c.Overflow {
			t.Fatalf("status differs from collection: %+v %+v", s, c)
		}
		if s.Roots != nil || s.Pods != nil || s.Samples != nil {
			t.Fatalf("status allocated latency results: %+v", s)
		}
		if c.Roots == nil || c.Pods == nil || len(c.Samples) != 2 {
			t.Fatalf("collection omitted latency: %+v", c)
		}
	}
}
