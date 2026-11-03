// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 NVIDIA Corporation

package main

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestScheduledWriteRejectsWorkerDelayedAfterAdmission(t *testing.T) {
	due := time.Now()
	release := make(chan struct{})
	done := make(chan bool, 1)
	go func() {
		<-release
		_, started, _ := scheduledWrite(context.Background(), due, time.Millisecond, func() error { t.Error("late worker issued API request"); return nil })
		done <- started
	}()
	// Model an admitted worker that cannot run until its arrival is already stale.
	timer := time.NewTimer(time.Until(due.Add(2 * time.Millisecond)))
	<-timer.C
	close(release)
	if <-done {
		t.Fatal("late worker counted as started")
	}
}

func TestScheduledWriteCancellationLagAndAPIErrors(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	apiError := errors.New("API update failed")
	cases := []struct {
		name            string
		ctx             context.Context
		due             time.Time
		wantStart       bool
		apiErr, wantErr error
	}{
		{"cancelled", cancelled, time.Now(), false, nil, context.Canceled},
		{"late", context.Background(), time.Now().Add(-2 * time.Hour), false, nil, nil},
		{"successful", context.Background(), time.Now().Add(-time.Second), true, nil, nil},
		{"API error", context.Background(), time.Now().Add(-time.Second), true, apiError, apiError},
	}
	started, missed, completed, errored := 0, 0, 0, 0
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			before := time.Now()
			lag, didStart, err := scheduledWrite(tc.ctx, tc.due, time.Hour, func() error { calls++; return tc.apiErr })
			if didStart != tc.wantStart || !errors.Is(err, tc.wantErr) || lag < before.Sub(tc.due) || lag > time.Since(tc.due) {
				t.Fatalf("lag=%s started=%v error=%v", lag, didStart, err)
			}
			if didStart {
				started++
				if err != nil {
					errored++
				} else {
					completed++
				}
				if calls != 1 {
					t.Fatal("started without a request")
				}
			} else {
				missed++
				if calls != 0 {
					t.Fatal("missed arrival issued request")
				}
			}
		})
	}
	if started+missed != len(cases) || started != completed+errored {
		t.Fatal("operation accounting identities differ")
	}
}
