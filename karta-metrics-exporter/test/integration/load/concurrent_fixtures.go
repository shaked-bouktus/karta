// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 NVIDIA Corporation

package main

import (
	"context"
	"fmt"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	inventory "github.com/dsx-ai-factory/workload-map/karta-metrics-exporter/pkg/kartadefinitioninventory"
	"github.com/dsx-ai-factory/workload-map/karta-metrics-exporter/pkg/store"
	v1alpha1 "github.com/dsx-ai-factory/workload-map/karta/sdk/pkg/api/runai/v1alpha1"
)

var concurrentKinds = []string{"Deployment", "StatefulSet", "DaemonSet"}

func concurrentNames() []string {
	return []string{"load-deployment", "load-statefulset", "load-daemonset"}
}

func newConcurrentDefinitions() ([]*v1alpha1.Karta, error) {
	var definitions []*v1alpha1.Karta
	for i, kind := range concurrentKinds {
		definition := &v1alpha1.Karta{TypeMeta: metav1.TypeMeta{APIVersion: "run.ai/v1alpha1", Kind: "Karta"}, ObjectMeta: metav1.ObjectMeta{Name: concurrentNames()[i]}, Spec: v1alpha1.KartaSpec{StructureDefinition: v1alpha1.StructureDefinition{RootComponent: v1alpha1.ComponentDefinition{
			Name: strings.ToLower(kind), Kind: &v1alpha1.GroupVersionKind{Group: "apps", Version: "v1", Kind: kind},
			SpecDefinition:   &v1alpha1.SpecDefinition{PodTemplateSpecPath: ptr.To(".spec.template")},
			StatusDefinition: &v1alpha1.StatusDefinition{StatusMappings: v1alpha1.StatusMappings{Initializing: []v1alpha1.StatusMatcher{{ByExpression: &v1alpha1.ExpressionMatcher{Expression: ".status.observedGeneration == .metadata.generation", ExpectedResult: "true"}}}}},
		}}}}
		if err := v1alpha1.NewKartaValidator(definition).Validate(); err != nil {
			return nil, err
		}
		definitions = append(definitions, definition)
	}
	return definitions, nil
}

func createConcurrentFixture(ctx context.Context, api kubernetes.Interface, namespace, name, kind string) (Fixture, error) {
	if kind == "Deployment" {
		f, err := createFixture(ctx, api, namespace, name)
		f.RootKind = kind
		return f, err
	}
	f := Fixture{Namespace: namespace, Name: name, RootKind: kind}
	meta := metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: map[string]string{RootRevisionLabel: "0"}}
	template := corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": name, PodRevisionLabel: "0"}}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: "ghcr.io/example/load-fixture:v1"}}}}
	selector := &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}}
	var owner metav1.Object
	switch kind {
	case "StatefulSet":
		root, err := api.AppsV1().StatefulSets(namespace).Create(ctx, &appsv1.StatefulSet{ObjectMeta: meta, Spec: appsv1.StatefulSetSpec{Replicas: ptr.To(int32(1)), ServiceName: "load", Selector: selector, Template: template}}, metav1.CreateOptions{})
		if err != nil {
			return f, err
		}
		root.Status.ObservedGeneration = root.Generation
		root, err = api.AppsV1().StatefulSets(namespace).UpdateStatus(ctx, root, metav1.UpdateOptions{})
		if err != nil {
			return f, err
		}
		owner = root
	case "DaemonSet":
		root, err := api.AppsV1().DaemonSets(namespace).Create(ctx, &appsv1.DaemonSet{ObjectMeta: meta, Spec: appsv1.DaemonSetSpec{Selector: selector, Template: template}}, metav1.CreateOptions{})
		if err != nil {
			return f, err
		}
		root.Status.ObservedGeneration = root.Generation
		root, err = api.AppsV1().DaemonSets(namespace).UpdateStatus(ctx, root, metav1.UpdateOptions{})
		if err != nil {
			return f, err
		}
		owner = root
	default:
		return f, fmt.Errorf("unsupported concurrent fixture kind %q", kind)
	}
	f.RootUID, f.RootVersion = owner.GetUID(), owner.GetResourceVersion()
	pod, err := api.CoreV1().Pods(namespace).Create(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: template.Labels, OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(owner, appsv1.SchemeGroupVersion.WithKind(kind))}}, Spec: template.Spec}, metav1.CreateOptions{})
	if err != nil {
		return f, err
	}
	f.PodUID, f.PodVersion = pod.UID, pod.ResourceVersion
	return f, nil
}

func concurrentCohorts(definitions []*v1alpha1.Karta, fixtures []Fixture, revised bool) []ConcurrentCohort {
	cohorts := make([]ConcurrentCohort, len(definitions))
	for i, d := range definitions {
		cohorts[i] = ConcurrentCohort{Previous: inventory.Origin{Source: inventory.SourceCluster, Name: d.Name, UID: d.UID, ResourceVersion: d.ResourceVersion}, Expected: DefinitionExpectation{Name: d.Name, Source: "cluster", Component: d.Spec.StructureDefinition.RootComponent.Name, Phase: "Initializing"}}
		if revised {
			cohorts[i].Expected.Component += "-revised"
			cohorts[i].Expected.Phase = "Running"
		}
		for _, f := range fixtures {
			if f.RootKind == concurrentKinds[i] {
				cohorts[i].Fixtures = append(cohorts[i].Fixtures, f)
			}
		}
	}
	return cohorts
}

func validateConcurrent(view store.CapturedView, cohorts []ConcurrentCohort) error {
	roots, pods := make(map[types.UID]*store.WorkloadResult), make(map[types.UID]*store.PodResult)
	expected := 0
	for _, c := range cohorts {
		expected += len(c.Fixtures)
	}
	if len(view.Workloads) != expected || len(view.Pods) != expected {
		return fmt.Errorf("concurrent publication count mismatch: %d roots, %d Pods, expected %d each", len(view.Workloads), len(view.Pods), expected)
	}
	for _, r := range view.Workloads {
		if r == nil || roots[r.Record.UID] != nil {
			return fmt.Errorf("duplicate or nil concurrent root")
		}
		roots[r.Record.UID] = r
	}
	for _, r := range view.Pods {
		if r == nil || pods[r.Record.UID] != nil {
			return fmt.Errorf("duplicate or nil concurrent Pod")
		}
		pods[r.Record.UID] = r
	}
	for _, c := range cohorts {
		subset := store.CapturedView{Sources: view.Sources, Failures: view.Failures}
		for _, f := range c.Fixtures {
			subset.Workloads = append(subset.Workloads, roots[f.RootUID])
			subset.Pods = append(subset.Pods, pods[f.PodUID])
		}
		if err := validateFixturesWithDefinition(subset, c.Fixtures, "0", "0", false, c.Expected); err != nil {
			return fmt.Errorf("%s: %w", c.Expected.Name, err)
		}
	}
	return nil
}

func verifyConcurrentIdentities(ctx context.Context, api client.Client, fixtures []Fixture) error {
	observed := make(map[types.UID]string, 3*len(fixtures))
	for _, list := range []client.ObjectList{&appsv1.DeploymentList{}, &appsv1.StatefulSetList{}, &appsv1.DaemonSetList{}, &appsv1.ReplicaSetList{}, &corev1.PodList{}} {
		if err := api.List(ctx, list, client.InNamespace("karta-load")); err != nil {
			return err
		}
		switch l := list.(type) {
		case *appsv1.DeploymentList:
			for _, x := range l.Items {
				observed[x.UID] = x.ResourceVersion
			}
		case *appsv1.StatefulSetList:
			for _, x := range l.Items {
				observed[x.UID] = x.ResourceVersion
			}
		case *appsv1.DaemonSetList:
			for _, x := range l.Items {
				observed[x.UID] = x.ResourceVersion
			}
		case *appsv1.ReplicaSetList:
			for _, x := range l.Items {
				observed[x.UID] = x.ResourceVersion
			}
		case *corev1.PodList:
			for _, x := range l.Items {
				observed[x.UID] = x.ResourceVersion
			}
		}
	}
	expected := 0
	for _, f := range fixtures {
		for uid, version := range map[types.UID]string{f.RootUID: f.RootVersion, f.PodUID: f.PodVersion, f.ReplicaSetUID: f.ReplicaSetVersion} {
			if uid == "" {
				continue
			}
			expected++
			if observed[uid] != version {
				return fmt.Errorf("workload identity changed for %s/%s UID %s", f.RootKind, f.Name, uid)
			}
		}
	}
	if len(observed) != expected {
		return fmt.Errorf("workload object count changed: %d != %d", len(observed), expected)
	}
	return nil
}
