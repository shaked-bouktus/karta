// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 NVIDIA Corporation

package controller

import (
	"reflect"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/tools/cache"

	"github.com/dsx-ai-factory/workload-map/karta-metrics-exporter/pkg/store"
)

func TestReflectorObserverPrecedesStateLockAndPreservesInput(t *testing.T) {
	for _, operation := range []string{"add", "update", "delete"} {
		for _, inputKind := range []string{"unstructured", "metadata", "tombstone"} {
			t.Run(operation+"/"+inputKind, func(t *testing.T) {
				metadata := &metav1.PartialObjectMetadata{ObjectMeta: metav1.ObjectMeta{
					Namespace: "test", Name: "pod", UID: "pod-uid", ResourceVersion: "42",
					ManagedFields: []metav1.ManagedFieldsEntry{{Manager: "test"}},
				}}
				object := &unstructured.Unstructured{}
				object.SetNamespace(metadata.Namespace)
				object.SetName(metadata.Name)
				object.SetUID(metadata.UID)
				object.SetResourceVersion(metadata.ResourceVersion)
				object.SetManagedFields(metadata.ManagedFields)
				before := object.DeepCopy()
				var input any = object
				switch inputKind {
				case "metadata":
					input = metadata
				case "tombstone":
					input = cache.DeletedFinalStateUnknown{Key: "test/pod", Obj: object}
				}
				state := newControllerState(&objectCache{})
				observed := make(chan struct {
					ref     store.ObjectRef
					deleted bool
				}, 1)
				state.deps.ObserveEvent = func(ref store.ObjectRef, deleted bool) {
					observed <- struct {
						ref     store.ObjectRef
						deleted bool
					}{ref, deleted}
				}
				source := &reflectorStore{controller: state, watch: &resourceWatch{gvk: podGVK, full: true}}
				callback := source.Add
				switch operation {
				case "update":
					callback = source.Update
				case "delete":
					callback = source.Delete
				}
				state.stateMu.Lock()
				done := make(chan error, 1)
				go func() { done <- callback(input) }()
				select {
				case event := <-observed:
					want := store.ObjectRef{GVK: podGVK, Namespace: "test", Name: "pod", UID: "pod-uid", ResourceVersion: "42"}
					if event.ref != want || event.deleted != (operation == "delete") {
						t.Errorf("observation = %+v, want ref %+v and delete %v", event, want, operation == "delete")
					}
				case <-time.After(5 * time.Second):
					t.Error("observer blocked behind state lock")
				}
				state.stateMu.Unlock()
				select {
				case err := <-done:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("callback did not complete")
				}
				if !reflect.DeepEqual(object, before) || len(metadata.ManagedFields) != 1 {
					t.Fatal("callback mutated source input")
				}
			})
		}
	}
}

func TestReflectorObserverSkipsReplaceAndSupportsNil(t *testing.T) {
	state := newControllerState(&objectCache{})
	source := &reflectorStore{controller: state, watch: &resourceWatch{gvk: podGVK, full: true}}
	object := &unstructured.Unstructured{}
	object.SetName("pod")
	object.SetUID("pod-uid")
	for _, callback := range []func(any) error{source.Add, source.Update, source.Delete} {
		if err := callback(object); err != nil {
			t.Fatal(err)
		}
	}
	state.deps.ObserveEvent = func(store.ObjectRef, bool) { t.Error("unexpected observation for Replace") }
	if err := source.Replace([]any{object}, "42"); err != nil {
		t.Fatal(err)
	}
}
