// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 NVIDIA Corporation

package main

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestRunLoadPacedDropsWhenWorkersFull(t *testing.T) {
	var current, maxRunning atomic.Int32
	var mu sync.Mutex
	seen := make(map[int]bool)
	result := runLoad(t.Context(), 10, 100, 2, true, 10*time.Millisecond, func(_ context.Context, index int) error {
		running := current.Add(1)
		defer current.Add(-1)
		for old := maxRunning.Load(); running > old; old = maxRunning.Load() {
			if maxRunning.CompareAndSwap(old, running) {
				break
			}
		}
		mu.Lock()
		if seen[index] {
			t.Errorf("index %d ran twice", index)
		}
		seen[index] = true
		mu.Unlock()
		time.Sleep(300 * time.Millisecond)
		return nil
	})
	if result.Started < 1 || result.Started > 2 || result.Missed < 8 || maxRunning.Load() > 2 {
		t.Fatalf("bounded arrivals violated: %+v, concurrency %d", result, maxRunning.Load())
	}
	if result.Scheduled != 10 || result.Completed != result.Started || result.Errors != 0 || result.Missed+result.Started != 10 || current.Load() != 0 {
		t.Fatalf("bad count/drain accounting: %+v", result)
	}
	if result.DurationSeconds < .3 {
		t.Fatal("duration excludes operation drain")
	}
	for i, success := range result.Success {
		if success != seen[i] {
			t.Fatalf("success flag mismatch at %d", i)
		}
	}
}

func TestRunLoadSetupDrainsAndCapsRate(t *testing.T) {
	var mu sync.Mutex
	var starts []time.Time
	var active, maxRunning atomic.Int32
	operationError := errors.New("expected operation failure")
	result := runLoad(t.Context(), 6, 50, 2, false, 0, func(_ context.Context, index int) error {
		running := active.Add(1)
		defer active.Add(-1)
		for old := maxRunning.Load(); running > old; old = maxRunning.Load() {
			if maxRunning.CompareAndSwap(old, running) {
				break
			}
		}
		mu.Lock()
		starts = append(starts, time.Now())
		mu.Unlock()
		time.Sleep(35 * time.Millisecond)
		if index == 2 {
			return operationError
		}
		return nil
	})
	if result.Started != 6 || result.Completed != 5 || result.Errors != 1 || result.Missed != 0 || result.FirstError != operationError.Error() || maxRunning.Load() > 2 || active.Load() != 0 {
		t.Fatalf("setup result: %+v", result)
	}
	if result.Success[2] {
		t.Fatal("failed operation marked successful")
	}
	// Five arrival gaps at 20 ms plus the final operation must be included.
	if result.DurationSeconds < .13 || starts[len(starts)-1].Sub(starts[0]) < 95*time.Millisecond {
		t.Fatalf("setup exceeded configured rate: %+v", result)
	}
}

func TestRunLoadCancellationDrainsStartedWork(t *testing.T) {
	for _, paced := range []bool{false, true} {
		t.Run(map[bool]string{false: "setup", true: "paced"}[paced], func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			started := make(chan struct{})
			release := make(chan struct{})
			result := make(chan LoadResult, 1)
			go func() {
				result <- runLoad(ctx, 100, 10, 1, paced, 100*time.Millisecond, func(ctx context.Context, _ int) error {
					close(started)
					<-ctx.Done()
					<-release
					return ctx.Err()
				})
			}()
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("operation did not start")
			}
			cancel()
			select {
			case <-result:
				t.Fatal("returned before started work drained")
			default:
			}
			close(release)
			select {
			case got := <-result:
				if got.Started != 1 || got.Completed != 0 || got.Errors != 1 || got.Missed != 99 || got.FirstError != context.Canceled.Error() {
					t.Fatalf("canceled accounting: %+v", got)
				}
			case <-time.After(time.Second):
				t.Fatal("cancellation did not drain")
			}
		})
	}
}

func TestRunLoadAlreadyCanceledAndEmpty(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	called := false
	operation := func(context.Context, int) error { called = true; return nil }
	got := runLoad(ctx, 5, 100, 2, true, 10*time.Millisecond, operation)
	if called || got.Missed != 5 || got.Started != 0 || len(got.Success) != 5 {
		t.Fatalf("already canceled load: %+v", got)
	}
	if got := runLoad(t.Context(), 0, 0, 0, true, 0, operation); got.Scheduled != 0 || got.Completed != 0 {
		t.Fatalf("empty load: %+v", got)
	}
	if got := runLoad(t.Context(), 5, 0, 1, true, 10*time.Millisecond, operation); got.Missed != 5 || got.FirstError == "" {
		t.Fatalf("invalid load: %+v", got)
	}
}

func TestAPIMeterDelegatesRealTransportThroughHeaders(t *testing.T) {
	releaseBody := make(chan struct{})
	clientConnection, serverConnection := net.Pipe()
	defer clientConnection.Close()
	defer serverConnection.Close()
	serverDone := make(chan error, 1)
	go func() {
		request, err := http.ReadRequest(bufio.NewReader(serverConnection))
		if err != nil {
			serverDone <- err
			return
		}
		if request.Method != http.MethodPost {
			t.Errorf("method changed: %s", request.Method)
		}
		_, err = io.Copy(io.Discard, request.Body)
		_ = request.Body.Close()
		if err != nil {
			serverDone <- err
			return
		}
		_, err = io.WriteString(serverConnection, "HTTP/1.1 201 Created\r\nContent-Length: 7\r\n\r\n")
		if err == nil {
			<-releaseBody
			_, err = io.WriteString(serverConnection, "payload")
		}
		serverDone <- err
	}()
	transport := &http.Transport{DialContext: func(context.Context, string, string) (net.Conn, error) {
		return clientConnection, nil
	}}
	defer transport.CloseIdleConnections()
	meter := NewAPIMeter(10)
	client := &http.Client{Transport: meter.Wrap(transport), Timeout: time.Second}
	response, err := client.Post("http://example.test", "text/plain", strings.NewReader("request"))
	if err != nil {
		close(releaseBody)
		t.Fatal(err)
	}
	stats := meter.Snapshot()
	close(releaseBody)
	body, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if serverErr := <-serverDone; serverErr != nil {
		t.Fatal(serverErr)
	}
	if err != nil || string(body) != "payload" {
		t.Fatalf("response body was not preserved: %q %v", body, err)
	}
	if stats.Requests != 1 || stats.Succeeded != 1 || stats.Failed != 0 || stats.Latency == nil || stats.Latency.Count != 1 || stats.Overflow != 0 {
		t.Fatalf("headers not recorded before body arrived: %+v", stats)
	}
}

func TestAPIMeterErrorsOverflowAndReset(t *testing.T) {
	meter := NewAPIMeter(2)
	transportError := errors.New("transport failed")
	responses := []int{200, 299, 300, 500, 0}
	for _, status := range responses {
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://example.test", nil)
		if err != nil {
			t.Fatal(err)
		}
		underlying := roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r != request {
				t.Fatal("request replaced")
			}
			if status == 0 {
				return nil, transportError
			}
			return &http.Response{StatusCode: status, Body: http.NoBody}, nil
		})
		response, err := meter.Wrap(underlying).RoundTrip(request)
		if status == 0 {
			if !errors.Is(err, transportError) || response != nil {
				t.Fatal("transport error not forwarded")
			}
		} else if err != nil || response.StatusCode != status {
			t.Fatal("response not forwarded")
		}
	}
	got := meter.Snapshot()
	if got.Requests != 5 || got.Succeeded != 2 || got.Failed != 3 || got.Overflow != 3 || got.Latency.Count != 2 || len(meter.samples) != 2 {
		t.Fatalf("unexpected meter accounting: %+v", got)
	}
	got.Latency.P50MS = -1
	if meter.Snapshot().Latency.P50MS < 0 {
		t.Fatal("snapshot aliases meter")
	}
	meter.Begin()
	if got := meter.Snapshot(); got.Requests != 0 || got.Latency != nil || got.Overflow != 0 {
		t.Fatalf("meter not reset: %+v", got)
	}
}

func TestAPIMeterConcurrentRequests(t *testing.T) {
	meter := NewAPIMeter(10)
	wrapped := meter.Wrap(roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 204, Body: http.NoBody}, nil
	}))
	var workers sync.WaitGroup
	for range 100 {
		workers.Go(func() {
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPut, "http://example.test", nil)
			if err != nil {
				t.Error(err)
				return
			}
			_, err = wrapped.RoundTrip(req)
			if err != nil {
				t.Error(err)
			}
			meter.Snapshot()
		})
	}
	workers.Wait()
	got := meter.Snapshot()
	if got.Requests != 100 || got.Succeeded != 100 || got.Failed != 0 || got.Overflow != 90 || got.Latency.Count != 10 {
		t.Fatalf("concurrent meter accounting: %+v", got)
	}
}

func TestStartLagBudgetAllowsMultipleArrivalIntervals(t *testing.T) {
	// Synthetic timestamps test jitter tolerance without relying on OS scheduling delays.
	scheduled := time.Unix(100, 0)
	for _, tc := range []struct {
		name   string
		lag    time.Duration
		budget time.Duration
		late   bool
	}{
		{"strict interval rejects 20ms", 20 * time.Millisecond, 10 * time.Millisecond, true},
		{"50ms budget accepts 20ms", 20 * time.Millisecond, 50 * time.Millisecond, false},
		{"budget boundary accepted", 50 * time.Millisecond, 50 * time.Millisecond, false},
		{"budget exceeded rejected", 50*time.Millisecond + time.Nanosecond, 50 * time.Millisecond, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := exceedsStartLag(scheduled, scheduled.Add(tc.lag), tc.budget); got != tc.late {
				t.Fatalf("lag %s with budget %s: late=%v", tc.lag, tc.budget, got)
			}
		})
	}
}

func TestRunLoadRequiresPositivePacedLagBudget(t *testing.T) {
	for _, budget := range []time.Duration{0, -time.Millisecond} {
		result := runLoad(t.Context(), 5, 100, 2, true, budget, func(context.Context, int) error {
			t.Fatal("invalid paced budget started an operation")
			return nil
		})
		if result.Started != 0 || result.Missed != 5 || result.FirstError == "" {
			t.Fatalf("invalid paced budget accepted: %+v", result)
		}
	}
}
