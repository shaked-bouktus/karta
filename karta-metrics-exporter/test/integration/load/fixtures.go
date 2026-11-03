// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 NVIDIA Corporation

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"

	"github.com/dsx-ai-factory/workload-map/karta-metrics-exporter/pkg/store"
)

type Fixture struct {
	RootKind                       string
	ReplicaSetVersion              string
	Namespace, Name                string
	RootUID, ReplicaSetUID, PodUID types.UID
	RootVersion, PodVersion        string
}

type DefinitionExpectation struct {
	Name      string `json:"name"`
	Source    string `json:"source"`
	Component string `json:"component"`
	Phase     string `json:"phase"`
}

const (
	RootRevisionLabel = "load.example.com/root-revision"
	PodRevisionLabel  = "load.example.com/pod-revision"
	RootMetricLabel   = "load_root_revision"
	PodMetricLabel    = "load_pod_revision"
)

func createFixture(ctx context.Context, api kubernetes.Interface, namespace, name string) (Fixture, error) {
	f := Fixture{Namespace: namespace, Name: name}
	template := corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": name, PodRevisionLabel: "0"}},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: "ghcr.io/example/load-fixture:v1"}}},
	}
	root, err := api.AppsV1().Deployments(namespace).Create(ctx, &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: map[string]string{RootRevisionLabel: "0"}},
		Spec:       appsv1.DeploymentSpec{Replicas: ptr.To(int32(1)), Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}}, Template: template},
	}, metav1.CreateOptions{})
	if err != nil {
		return f, fmt.Errorf("create Deployment %s: %w", name, err)
	}
	f.RootUID, f.RootVersion = root.UID, root.ResourceVersion
	// Envtest has no Deployment controller to populate the catalog's status conditions.
	root.Status.ObservedGeneration = root.Generation
	root.Status.Conditions = []appsv1.DeploymentCondition{{Type: appsv1.DeploymentProgressing, Status: corev1.ConditionTrue, Reason: "NewReplicaSetCreated"}}
	root, err = api.AppsV1().Deployments(namespace).UpdateStatus(ctx, root, metav1.UpdateOptions{})
	if err != nil {
		return f, fmt.Errorf("set Deployment status %s: %w", name, err)
	}
	f.RootVersion = root.ResourceVersion
	rs, err := api.AppsV1().ReplicaSets(namespace).Create(ctx, &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(root, appsv1.SchemeGroupVersion.WithKind("Deployment"))}},
		Spec:       appsv1.ReplicaSetSpec{Replicas: ptr.To(int32(1)), Selector: root.Spec.Selector, Template: template},
	}, metav1.CreateOptions{})
	if err != nil {
		return f, fmt.Errorf("create ReplicaSet %s: %w", name, err)
	}
	f.ReplicaSetUID, f.ReplicaSetVersion = rs.UID, rs.ResourceVersion
	pod, err := api.CoreV1().Pods(namespace).Create(ctx, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: template.Labels, OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(rs, appsv1.SchemeGroupVersion.WithKind("ReplicaSet"))}},
		Spec:       template.Spec,
	}, metav1.CreateOptions{})
	if err != nil {
		return f, fmt.Errorf("create Pod %s: %w", name, err)
	}
	f.PodUID, f.PodVersion = pod.UID, pod.ResourceVersion
	return f, nil
}

func updateRoot(ctx context.Context, api kubernetes.Interface, f *Fixture, revision string) error {
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{"labels": map[string]string{RootRevisionLabel: revision}}})
	if err != nil {
		return err
	}
	root, err := api.AppsV1().Deployments(f.Namespace).Patch(ctx, f.Name, types.MergePatchType, patch, metav1.PatchOptions{})
	if err != nil {
		return fmt.Errorf("update Deployment %s: %w", f.Name, err)
	}
	f.RootVersion = root.ResourceVersion
	return nil
}

func updatePod(ctx context.Context, api kubernetes.Interface, f *Fixture, revision string) error {
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{"labels": map[string]string{PodRevisionLabel: revision}}})
	if err != nil {
		return err
	}
	pod, err := api.CoreV1().Pods(f.Namespace).Patch(ctx, f.Name, types.MergePatchType, patch, metav1.PatchOptions{})
	if err != nil {
		return fmt.Errorf("update Pod %s: %w", f.Name, err)
	}
	f.PodVersion = pod.ResourceVersion
	return nil
}

func deleteFixture(ctx context.Context, api kubernetes.Interface, f Fixture) error {
	// Delete every object explicitly because envtest has no garbage collector.
	var errs []error
	for _, item := range []struct {
		uid    types.UID
		kind   string
		remove func(context.Context, string, metav1.DeleteOptions) error
	}{
		{f.PodUID, "Pod", api.CoreV1().Pods(f.Namespace).Delete},
		{f.ReplicaSetUID, "ReplicaSet", api.AppsV1().ReplicaSets(f.Namespace).Delete},
		{f.RootUID, "Deployment", api.AppsV1().Deployments(f.Namespace).Delete},
	} {
		if item.uid == "" {
			continue
		}
		err := item.remove(ctx, f.Name, metav1.DeleteOptions{GracePeriodSeconds: ptr.To(int64(0)), Preconditions: &metav1.Preconditions{UID: ptr.To(item.uid)}})
		if err != nil && !apierrors.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("delete %s %s: %w", item.kind, f.Name, err))
		}
	}
	return errors.Join(errs...)
}

func fixtureExpectations(f Fixture, deleted bool) []Expectation {
	kind := f.RootKind
	if kind == "" {
		kind = "Deployment"
	}
	rootVersion, podVersion := f.RootVersion, f.PodVersion
	// Delete events can carry a newer resource version than the last update response.
	if deleted {
		rootVersion, podVersion = "", ""
	}
	return []Expectation{
		{Ref: store.ObjectRef{GVK: appsv1.SchemeGroupVersion.WithKind(kind), Namespace: f.Namespace, Name: f.Name, UID: f.RootUID, ResourceVersion: rootVersion}, Deleted: deleted},
		{Ref: store.ObjectRef{GVK: corev1.SchemeGroupVersion.WithKind("Pod"), Namespace: f.Namespace, Name: f.Name, UID: f.PodUID, ResourceVersion: podVersion}, Deleted: deleted},
	}
}

func validateFixtures(view store.CapturedView, fixtures []Fixture, rootRevision, podRevision string, deleted bool) error {
	return validateFixturesWithDefinition(view, fixtures, rootRevision, podRevision, deleted, DefinitionExpectation{Name: "apps-deployment-v1", Source: "catalog", Component: "deployment", Phase: "Initializing"})
}

func validateFixturesWithDefinition(view store.CapturedView, fixtures []Fixture, rootRevision, podRevision string, deleted bool, definition DefinitionExpectation) error {
	if len(view.Failures) != 0 {
		return fmt.Errorf("discovery failures: %d", len(view.Failures))
	}
	healthy := make(map[schema.GroupVersionKind]bool)
	for key, health := range view.Sources {
		if !health.Synced || health.Error != "" {
			return fmt.Errorf("source %v unhealthy: synced=%t error=%q", key, health.Synced, health.Error)
		}
		healthy[health.GVK] = true
	}
	kinds := []schema.GroupVersionKind{corev1.SchemeGroupVersion.WithKind("Pod")}
	for _, f := range fixtures {
		kind := f.RootKind
		if kind == "" {
			kind = "Deployment"
		}
		gvk := appsv1.SchemeGroupVersion.WithKind(kind)
		if !slices.Contains(kinds, gvk) {
			kinds = append(kinds, gvk)
		}
		if kind == "Deployment" && !slices.Contains(kinds, appsv1.SchemeGroupVersion.WithKind("ReplicaSet")) {
			kinds = append(kinds, appsv1.SchemeGroupVersion.WithKind("ReplicaSet"))
		}
	}
	for _, gvk := range kinds {
		if !healthy[gvk] {
			return fmt.Errorf("missing healthy source %s", gvk)
		}
	}
	expected := len(fixtures)
	if deleted {
		expected = 0
	}
	if len(view.Workloads) != expected || len(view.Pods) != expected {
		return fmt.Errorf("publication counts: workloads=%d pods=%d expected=%d each", len(view.Workloads), len(view.Pods), expected)
	}
	if deleted {
		return nil
	}
	roots := make(map[types.UID]*store.WorkloadResult, len(view.Workloads))
	pods := make(map[types.UID]*store.PodResult, len(view.Pods))
	for _, root := range view.Workloads {
		if root == nil {
			return errors.New("nil workload record")
		}
		if roots[root.Record.UID] != nil {
			return fmt.Errorf("duplicate root UID %s", root.Record.UID)
		}
		roots[root.Record.UID] = root
	}
	for _, pod := range view.Pods {
		if pod == nil {
			return errors.New("nil Pod record")
		}
		if pods[pod.Record.UID] != nil {
			return fmt.Errorf("duplicate Pod UID %s", pod.Record.UID)
		}
		pods[pod.Record.UID] = pod
	}
	for _, f := range fixtures {
		refs := fixtureExpectations(f, false)
		root, pod := roots[f.RootUID], pods[f.PodUID]
		if root == nil || pod == nil {
			return fmt.Errorf("fixture %s missing root or Pod", f.Name)
		}
		if !root.HasValue || root.Error != "" || root.StatusError != "" || root.Record.Ref != refs[0].Ref || root.Record.OwnerResolution != store.Resolved || root.Record.Karta != definition.Name || root.Record.KartaSource != definition.Source || !root.Record.HasStatus || len(root.Record.Phases) != 1 || !slices.Contains(root.Record.Phases, definition.Phase) || root.Record.StatusLabels[RootMetricLabel] != rootRevision {
			return fmt.Errorf("fixture %s has incorrect workload publication", f.Name)
		}
		if !pod.HasValue || pod.Error != "" || pod.Input != refs[1].Ref || pod.Record.Workload != refs[0].Ref || pod.Record.Namespace != f.Namespace || pod.Record.Name != f.Name || pod.Record.OwnerResolution != store.Resolved || pod.Record.Karta != definition.Name || pod.Record.KartaSource != definition.Source || pod.Record.Component != definition.Component || pod.Record.Labels[RootMetricLabel] != rootRevision || pod.Record.Labels[PodMetricLabel] != podRevision {
			return fmt.Errorf("fixture %s has incorrect Pod publication", f.Name)
		}
	}
	return nil
}
