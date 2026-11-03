<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 NVIDIA Corporation -->

# Candidate C: small in-process CLI

This executable runs isolated envtest startup, Deployment -> ReplicaSet -> Pod
fixture creation, root updates, Pod updates, one live Karta update and deletion/
recreation of one complete chain. It validates the full Store after the lifecycle
change. It uses the real controller and definition inventory without starting
metrics serving or scraping.

From the repository root:

```sh
make test-exporter-load-cli LOAD_CLI_ARGS='-roots=20 -rate=10 -output=/tmp/karta-load-c.json'
make test-exporter-load-cli LOAD_CLI_ARGS='-roots=1000 -rate=100'
```

For direct invocation, provision envtest assets with `make envtest`, then:

```sh
export KUBEBUILDER_ASSETS="$(bin/setup-envtest-release-0.23 use 1.31.0 --bin-dir bin -p path)"
cd karta-metrics-exporter/test/integration
go run ./loadcli -roots=20 -rate=10 -output=/tmp/karta-load-c.json
```

The asset export runs from the repository root before `cd`. CLI defaults are:

| Flag | Default |
| --- | --- |
| `-roots` | 20 |
| `-rate` | 10 updates/s per finite wave |
| `-concurrency` | 16 concurrent generator writes |
| `-quiet` | 250ms publication silence |
| `-phase-timeout` | 2m |
| `-timeout` | 10m, excluding bounded cleanup |
| `-crds` | `../../../charts/karta/crds`, relative to the integration module |
| `-output` | `load-result.json` |

Exporter workers are fixed at four and both clients use QPS/burst 1000/2000.
Each workload wave offers one update per chain. Busy slots and arrivals one
interval late count as misses rather than waiting for capacity. The CLI exits
nonzero on failed measured phases, missing samples, API errors, invalid measured
publications, evidence overflow, timeouts or cleanup errors. JSON retains error
counts and bounded invalid-publication examples. Startup/lifecycle invalid
publications are recorded separately and those transitions require final
healthy semantic validation.

A delegating Store observer records writes after commit. UID and opaque
resource-version keys correlate API responses, reflector receipts and valid
publications, including receipts/writes before the response. Each live Karta
update renames the root component/child owner link and changes Running to
Completed; all resident roots and Pods must converge without workload identity
or version changes. Percentiles use nearest rank, with raw sample timestamps and
nanosecond durations. Cold startup, drain and validation are separate wall times.

The quiet gate is a warmup heuristic, not proof that queues are empty. Observer
locking and validation add overhead. Exporter, generator, observer and validation
share a process; CPU and RSS are not measured for it or the envtest children.
The lifecycle covers one chain, unlike the original harness and candidate A's
full-fleet lifecycle. This difference matters when comparing results.

The final 20-root run passed 80/80 required samples. One final 1,000-root run
at 100 updates/s passed 4,000/4,000 required samples and one-chain lifecycle
validation; earlier runs failed on invalid publications. No maximum capacity is
claimed. See the [comparison](../LOAD_TESTS.md). Ordinary regression tests run
with `go test -race ./loadcli` from the integration module.

The PR assembly smoke at 20 roots and 10 updates/s failed in the Pod wave with
20 invalid `api_unavailable` publications. Karta fanout and lifecycle did not run.
The earlier passing run does not establish repeatability.
