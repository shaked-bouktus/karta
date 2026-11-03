// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 NVIDIA Corporation

package kartadefinitioninventory

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
)

func observationObject(t *testing.T, name, uid, version string) *unstructured.Unstructured {
	t.Helper()
	k := newKarta(name, deploymentGVK, fromCluster(uid, version))
	data, err := runtime.DefaultUnstructuredConverter.ToUnstructured(k)
	if err != nil {
		t.Fatal(err)
	}
	return &unstructured.Unstructured{Object: data}
}

func TestObserveDefinitionBeforeMutation(t *testing.T) {
	var inventory *Inventory
	var observed []Origin
	previousVersion := ""
	cfg := Config{Cluster: SourceConfig{Enabled: true, Include: []string{"target"}}}
	cfg.ObserveEvent = func(origin Origin) {
		if !inventory.mu.TryLock() {
			t.Fatal("callback must precede inventory locking")
		}
		entry, exists := inventory.cluster[origin.Name]
		inventory.mu.Unlock()
		if previousVersion == "" && exists {
			t.Fatal("Add mutated inventory before observation")
		}
		if previousVersion != "" && (!exists || entry.definition.ResourceVersion != previousVersion) {
			t.Fatal("Update mutated inventory before observation")
		}
		observed = append(observed, origin)
	}
	var err error
	inventory, err = New(newFakeClient(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	s := reflectorStore{inventory: inventory}
	if err := s.Add(observationObject(t, "target", "target-uid", "opaque-a")); err != nil {
		t.Fatal(err)
	}
	previousVersion = "opaque-a"
	if err := s.Update(observationObject(t, "target", "target-uid", "opaque-b")); err != nil {
		t.Fatal(err)
	}
	if len(observed) != 2 {
		t.Fatalf("observations=%d, want 2", len(observed))
	}
	for n, version := range []string{"opaque-a", "opaque-b"} {
		want := Origin{Source: SourceCluster, Name: "target", UID: types.UID("target-uid"), ResourceVersion: version}
		if observed[n] != want {
			t.Fatalf("observation=%+v, want %+v", observed[n], want)
		}
	}
	if inventory.cluster["target"].definition.ResourceVersion != "opaque-b" {
		t.Fatal("update was not applied")
	}
}

func TestObserveDefinitionFiltersAndLiveEventsOnly(t *testing.T) {
	calls := 0
	inventory, err := New(newFakeClient(), Config{
		Cluster:      SourceConfig{Enabled: true, Include: []string{"target", "excluded"}, Exclude: []string{"excluded"}},
		ObserveEvent: func(Origin) { calls++ },
	})
	if err != nil {
		t.Fatal(err)
	}
	s := reflectorStore{inventory: inventory}
	for _, object := range []*unstructured.Unstructured{
		observationObject(t, "excluded", "u", "v"),
		observationObject(t, "not-included", "u", "v"),
		observationObject(t, "target", "", "v"),
		observationObject(t, "target", "u", ""),
		observationObject(t, "", "u", "v"),
	} {
		if err := s.Add(object); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Add("invalid object"); err == nil {
		t.Fatal("invalid object must be rejected")
	}
	target := observationObject(t, "target", "u", "v")
	if err := s.Replace([]any{target}, "list-rv"); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(target); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatalf("filtered, incomplete metadata or snapshot/deletion triggered %d callbacks", calls)
	}
	if err := s.Add(target); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("live Add callbacks=%d, want 1", calls)
	}
}
