// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 NVIDIA Corporation

package main

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/dsx-ai-factory/workload-map/karta-metrics-exporter/pkg/store"
	v1alpha1 "github.com/dsx-ai-factory/workload-map/karta/sdk/pkg/api/runai/v1alpha1"
	"github.com/dsx-ai-factory/workload-map/karta/sdk/pkg/resource"
)

func TestLiveDefinitionReinterpretsExistingStatus(t *testing.T) {
	definition, err := newLiveDefinition()
	if err != nil {
		t.Fatal(err)
	}
	if err := v1alpha1.NewKartaValidator(definition).Validate(); err != nil {
		t.Fatal(err)
	}
	object := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1", "kind": "Deployment",
		"metadata": map[string]any{"name": "sample", "namespace": "load", "uid": "root", "resourceVersion": "unchanged"},
		"status":   map[string]any{"conditions": []any{map[string]any{"type": "Progressing", "status": "True", "reason": "NewReplicaSetCreated"}}},
	}}
	for _, phase := range []v1alpha1.ResourceStatus{v1alpha1.InitializingStatus, v1alpha1.RunningStatus} {
		component, err := resource.NewComponentFactoryFromObject(definition, object).GetRootComponent()
		if err != nil {
			t.Fatal(err)
		}
		status, err := component.GetStatus(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if len(status.MatchedStatuses) != 1 || status.MatchedStatuses[0] != phase {
			t.Fatalf("status=%v, want %s", status.MatchedStatuses, phase)
		}
		if phase == v1alpha1.InitializingStatus {
			reviseLiveDefinition(definition)
		}
	}
	if err := v1alpha1.NewKartaValidator(definition).Validate(); err != nil {
		t.Fatal(err)
	}
	structure := definition.Spec.StructureDefinition
	if structure.RootComponent.Name != revisedComponent || *structure.ChildComponents[0].OwnerRef != revisedComponent {
		t.Fatal("definition ownership was not renamed consistently")
	}
	if object.GetResourceVersion() != "unchanged" {
		t.Fatal("workload was modified")
	}
	baseline, err := newLiveDefinition()
	if err != nil {
		t.Fatal(err)
	}
	if baseline.Spec.StructureDefinition.RootComponent.Name != "deployment" {
		t.Fatal("catalog was mutated")
	}
}

func TestValidateLiveDefinitionRequiresBothNewOutputsAndStableInputs(t *testing.T) {
	fixture := Fixture{Namespace: "load", Name: "sample", RootUID: "root", ReplicaSetUID: "rs", PodUID: "pod", RootVersion: "10", PodVersion: "11"}
	expected := DefinitionExpectation{Name: liveDefinitionName, Source: "cluster", Component: revisedComponent, Phase: "Running"}
	for _, tc := range []struct {
		name      string
		mutate    func(*store.CapturedView)
		wantError bool
	}{
		{"valid", func(_ *store.CapturedView) {}, false},
		{"old root semantics", func(v *store.CapturedView) { v.Workloads[0].Record.Phases = []string{"Initializing"} }, true},
		{"old pod semantics", func(v *store.CapturedView) { v.Pods[0].Record.Component = "deployment" }, true},
		{"changed workload version", func(v *store.CapturedView) { v.Workloads[0].Record.Ref.ResourceVersion = "12" }, true},
		{"changed pod version", func(v *store.CapturedView) { v.Pods[0].Input.ResourceVersion = "12" }, true},
		{"catalog instead of cluster", func(v *store.CapturedView) { v.Pods[0].Record.KartaSource = "catalog" }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			view := validFixtureView(fixture)
			view.Workloads[0].Record.Karta, view.Workloads[0].Record.KartaSource = liveDefinitionName, "cluster"
			view.Workloads[0].Record.Phases = []string{"Running"}
			view.Pods[0].Record.Karta, view.Pods[0].Record.KartaSource, view.Pods[0].Record.Component = liveDefinitionName, "cluster", revisedComponent
			tc.mutate(&view)
			err := validateFixturesWithDefinition(view, []Fixture{fixture}, "0", "0", false, expected)
			if (err != nil) != tc.wantError {
				t.Fatalf("error=%v, want error=%t", err, tc.wantError)
			}
		})
	}
}
