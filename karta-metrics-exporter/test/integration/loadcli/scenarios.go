// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 NVIDIA Corporation

package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/dsx-ai-factory/workload-map/karta-metrics-exporter/pkg/store"
	v1alpha1 "github.com/dsx-ai-factory/workload-map/karta/sdk/pkg/api/runai/v1alpha1"
)

type chain struct {
	root *appsv1.Deployment
	rs   *appsv1.ReplicaSet
	pod  *corev1.Pod
}

var deployGVK = appsv1.SchemeGroupVersion.WithKind("Deployment")

func createChain(ctx context.Context, api client.Client, name string) (chain, error) {
	root := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "load-cli", Labels: map[string]string{"revision": "0"}}, Spec: appsv1.DeploymentSpec{Replicas: ptr.To(int32(1)), Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}}, Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": name}}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: "example.test/image:v1"}}}}}}
	c := chain{root: root}
	if err := api.Create(ctx, root); err != nil {
		return c, err
	}
	root.Status = appsv1.DeploymentStatus{Replicas: 1, ReadyReplicas: 1, AvailableReplicas: 1, UpdatedReplicas: 1, Conditions: []appsv1.DeploymentCondition{{Type: appsv1.DeploymentAvailable, Status: corev1.ConditionTrue}, {Type: appsv1.DeploymentProgressing, Status: corev1.ConditionTrue, Reason: "NewReplicaSetAvailable"}}}
	if err := api.Status().Update(ctx, root); err != nil {
		return c, err
	}
	c.rs = &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: root.Namespace, OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(root, deployGVK)}}, Spec: appsv1.ReplicaSetSpec{Replicas: ptr.To(int32(1)), Selector: root.Spec.Selector, Template: root.Spec.Template}}
	if err := api.Create(ctx, c.rs); err != nil {
		return c, err
	}
	c.pod = &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: root.Namespace, Labels: map[string]string{"revision": "0"}, OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(c.rs, appsv1.SchemeGroupVersion.WithKind("ReplicaSet"))}}, Spec: root.Spec.Template.Spec}
	if err := api.Create(ctx, c.pod); err != nil {
		return c, err
	}
	c.pod.Status.Phase = corev1.PodPending
	if err := api.Status().Update(ctx, c.pod); err != nil {
		return c, err
	}
	return c, nil
}
func deleteChain(ctx context.Context, api client.Client, c chain) error {
	for _, obj := range []client.Object{c.pod, c.rs, c.root} {
		if err := client.IgnoreNotFound(api.Delete(ctx, obj, client.GracePeriodSeconds(0))); err != nil {
			return err
		}
		if err := wait(ctx, func() error {
			err := api.Get(ctx, client.ObjectKeyFromObject(obj), obj)
			if apierrors.IsNotFound(err) {
				return nil
			}
			if err != nil {
				return err
			}
			return errors.New("object still exists")
		}); err != nil {
			return err
		}
	}
	return nil
}
func objectRef(obj client.Object, gvk schema.GroupVersionKind) store.ObjectRef {
	return store.ObjectRef{GVK: gvk, Namespace: obj.GetNamespace(), Name: obj.GetName(), UID: obj.GetUID(), ResourceVersion: obj.GetResourceVersion()}
}
func targetsFor(chains []chain, component, phase string) map[types.UID]target {
	targets := make(map[types.UID]target, len(chains)*2)
	for _, c := range chains {
		root := objectRef(c.root, deployGVK)
		pod := objectRef(c.pod, corev1.SchemeGroupVersion.WithKind("Pod"))
		t := target{Ref: root, Root: root, Definition: "load-cli", Component: component, Phase: phase, RootRevision: c.root.Labels["revision"], PodRevision: c.pod.Labels["revision"]}
		targets[root.UID] = t
		t.Ref = pod
		targets[pod.UID] = t
	}
	return targets
}
func validate(o *observer, chains []chain, component, phase string) error {
	v := o.Capture()
	if len(v.Pods) != len(chains) || len(v.Workloads) != len(chains) || len(v.Failures) != 0 {
		return fmt.Errorf("store counts roots=%d pods=%d failures=%d, want %d chains", len(v.Workloads), len(v.Pods), len(v.Failures), len(chains))
	}
	if len(v.Sources) == 0 {
		return errors.New("no source health")
	}
	for _, h := range v.Sources {
		if !h.Synced || (h.Error != "" && h.Error != store.ReasonNone) {
			return fmt.Errorf("unhealthy source: %+v", h)
		}
	}
	targets := targetsFor(chains, component, phase)
	for _, r := range v.Workloads {
		t, ok := targets[r.Record.UID]
		valid, _ := v.Valid(r.Sources, r.Error)
		if !ok || !valid || !matchRoot(r, t, true) {
			return fmt.Errorf("root semantics mismatch %s: %+v", r.Record.Ref.Name, r.Record)
		}
	}
	for _, r := range v.Pods {
		t, ok := targets[r.Record.UID]
		valid, _ := v.Valid(r.Sources, r.Error)
		if !ok || !valid || !matchPod(r, t, true) {
			return fmt.Errorf("pod semantics mismatch %s: %+v", r.Record.Name, r.Record)
		}
	}
	return nil
}
func workloadWave(ctx context.Context, o options, api client.Client, obs *observer, chains []chain, kind string) (p phaseReport, err error) {
	p.Name = kind + "-updates"
	p.Offered = len(chains)
	defer func() {
		if err != nil {
			p.Error = err.Error()
		}
	}()
	ctx, cancel := context.WithTimeout(ctx, o.phase)
	defer cancel()
	if err = quiet(ctx, obs, o.quiet); err != nil {
		return p, err
	}
	expected := targetsFor(chains, "deployment", "Running")
	for uid, t := range expected {
		if kind == "root" {
			t.RootRevision = "1"
		} else {
			t.PodRevision = "1"
		}
		if (kind == "root" && t.Ref.GVK.Kind == "Deployment") || (kind == "pod" && t.Ref.GVK.Kind == "Pod") {
			expected[uid] = t
		} else {
			delete(expected, uid)
		}
	}
	obs.arm(expected)
	defer func() {
		invalid, finishErr := obs.finish()
		p.InvalidPublications = invalid
		p.InvalidExamples = obs.invalidExamples()
		err = errors.Join(err, finishErr)
	}()
	start := time.Now()
	interval := time.Duration(float64(time.Second) / o.rate)
	slots := make(chan struct{}, o.concurrency)
	var wg sync.WaitGroup
	var mu sync.Mutex
	schedulerMissed := 0
	for i := range chains {
		due := start.Add(time.Duration(i) * interval)
		timer := time.NewTimer(time.Until(due))
		select {
		case <-ctx.Done():
			timer.Stop()
			schedulerMissed += len(chains) - i
			goto drained
		case <-timer.C:
		}
		if time.Since(due) >= interval {
			schedulerMissed++
			continue
		}
		select {
		case slots <- struct{}{}:
		default:
			schedulerMissed++
			continue
		}
		wg.Add(1)
		go func(i int, due time.Time) {
			defer wg.Done()
			defer func() { <-slots }()
			lag, started, e := scheduledWrite(ctx, due, interval, func() error {
				var updateErr error
				if kind == "root" {
					next := chains[i].root.DeepCopy()
					next.Labels["revision"] = "1"
					updateErr = api.Update(ctx, next)
					if updateErr == nil {
						chains[i].root = next
					}
				} else {
					next := chains[i].pod.DeepCopy()
					next.Labels["revision"] = "1"
					updateErr = api.Update(ctx, next)
					if updateErr == nil {
						chains[i].pod = next
					}
				}
				return updateErr
			})
			mu.Lock()
			defer mu.Unlock()
			if !started {
				p.Missed++
				err = errors.Join(err, e)
				return
			}
			p.Started++
			p.StartLagNS = append(p.StartLagNS, lag.Nanoseconds())
			if e != nil {
				p.Errored++
				err = errors.Join(err, e)
			} else {
				p.Completed++
			}
		}(i, due)
	}
drained:
	wg.Wait()
	p.Missed += schedulerMissed
	p.GeneratorNS = time.Since(start).Nanoseconds()
	p.AchievedPerSecond = float64(p.Completed) / time.Since(start).Seconds()
	exact := targetsFor(chains, "deployment", "Running")
	for uid, t := range exact {
		if (kind == "root" && t.Ref.GVK.Kind != "Deployment") || (kind == "pod" && t.Ref.GVK.Kind != "Pod") {
			delete(exact, uid)
		}
	}
	drainStart := time.Now()
	observationErr := wait(ctx, func() error { return obs.pending(exact, nil) })
	p.DrainNS = time.Since(drainStart).Nanoseconds()
	p.Roots, p.Pods, _ = obs.results(exact, nil)
	validationStart := time.Now()
	validationErr := wait(ctx, func() error { return validate(obs, chains, "deployment", "Running") })
	p.ValidationNS = time.Since(validationStart).Nanoseconds()
	err = errors.Join(err, observationErr, validationErr)
	if p.Missed+p.Errored > 0 {
		err = errors.Join(err, fmt.Errorf("offered load incomplete: missed=%d errored=%d", p.Missed, p.Errored))
	}
	return p, err
}
func definitionWave(ctx context.Context, o options, api client.Client, obs *observer, chains []chain, definition *v1alpha1.Karta) (p phaseReport, err error) {
	p.Name = "karta-fanout"
	p.Offered = 1
	defer func() {
		if err != nil {
			p.Error = err.Error()
		}
	}()
	ctx, cancel := context.WithTimeout(ctx, o.phase)
	defer cancel()
	if err = quiet(ctx, obs, o.quiet); err != nil {
		return p, err
	}
	targets := targetsFor(chains, "changed", "Completed")
	obs.arm(targets)
	defer func() {
		invalid, finishErr := obs.finish()
		p.InvalidPublications = invalid
		p.InvalidExamples = obs.invalidExamples()
		err = errors.Join(err, finishErr)
	}()
	definition.Spec.StructureDefinition.RootComponent.Name = "changed"
	definition.Spec.StructureDefinition.ChildComponents[0].OwnerRef = ptr.To("changed")
	mappings := &definition.Spec.StructureDefinition.RootComponent.StatusDefinition.StatusMappings
	mappings.Completed = mappings.Running
	mappings.Running = nil
	p.Started = 1
	if err = api.Update(ctx, definition); err != nil {
		p.Errored = 1
		return p, err
	}
	p.Completed = 1
	origin := key{definition.UID, definition.ResourceVersion}
	p.DefinitionUID = string(origin.UID)
	p.DefinitionRV = origin.RV
	drainStart := time.Now()
	err = wait(ctx, func() error { return obs.pending(targets, &origin) })
	p.DrainNS = time.Since(drainStart).Nanoseconds()
	p.Roots, p.Pods, _ = obs.results(targets, &origin)
	validationStart := time.Now()
	err = errors.Join(err, wait(ctx, func() error { return validate(obs, chains, "changed", "Completed") }))
	for _, c := range chains {
		for _, obj := range []client.Object{c.root, c.rs, c.pod} {
			actual := obj.DeepCopyObject().(client.Object)
			if e := api.Get(ctx, client.ObjectKeyFromObject(obj), actual); e != nil {
				err = errors.Join(err, e)
				continue
			}
			if actual.GetUID() != obj.GetUID() || actual.GetResourceVersion() != obj.GetResourceVersion() {
				err = errors.Join(err, fmt.Errorf("workload identity changed during fanout: %s", obj.GetName()))
			}
		}
	}
	p.ValidationNS = time.Since(validationStart).Nanoseconds()
	return p, err
}

// scheduledWrite rechecks the deadline where the worker actually starts.
func scheduledWrite(ctx context.Context, due time.Time, interval time.Duration, write func() error) (time.Duration, bool, error) {
	lag := time.Since(due)
	if err := ctx.Err(); err != nil {
		return lag, false, err
	}
	if lag >= interval {
		return lag, false, nil
	}
	return lag, true, write()
}
