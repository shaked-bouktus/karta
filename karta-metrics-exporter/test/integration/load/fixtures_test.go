// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 NVIDIA Corporation

package main

import (
	"fmt"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/dsx-ai-factory/workload-map/karta-metrics-exporter/pkg/store"
)

func TestFixtureLifecycle(t *testing.T) {
	api := fake.NewClientset()
	version := 0
	api.PrependReactor("create", "*", func(action clienttesting.Action) (bool, runtime.Object, error) {
		object := action.(clienttesting.CreateAction).GetObject()
		metadata, err := meta.Accessor(object)
		if err != nil {
			return true, nil, err
		}
		version++
		metadata.SetUID(types.UID(fmt.Sprintf("uid-%d", version)))
		metadata.SetResourceVersion(fmt.Sprint(version))
		return false, nil, nil
	})
	f, err := createFixture(t.Context(), api, "load", "sample")
	if err != nil {
		t.Fatal(err)
	}
	root, err := api.AppsV1().Deployments(f.Namespace).Get(t.Context(), f.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	rs, err := api.AppsV1().ReplicaSets(f.Namespace).Get(t.Context(), f.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	pod, err := api.CoreV1().Pods(f.Namespace).Get(t.Context(), f.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if f.RootUID == "" || f.ReplicaSetUID == "" || f.PodUID == "" || f.RootUID == f.PodUID {
		t.Fatalf("invalid identities: %+v", f)
	}
	if len(rs.OwnerReferences) != 1 || rs.OwnerReferences[0].UID != root.UID || rs.OwnerReferences[0].Kind != "Deployment" || rs.OwnerReferences[0].Controller == nil || !*rs.OwnerReferences[0].Controller {
		t.Fatalf("invalid ReplicaSet ownership: %+v", rs.OwnerReferences)
	}
	if len(pod.OwnerReferences) != 1 || pod.OwnerReferences[0].UID != rs.UID || pod.OwnerReferences[0].Kind != "ReplicaSet" || pod.OwnerReferences[0].Controller == nil || !*pod.OwnerReferences[0].Controller {
		t.Fatalf("invalid Pod ownership: %+v", pod.OwnerReferences)
	}
	if len(root.Status.Conditions) != 1 || root.Status.Conditions[0].Reason != "NewReplicaSetCreated" {
		t.Fatalf("incorrect status: %+v", root.Status)
	}
	if root.Labels[RootRevisionLabel] != "0" || pod.Labels[PodRevisionLabel] != "0" {
		t.Fatal("missing baseline labels")
	}
	api.ClearActions()
	f.RootVersion, f.PodVersion = "outdated-root", "outdated-pod"
	if err := updateRoot(t.Context(), api, &f, "1"); err != nil {
		t.Fatal(err)
	}
	if err := updatePod(t.Context(), api, &f, "2"); err != nil {
		t.Fatal(err)
	}
	for _, action := range api.Actions() {
		if action.GetVerb() != "patch" {
			t.Fatalf("unexpected update API call: %s", action.GetVerb())
		}
	}
	root, _ = api.AppsV1().Deployments(f.Namespace).Get(t.Context(), f.Name, metav1.GetOptions{})
	pod, _ = api.CoreV1().Pods(f.Namespace).Get(t.Context(), f.Name, metav1.GetOptions{})
	if root.Labels[RootRevisionLabel] != "1" || pod.Labels[PodRevisionLabel] != "2" {
		t.Fatal("revision patches were not applied")
	}
	if f.RootVersion != root.ResourceVersion || f.PodVersion != pod.ResourceVersion {
		t.Fatal("fixture versions differ from returned objects")
	}
	api.ClearActions()
	if err := deleteFixture(t.Context(), api, f); err != nil {
		t.Fatal(err)
	}
	actions := api.Actions()
	if len(actions) != 3 {
		t.Fatalf("expected three explicit deletes, got %d", len(actions))
	}
	for i, uid := range []types.UID{f.PodUID, f.ReplicaSetUID, f.RootUID} {
		options := actions[i].(clienttesting.DeleteAction).GetDeleteOptions()
		if options.GracePeriodSeconds == nil || *options.GracePeriodSeconds != 0 || options.Preconditions == nil || options.Preconditions.UID == nil || *options.Preconditions.UID != uid {
			t.Fatalf("unsafe delete options: %+v", options)
		}
	}
	for _, expected := range fixtureExpectations(f, true) {
		if !expected.Deleted || expected.Ref.ResourceVersion != "" || expected.Ref.UID == "" {
			t.Fatalf("incorrect deletion expectation: %+v", expected)
		}
	}
}

func TestValidateFixtures(t *testing.T) {
	f := Fixture{Namespace: "load", Name: "sample", RootUID: "root", ReplicaSetUID: "rs", PodUID: "pod", RootVersion: "10", PodVersion: "11"}
	for _, tc := range []struct {
		name      string
		mutate    func(*store.CapturedView)
		wantError bool
	}{
		{"valid", func(_ *store.CapturedView) {}, false},
		{"wrong root mapping", func(v *store.CapturedView) { v.Pods[0].Record.Workload.UID = "other" }, true},
		{"stale root revision", func(v *store.CapturedView) { v.Workloads[0].Record.Ref.ResourceVersion = "old" }, true},
		{"stale pod revision", func(v *store.CapturedView) { v.Pods[0].Input.ResourceVersion = "old" }, true},
		{"wrong label", func(v *store.CapturedView) { v.Pods[0].Record.Labels[PodMetricLabel] = "old" }, true},
		{"missing Pod", func(v *store.CapturedView) { v.Pods = nil }, true},
		{"wrong status", func(v *store.CapturedView) { v.Workloads[0].Record.Phases = []string{"Running"} }, true},
		{"retained error", func(v *store.CapturedView) { v.Workloads[0].StatusError = "failed" }, true},
		{"unhealthy source", func(v *store.CapturedView) {
			for k, h := range v.Sources {
				h.Synced = false
				v.Sources[k] = h
				break
			}
		}, true},
		{"missing sources", func(v *store.CapturedView) { v.Sources = nil }, true},
		{"wrong catalog", func(v *store.CapturedView) { v.Pods[0].Record.Karta = "other" }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			view := validFixtureView(f)
			tc.mutate(&view)
			err := validateFixtures(view, []Fixture{f}, "0", "0", false)
			if (err != nil) != tc.wantError {
				t.Fatalf("error=%v, want error=%t", err, tc.wantError)
			}
		})
	}
	view := validFixtureView(f)
	if err := validateFixtures(view, []Fixture{f}, "0", "0", true); err == nil {
		t.Fatal("deletion accepted retained records")
	}
	view.Pods, view.Workloads = nil, nil
	if err := validateFixtures(view, []Fixture{f}, "0", "0", true); err != nil {
		t.Fatal(err)
	}
}

func validFixtureView(f Fixture) store.CapturedView {
	refs := fixtureExpectations(f, false)
	return store.CapturedView{
		Workloads: []*store.WorkloadResult{{HasValue: true, Record: store.WorkloadRecord{UID: f.RootUID, Ref: refs[0].Ref, Karta: "apps-deployment-v1", KartaSource: "catalog", OwnerResolution: store.Resolved, HasStatus: true, Phases: []string{"Initializing"}, StatusLabels: map[string]string{RootMetricLabel: "0"}}}},
		Pods:      []*store.PodResult{{HasValue: true, Input: refs[1].Ref, Record: store.PodRecord{UID: f.PodUID, Name: f.Name, Namespace: f.Namespace, Workload: refs[0].Ref, Karta: "apps-deployment-v1", KartaSource: "catalog", OwnerResolution: store.Resolved, Component: "deployment", Labels: map[string]string{RootMetricLabel: "0", PodMetricLabel: "0"}}}},
		Sources: map[store.SourceKey]store.SourceHealth{
			{Resource: appsv1.SchemeGroupVersion.WithResource("deployments"), Namespace: f.Namespace}: {GVK: appsv1.SchemeGroupVersion.WithKind("Deployment"), Synced: true},
			{Resource: appsv1.SchemeGroupVersion.WithResource("replicasets"), Namespace: f.Namespace}: {GVK: appsv1.SchemeGroupVersion.WithKind("ReplicaSet"), Synced: true},
			{Resource: corev1.SchemeGroupVersion.WithResource("pods"), Namespace: f.Namespace}:        {GVK: corev1.SchemeGroupVersion.WithKind("Pod"), Synced: true},
		},
	}
}
