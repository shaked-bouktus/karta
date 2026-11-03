// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 NVIDIA Corporation

package main

import (
	"context"
	"math"
	"net/http"
	"sync"
	"time"
)

type LoadResult struct {
	Scheduled       int     `json:"scheduled"`
	Started         int     `json:"started"`
	Completed       int     `json:"completed"`
	Missed          int     `json:"missed"`
	Errors          int     `json:"errors"`
	DurationSeconds float64 `json:"duration_seconds"`
	MaxStartLagMS   float64 `json:"max_start_lag_ms"`
	FirstError      string  `json:"first_error,omitempty"`
	Success         []bool  `json:"-"`
}

// APIStats measures all HTTP methods through response headers, excluding body reads.
// Overflow invalidates latency statistics even though request counters remain complete.
type APIStats struct {
	Requests  int           `json:"requests"`
	Succeeded int           `json:"succeeded"`
	Failed    int           `json:"failed"`
	Overflow  int           `json:"overflow"`
	Latency   *Distribution `json:"header_latency"`
}

type APIMeter struct {
	mu         sync.Mutex
	maxSamples int
	stats      APIStats
	samples    []int64
}

type meteredTransport struct {
	meter *APIMeter
	next  http.RoundTripper
}

func runLoad(ctx context.Context, count int, rate float64, parallel int, paced bool, maxStartLag time.Duration, operation func(context.Context, int) error, traces ...[]Arrival) LoadResult {
	start := time.Now()
	result := LoadResult{Scheduled: max(0, count), Success: make([]bool, max(0, count))}
	if count <= 0 {
		result.DurationSeconds = time.Since(start).Seconds()
		return result
	}
	if rate <= 0 || math.IsNaN(rate) || math.IsInf(rate, 0) || float64(time.Second)/rate >= float64(math.MaxInt64) || parallel <= 0 || (paced && maxStartLag <= 0) {
		result.Missed = count
		result.FirstError = "load rate must yield a finite representable interval; parallelism and paced start-lag budget must be positive"
		result.DurationSeconds = time.Since(start).Seconds()
		return result
	}
	interval := max(time.Nanosecond, time.Duration(float64(time.Second)/rate))
	var arrivals []Arrival
	if len(traces) > 0 {
		arrivals = traces[0]
	}
	for i := range arrivals {
		arrivals[i] = Arrival{Index: i, Scheduled: start.Add(time.Duration(i) * interval), Reason: "canceled"}
	}
	slots := make(chan struct{}, min(parallel, count))
	var mu sync.Mutex
	var workers sync.WaitGroup
	deadline := start
schedule:
	for index := 0; index < count; index++ {
		if ctx.Err() != nil {
			break
		}
		if delay := time.Until(deadline); delay > 0 {
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				break schedule
			case <-timer.C:
			}
		}
		var arrival *Arrival
		if index < len(arrivals) {
			arrival = &arrivals[index]
		}
		dispatched := time.Now()
		if arrival != nil {
			arrival.Dispatch = dispatched
			arrival.StartLagNS = dispatched.Sub(deadline).Nanoseconds()
			arrival.Inflight = len(slots)
		}
		if paced {
			if exceedsStartLag(deadline, dispatched, maxStartLag) {
				if arrival != nil {
					arrival.Reason = "scheduler_late"
					arrival.End = dispatched
					arrival.StartLagNS = dispatched.Sub(deadline).Nanoseconds()
				}
				deadline = deadline.Add(interval)
				continue
			}
			select {
			case slots <- struct{}{}:
			default:
				if arrival != nil {
					arrival.Reason = "slots_full"
					arrival.End = time.Now()
					arrival.Inflight = len(slots)
				}
				deadline = deadline.Add(interval)
				continue
			}
		} else {
			select {
			case <-ctx.Done():
				break schedule
			case slots <- struct{}{}:
			}
		}
		scheduled := deadline
		workers.Go(func() {
			defer func() { <-slots }()
			began := time.Now()
			if arrival != nil {
				arrival.WorkerStart = began
				arrival.InflightAtStart = len(slots)
				arrival.StartLagNS = began.Sub(scheduled).Nanoseconds()
				defer func() { arrival.End = time.Now() }()
			}
			if reason := workerRejection(ctx, paced, scheduled, began, maxStartLag); reason != "" {
				if arrival != nil {
					arrival.Reason = reason
				}
				return
			}
			if arrival != nil {
				arrival.Reason = "started"
			}
			mu.Lock()
			result.Started++
			result.MaxStartLagMS = max(result.MaxStartLagMS, float64(max(0, began.Sub(scheduled)))/float64(time.Millisecond))
			mu.Unlock()
			err := operation(ctx, index)
			if arrival != nil {
				arrival.APIError = err != nil
			}
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				result.Errors++
				if result.FirstError == "" {
					result.FirstError = err.Error()
				}
			} else {
				result.Completed++
				result.Success[index] = true
			}
		})
		if paced {
			deadline = deadline.Add(interval)
		} else {
			// Setup may wait for capacity but never catches up with a burst of arrivals.
			deadline = time.Now().Add(interval)
		}
	}
	workers.Wait()
	for i := range arrivals {
		if arrivals[i].Reason == "canceled" && arrivals[i].End.IsZero() {
			arrivals[i].End = time.Now()
		}
	}
	result.Missed = result.Scheduled - result.Started
	result.DurationSeconds = time.Since(start).Seconds()
	return result
}

// The lateness budget is independent of the arrival interval, allowing bounded catch-up.
func exceedsStartLag(scheduled, started time.Time, budget time.Duration) bool {
	return started.Sub(scheduled) > budget
}

func NewAPIMeter(maxSamples int) *APIMeter {
	return &APIMeter{maxSamples: max(0, maxSamples)}
}

// Begin resets the meter between phases, after all prior requests have drained.
func (m *APIMeter) Begin() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stats = APIStats{}
	m.samples = nil
}

func (m *APIMeter) Snapshot() APIStats {
	m.mu.Lock()
	stats := m.stats
	samples := append([]int64(nil), m.samples...)
	m.mu.Unlock()
	stats.Latency = distribution(samples)
	return stats
}

func (m *APIMeter) Wrap(next http.RoundTripper) http.RoundTripper {
	if next == nil {
		next = http.DefaultTransport
	}
	return &meteredTransport{meter: m, next: next}
}

func (t *meteredTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	t.meter.mu.Lock()
	t.meter.stats.Requests++
	t.meter.mu.Unlock()
	start := time.Now()
	response, err := t.next.RoundTrip(request)
	elapsed := time.Since(start).Nanoseconds()
	t.meter.mu.Lock()
	defer t.meter.mu.Unlock()
	if err == nil && response != nil && response.StatusCode >= 200 && response.StatusCode < 300 {
		t.meter.stats.Succeeded++
	} else {
		t.meter.stats.Failed++
	}
	if len(t.meter.samples) < t.meter.maxSamples {
		t.meter.samples = append(t.meter.samples, elapsed)
	} else {
		t.meter.stats.Overflow++
	}
	return response, err
}
