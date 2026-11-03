<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 NVIDIA Corporation -->

# Exporter load harness comparison

Three implementations are retained for review. The original harness is the
measurement reference; candidates A and C explore simpler execution paths. No
replacement has been selected.

| Approach | Entry point | Scenarios | Resource accounting |
| --- | --- | --- | --- |
| [Original](load/README.md) | Separate driver and exporter processes | Full-fleet root/Pod waves, delete/recreate, one or three live Karta updates | Driver, exporter, API server and etcd CPU/RSS samples |
| [A: Go test](loadtest/README.md) | Opt-in `TestExporterLoad` | Root/Pod waves, one Karta update, full-fleet delete/recreate | None; exporter and driver share a process |
| [C: small CLI](loadcli/README.md) | In-process executable | Root/Pod waves, one Karta update, one-chain delete/recreate | None; exporter and driver share a process |

All three use an isolated envtest API server and etcd, the real controller and
definition inventory, ownership resolution, CEL evaluation and publication Store.
Fixtures are explicit API objects. Envtest has no workload controllers, kubelets
or garbage collector; no containers run. These tests stop at the Store and do
not exercise metrics scraping or HTTP/OTel serving.

Run one real load experiment at a time. From the repository root:

```sh
# Original: configurable workers, populations, rates and diagnostic scenarios.
make test-exporter-load LOAD_ARGS='--roots=20 --rates=10'

# A: fixed four exporter workers, at most 16 concurrent generator writes.
KARTA_LOAD_ROOTS=20 KARTA_LOAD_RATE=10 make test-exporter-load-gotest

# C: fixed four exporter workers, configurable generator concurrency.
make test-exporter-load-cli LOAD_CLI_ARGS='-roots=20 -rate=10'
```

Make provisions the pinned envtest assets. Real load runs are opt-in; ordinary
package regression tests run without starting these experiments. Generated
reports stay local and are not source fixtures. The JSON formats differ across
implementations; compare the reported identities, phase contracts and settings
rather than assuming fields or failure counts are interchangeable.

## What the measurements mean

Primary latency starts at the live reflector callback and ends after the
matching valid real Store write. The Karta scenario starts at the live definition
inventory receipt and measures fanout to root and Pod publications separately.
Exact UID and opaque resource-version identities bind API responses to receipts
and writes, including callbacks that arrive before the API response. Initial
LIST replacement is not a live receipt. Percentiles use nearest rank over
completed required samples; raw samples preserve identities and durations.

Missing arrivals or samples, invalid measured publications, API errors and
observation overflow fail a measured wave. Final semantic validation checks the
Store independently. A partial percentile distribution is not a passing result.
Publication quiet is a warmup heuristic; it does not prove queues are empty.
All observers and validation add overhead. A finite wave is not sustained churn
or an isolated exporter capacity measurement.

A and C use client QPS/burst 1000/2000 and four exporter workers. The original
keeps production exporter client defaults (20/40), allows worker overrides and
uses a separate generator. Arrival lateness budgets also differ: the original
uses 50ms by default, while A/C reject arrivals one interval late. Therefore the
three approaches are not controlled performance comparisons without aligning
these settings and lifecycle coverage first.

## Local evidence before PR assembly

These observations came from a 12-logical-CPU, 36-GiB Mac using Go 1.27.3 and
Kubernetes envtest 1.31.0. They are individual development runs, not CI thresholds.

| Implementation | Population and offered workload rate | Observed result |
| --- | --- | --- |
| A | 20 roots, 10 updates/s | Passed 80/80 required samples and full-fleet lifecycle validation |
| A | 1,000 roots, 100 updates/s | Failed on two invalid Pod publications after completing the root wave; later phases did not run |
| C | 20 roots, 10 updates/s | Passed 80/80 required samples and one-chain lifecycle validation |
| C | 1,000 roots, 100 updates/s | One final run passed 4,000/4,000 required samples and one-chain lifecycle validation; earlier runs failed |
| Original | 50,000 roots, 500 root updates/s, 2/4/8/16 workers | Every measured root wave missed generator arrivals; none passed the zero-miss contract |
| Original | 50,000 resident roots, 16 workers, one live Karta update | All 100,000 required root/Pod publications completed and validated |
| Original | 50,000 total roots across three Kartas, 16 workers | All 100,000 required publications completed, with overlapping Karta update requests and unchanged workload identities/versions |

Increasing the original generator concurrency from 128 to 512 reduced misses
in the two-worker root experiment, but did not eliminate them. Its actual CLI
default is 32; the 128 comparison was an experiment setting. With 16 workers,
root and Pod waves still missed arrivals, while separately selected finite
lifecycle and Karta scenarios converged. Shared-host CPU/memory pressure and
source-health transitions were also observed. The exporter breaking point has
not been isolated, and a successful 100,000-root requested-rate load point has
not been established. Candidate C's one successful 1,000-root run does not
resolve those limits or invalidate candidate A's failure.

## PR assembly smoke checks

Fresh small trials passed the original workload/lifecycle scenario (10 roots),
single Karta update (10 roots), and three concurrent Kartas (six total roots,
16 workers). Candidate A passed 80/80 required samples and full-fleet lifecycle
validation at 20 roots and 10 updates/s. Candidate C failed at the same population
and rate: the Pod wave recorded 20 invalid `api_unavailable` publications despite
completing its required latency samples. Karta fanout and lifecycle did not run.
The candidate remains available for comparison; its earlier pass is not a
repeatability guarantee.
