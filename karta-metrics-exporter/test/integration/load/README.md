<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 NVIDIA Corporation -->

# Exporter load tests

This opt-in harness runs a real API server and etcd through envtest, a parent
load generator, and a separate exporter process. It uses the real reflector,
queues, ownership discovery, catalog definition, CEL evaluation, and publication
Store. It adds no dependencies and is not part of the default load executed by
`make check` or CI. Its unit tests remain ordinary Go tests.

Each fixture is a Deployment root, an owned ReplicaSet, and an owned Pod.
The driver creates all three API objects explicitly. Envtest does not run
workload controllers, kubelets, containers, or Kubernetes garbage collection.
Deployment status is set explicitly for the catalog's Initializing phase, and
lifecycle deletion removes all three objects explicitly. The driver always
starts its own isolated control plane; it does not target a production cluster.

The default `--scenario=workload` uses `apps-deployment-v1` from the catalog,
with cluster definitions disabled. CEL rules read revision labels from both the root and Pod. Exporter
client limits remain the production defaults of 20 QPS and burst 40. The generator
has a separate client and arrival scheduler. `--workers=0` keeps the production
worker default; a positive value sets both worker pools through their normal
fallback configuration. No HTTP metrics server or periodic metrics scrape runs.

## Run locally

Run from the repository root. The Make target builds the binary and selects the
same pinned envtest assets as the exporter integration suite.

```sh
make test-exporter-load

# Small smoke run before a larger experiment.
make test-exporter-load LOAD_ARGS='--roots=10 --rates=10'

# Increasing populations and offered mutation rates.
make test-exporter-load LOAD_ARGS='--roots=1000,10000 --rates=100,500'

# 100,000 roots mean 300,000 fixture API objects, plus the namespace.
make test-exporter-load LOAD_ARGS='--roots=100000 --rates=1000 --phase-timeout=20m --max-duration=60m'
```

The default is 1,000 roots at 100 offered updates per second, with fixture setup
capped at 200 chains per second and 32 concurrent generator operations. The
normal phase budget is five minutes and the total run budget is 30 minutes.
Before each load phase, `--quiet-period=2s` requires two seconds without Store
publication writes. The wait is bounded by the phase budget.
Envtest startup and teardown have separate bounded process timeouts; the run
budget is not a hard wall-clock termination deadline during those calls.
Preloading 100,000 chains at 200/s requires at least 500 seconds, so the larger
example explicitly raises the budgets. It can still hit generator, API server,
exporter, time, or memory limits. It is an experiment, not a capacity guarantee.

Start with a small population, inspect the artifacts, and increase one
dimension at a time. The default combined RSS
stop budget is 12 GiB (`--max-memory-mib=12288`). Sampling is periodic, so this is
a soft stop budget, not an OS-enforced memory limit. Move stable scenarios to a
known GitHub Actions runner later and calibrate there independently; local
throughput thresholds do not transfer automatically. Larger root values are
accepted, but no result at or beyond 100,000 roots is implied by these examples.

## Live Karta definition update

`--scenario=karta-update` measures the fanout from one live definition update to
all resident workload publications. The default workload scenario is unchanged.
This initial experiment does not mutate workloads concurrently with the Karta
update and does not run the workload update/delete/create waves.

Run from the repository root so the default `--crds=charts/karta/crds` resolves.
Envtest installs that directory's Karta CRD in its isolated control plane. The
harness creates a cluster definition named `load-deployment`, derived from the
Deployment catalog definition. The child exporter enables only that cluster
definition and disables catalog loading. No existing cluster is used.

```sh
# Small definition-update experiment.
make test-exporter-load LOAD_ARGS='--scenario=karta-update --roots=10'

# Separate population trials through 100,000 roots, each with one Karta update.
make test-exporter-load LOAD_ARGS='--scenario=karta-update --roots=10000,50000,100000 --setup-rate=1000 --parallel=128 --quiet-period=2s --phase-timeout=20m --max-duration=90m'
```

`--setup-rate` and `--parallel` govern fixture preload. `--rates` retains its
normal default but is not used to pace or repeat definition updates in this
scenario. These finite trials do not establish sustained churn performance or
maximum capacity. See the [implementation comparison](../LOAD_TESTS.md) for local
evidence and limitations.

For each population, the driver creates Deployment -> ReplicaSet -> Pod chains,
validates the cold Store view, waits for publication quiet, and arms the recorder.
It then makes one Karta API update: the definition's root component becomes
`deployment-revised`, the ReplicaSet child component's definition `OwnerRef`
points to that renamed component, and the root's Initializing matchers become
Running matchers. Workload objects, their UIDs, and their resource versions remain
unchanged. The experiment tests definition propagation rather than object churn.

Primary timing starts at the definition inventory reflector's live Add/Update
observer, after name filtering but before Karta decoding and the inventory lock.
Each root or Pod sample ends after its matching valid real Store write commits.
The matching root must expose Running status under the cluster definition; the
matching Pod must resolve to the expected root with the revised component. All
samples share the definition-receipt start timestamp inside the exporter process.
The observed Karta identity and opaque resource version must exactly match the
UID/resource version returned by the update API. Initial LIST replacement does
not start this live-update measurement.

Root and Pod p50/p90/p99 distributions are reported separately under
`trial.definition_update.definition_to_store.roots` and `.pods` (the same trial
objects appear in `summary.json` under `trials`). The measurement also records
`definition`, `observed`, `expected`, `completed`, `missing`, and `overflow`.
Expected completion is two publications per fixture chain. `--max-samples` must
be at least twice the largest root population; missing completions, overflow or
failed semantic validation invalidate the result.

`trial.definition_update.mutation_to_validated_store_seconds` is the separate
wall time from starting the API update through complete semantic validation. It
includes API work, IPC and polling, unlike the per-publication timing. The report
also records API header latency, mutation time, previous/updated definition
origins, and publication-quiescence duration/write counts. The quiet period is
still a heuristic, not proof that every queue is empty.

The existing phase/run timeouts, process CPU/RSS sampling and combined memory
budget apply. Each trial writes `karta-update-progress.jsonl` and
`karta-update-samples.json`, plus `karta-update.heap` when the child remains
available for collection. These files sit beside the usual control-plane and
exporter logs and resource time series. Final distributions are summaries of
matched publications; partial distributions are not a successful fanout result.

## Three concurrent live Karta updates

`--scenario=karta-concurrent` creates three cluster Kartas for Deployment,
StatefulSet and DaemonSet roots. `--roots` is the total population, split across
those three cohorts, with one Pod per root and an extra ReplicaSet per Deployment.
At least three roots are required. Three update goroutines start behind a common
barrier; the report retains each API request's start/end timestamps so actual
overlap can be assessed. This scenario does not mutate workloads concurrently.

```sh
make test-exporter-load LOAD_ARGS='--scenario=karta-concurrent --roots=30 --workers=16'
```

Each Karta renames its root component and changes Initializing to Running.
Each cohort binds root/Pod samples to its own exact live definition UID/version
receipt. Final semantic validation and API reads require unchanged workload
UIDs and resource versions, including the Deployment ReplicaSets. Root and Pod
distributions remain separate per cohort. The sample budget must cover at least
four times the total roots plus three definition receipts. Missing publications,
overflow, API errors or failed validation invalidate the trial.

## Select an individual workload phase

`--workload-phase` accepts `all` (default), `root-update`, `pod-update`, `delete`
or `live-create`. Individual selection allows later scenarios to run even when
an earlier requested-rate wave would fail. Every selection still preloads the
fixtures and validates cold startup. `live-create` first deletes the fleet in a
separately tagged preparation phase, then measures recreation with new UIDs; a
failed preparation prevents the measured phase.

```sh
make test-exporter-load LOAD_ARGS='--roots=1000 --rates=100 --workload-phase=pod-update --workers=16 --parallel=512'
```

Worker overrides set both controller worker pools. `--parallel` limits concurrent
generator operations, not exporter workers or API QPS. See
[diagnostic modes](DIAGNOSTICS.md) for arrival traces and API-only isolation.

## Workload measurement contract

Primary latency begins at the reflector Store Add/Update/Delete callback entry,
after watch decoding and the existing watch proxy's object preparation, but
before callback object preparation and the controller state lock. It ends just
after the matching valid real Store write returns. Both timestamps are inside
the exporter process. The observer wraps the real Store; it does not replace the
controller pipeline with a fake implementation.

Updates match UID and opaque resource version. Pod publications must be valid,
error-free, and resolved to a workload; root publications must be valid and free
of publication/status errors. Delete responses do not expose the tombstone's
resource version, so deletion expectations match an observed deletion for the
UID to a subsequent Store removal. Resource versions are never sorted as numbers.

Reported p50/p90/p99 use nearest rank over completed expected samples. Raw
samples retain their identities and nanosecond durations. Missing expected
samples or recorder/API sample overflow invalidate a phase; partial percentiles
must not be interpreted as a successful capacity result. The default retention
budget is one million events/API samples per phase (`--max-samples`).

Full semantic convergence is checked separately: exact root/Pod counts, UID and
version identity, ownership, catalog selection, component, Initializing phase,
CEL revision labels, and source health. Root-update latency measures the root
publication; convergence additionally checks propagation to the Pod's workload
reference and root-derived labels. That fanout is not included in the root
latency percentile itself.

Cold startup is wall time from launching the exporter until all preloaded
fixtures pass semantic validation. Initial/replacement LIST snapshots use
Replace and do not generate individual observer samples. Cold-start time is
therefore a separate measurement from callback-to-publication latency.

A correct Store view at startup does not prove background work has finished.
Before each mutation or lifecycle phase, the driver waits for the configured
quiet period without any Store publication writes. The counter includes all
publication writes, not just writes matching measured expected events. This is
a warmup heuristic, not proof that every queue is empty or that the system has
reached steady state. Reports include `pre_load_quiescence_seconds` and
`pre_load_publication_writes` so the wait and observed activity remain visible.

Recorder `pending` is observed callbacks without a matched completion, not the
controller's queue depth. It can include superseded versions that were coalesced
before evaluation. Required final versions must complete; superseded callbacks
remain visible in observation statistics without becoming a convergence gate.

## Load shape and interpretation

In the default workload scenario, each population starts with preload and cold
startup, then runs one root-update wave and one Pod-update wave for each offered rate. Deletion and live creation
follow. Each update wave touches each fixture once. Mutation waves use open-loop
scheduled arrivals with bounded concurrency; missed arrivals identify generator
limits instead of being silently delayed into a lower offered rate. The default
arrival lateness budget is 50 ms (`--max-start-lag`); catch-up within that budget
may produce short bursts, still bounded by concurrency. Larger delays are
counted as missed. Each phase reports its maximum actual start lag. Setup and
lifecycle phases are rate-capped and may wait for generator capacity.

Compare offered rate, started/completed operations, missed arrivals, API errors,
API response-header latency, achieved completion rate, and drain/convergence
time together. Drain time includes expectation IPC, full semantic validation,
and polling at one-second intervals; it is not a direct queue-drain timer.
The primary percentile timestamps are recorded directly at the reflector and
publication endpoints and do not inherit that polling interval.
Achieved rate describes generator operations, not API request
count or exporter publication rate. A fixture create/delete uses several API
requests. API timings stop at response headers and exclude body reads.

A passing finite wave does not establish sustained churn capacity or a steady
state. Compare identical fixture counts, rates, worker settings, tool versions,
and host resources. Distinguish generator/API/infrastructure limits from
exporter convergence limits before drawing conclusions.

## Artifacts and resource accounting

Results default to a new `.artifacts/exporter-load/<timestamp>` directory:

- `summary.json`: scenario, build/platform metadata, asset versions, and trials.
- `roots-<count>/trial.json`: startup, phase outcomes, and peak RSS summaries.
- `resources.jsonl` within each trial: sampled process RSS and cumulative CPU
  seconds for driver, exporter, API server, and etcd. CPU utilization requires
  differences between successive CPU and wall-time samples.
- Per-phase `*-progress.jsonl` and `*-samples.json`: observation progress and
  completed callback latency samples. Progress uses recorder statistics only;
  it does not call exporter readiness, capture the Store, or run full validation.
  The progress request watchdog is 30 seconds and probe errors are preserved.
- Exporter/control-plane logs and heap profiles at successful completion or
  recoverable phase limits. Heap collection does not force a Go GC.

Resource peaks include observer, validation, and artifact-generation overhead.
The exporter process excludes the driver and control plane, while the combined
RSS budget includes all four roles. RSS is converted to bytes on Mac and Linux;
process-sampling failures are errors, not zero resource usage. Heap profiles
are distinct from RSS and cumulative CPU accounting; this harness does not
currently collect CPU pprof profiles.

Artifacts are private local files. Kubeconfig credentials live in a separate
temporary directory and are removed during teardown. Stop, EOF, cancellation,
and request timeout paths shut down or terminate and reap the exporter; the
driver also stops its envtest control plane.

## Initial local evidence

The [implementation comparison](../LOAD_TESTS.md) summarizes the original and
selected candidate runs. All measured 50,000-root requested-rate waves missed
generator arrivals, even when admitted updates converged. Separately selected
finite lifecycle and live Karta scenarios converged at that population. Neither
partial latency distributions nor finite convergence isolate the exporter's
breaking point.
