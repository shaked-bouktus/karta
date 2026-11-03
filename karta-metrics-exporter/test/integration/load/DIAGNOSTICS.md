<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 NVIDIA Corporation -->

# Local limit diagnostics

The original harness includes opt-in modes to distinguish generator/control-plane
limits from exporter convergence. They do not change production defaults.

Run the built binary from the repository root, or use `make test-exporter-load`
with the corresponding `LOAD_ARGS`. Keep the original
run's setup rate, parallelism, workers, scheduling budget and resource budgets
when comparing results.

- `--diagnostic-root-only` exits after the first root update wave, after normal
  convergence and evidence collection. Normal child and control-plane cleanup
  still run. Pod and lifecycle waves are excluded.
- `--diagnostic-api-only` preloads identical fixtures and executes the same root
  PATCH generator and API meter without starting the exporter. Store validation
  and reflector-to-Store latency are absent. This measures the generator and
  control plane only; it is not an exporter capacity result.

Both modes require the workload scenario and exactly one offered rate. The
sample budget must cover every root. Each workload phase writes a separate
`*-arrivals.json` containing one record per offered index. Reasons are `started`,
`scheduler_late`, `slots_full`, `worker_late`, and `canceled`. Started operations
have a separate API error boolean. Dispatch timestamps record scheduler
admission time; worker start and end delimit the operation task. Zero timestamps
mean the corresponding stage was never reached. Inflight values are concurrent
slot occupancy snapshots, not a global atomic execution timeline. The original
arrival deadlines, worker bound and lateness cutoff are retained.

Exporter runs also write `*-source-health.json`. This contains bounded cumulative
Store source-health calls, including cold start, with timestamps at writer entry.
Repeated health values are retained. Store methods still delegate normally.
Overflow is reported explicitly. This trace records notifications, not the
underlying transport cause. Concurrent calls may appear out of timestamp order.

Trace allocation, timestamping and health recording add diagnostic overhead.
Compare instrumented exporter and API-only runs with identical settings and do
not interpret a single difference as proof of causality. These modes do not collect CPU, mutex or block profiles.
