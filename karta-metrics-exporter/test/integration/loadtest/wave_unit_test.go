// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 NVIDIA Corporation

package loadtest

import (
	"context"
	"errors"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/dsx-ai-factory/workload-map/karta-metrics-exporter/pkg/store"
)

type updateClient struct {
	client.Client
	calls int
	err   error
}

func (c *updateClient) Update(_ context.Context, object client.Object, _ ...client.UpdateOption) error {
	c.calls++
	object.SetResourceVersion("response-version")
	return c.err
}

func TestDelayedWriterMissesArrivalWithoutAPICall(t *testing.T) {
	api := &updateClient{}
	r := &run{api: api}
	scheduled := time.Now()
	release := make(chan struct{})
	done := make(chan struct{})
	var lag time.Duration
	var started bool
	go func() {
		defer close(done)
		<-release
		lag, started, _ = r.updateObject(context.Background(), 0, false, scheduled, time.Millisecond)
	}()
	time.Sleep(10 * time.Millisecond)
	close(release)
	<-done
	if started || api.calls != 0 || lag <= time.Millisecond {
		t.Fatalf("late writer: started=%v API calls=%d lag=%v", started, api.calls, lag)
	}
}

func TestFailedUpdatePreservesCommittedFixture(t *testing.T) {
	for _, pods := range []bool{false, true} {
		api := &updateClient{err: errors.New("API failed")}
		meta := metav1.ObjectMeta{ResourceVersion: "committed", Labels: map[string]string{"load.example/revision": "0"}}
		root := &appsv1.Deployment{ObjectMeta: *meta.DeepCopy()}
		pod := &corev1.Pod{ObjectMeta: *meta.DeepCopy()}
		r := &run{api: api, chains: []chain{{Root: root, Pod: pod}}}
		_, started, err := r.updateObject(context.Background(), 0, pods, time.Now(), time.Second)
		if !started || err == nil || api.calls != 1 {
			t.Fatalf("started=%v error=%v calls=%d", started, err, api.calls)
		}
		for _, object := range []client.Object{r.chains[0].Root, r.chains[0].Pod} {
			if object.GetResourceVersion() != "committed" || object.GetLabels()["load.example/revision"] != "0" {
				t.Fatalf("failed API request changed fixture: %+v", object)
			}
		}
		api.err = nil
		_, _, err = r.updateObject(context.Background(), 0, pods, time.Now(), time.Second)
		if err != nil {
			t.Fatal(err)
		}
		var updated client.Object = r.chains[0].Root
		if pods {
			updated = r.chains[0].Pod
		}
		if updated.GetResourceVersion() != "response-version" || updated.GetLabels()["load.example/revision"] != "1" {
			t.Fatal("successful API response was not retained")
		}
	}
}

func TestMissedAndErroredWaveDoesNotWaitForImpossibleReceipts(t *testing.T) {
	o := newObservation(100)
	o.SetSourceHealth(store.SourceKey{}, store.SourceHealth{Synced: true})
	o.begin()
	r := &run{ctx: context.Background(), observer: o}
	p := &phaseReport{Offered: 2, Started: 1, Missed: 1, Errored: 1}
	start := time.Now()
	err := r.drainPhase(p, nil, true, false, nil)
	if err == nil || p.Roots.Expected != 2 || p.Roots.Completed != 0 || p.Roots.Missing != 2 || p.Timeouts != 0 || time.Since(start) > time.Second {
		t.Fatalf("drain error=%v report=%+v elapsed=%v", err, p, time.Since(start))
	}
}

func TestInvalidExamplesAreBoundedWithoutDroppingCounts(t *testing.T) {
	o := newObservation(100)
	o.begin()
	for range 15 {
		o.UpsertPod(&store.PodResult{Input: store.ObjectRef{UID: "pod", ResourceVersion: "bad-rv"}, Record: store.PodRecord{UID: "pod"}, Error: "invalid", HasValue: true})
	}
	if o.invalid != 15 || len(o.invalidExamples) != 10 {
		t.Fatalf("invalid=%d examples=%d", o.invalid, len(o.invalidExamples))
	}
	e := o.invalidExamples[0]
	if e.UID != "pod" || e.RV != "bad-rv" || e.Kind != "Pod" || e.Reason != "invalid" || !e.HasValue || !e.SourcesValid {
		t.Fatalf("invalid evidence: %+v", e)
	}
	o.begin()
	if len(o.invalidExamples) != 0 {
		t.Fatal("examples leaked between phases")
	}
}
