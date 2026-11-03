<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 NVIDIA Corporation -->

# Candidate A: sequential Go integration test

`TestExporterLoad` runs one readable sequence: isolated envtest startup, explicit
Deployment -> ReplicaSet -> Pod fixtures, root updates, Pod updates, one live
Karta update, full-fleet deletion/recreation and semantic validation. The real
exporter and definition inventory run inside the test process. There is no
metrics server or scrape load.

From the repository root:

```sh
KARTA_LOAD_ROOTS=20 KARTA_LOAD_RATE=10 make test-exporter-load-gotest
KARTA_LOAD_ROOTS=1000 KARTA_LOAD_RATE=100 make test-exporter-load-gotest
```

For direct invocation, provision envtest assets with `make envtest`, then run
from the integration module:

```sh
export KUBEBUILDER_ASSETS="$(bin/setup-envtest-release-0.23 use 1.31.0 --bin-dir bin -p path)"
cd karta-metrics-exporter/test/integration
KARTA_LOAD=1 KARTA_LOAD_ROOTS=20 KARTA_LOAD_RATE=10 \
  KARTA_LOAD_OUTPUT=/tmp/karta-load-a.json \
  go test ./loadtest -run '^TestExporterLoad$' -count=1 -timeout=17m -v
```

The asset export runs from the repository root before `cd`. Without
`KARTA_LOAD=1`, the real experiment is skipped. Roots and offered updates/s
default to 20 and 10. `KARTA_LOAD_OUTPUT` defaults to `load-result.json` in the
package working directory. Cleanup writes evidence on normal test failures;
external process termination can prevent it.

Exporter workers are fixed at four, both clients use QPS/burst 1000/2000 and
generator concurrency is fixed at 16. Each finite wave offers one update per
root or Pod on an absolute schedule. Busy slots or arrivals one interval late
fail the wave instead of postponing traffic. API errors, invalid publications,
missing samples, bounded evidence overflow and timeouts also fail. UID and opaque
resource-version correlation retains receipts/writes that precede API replies.
Percentiles are nearest rank; validation and drain wall times are separate.

The Karta update renames the root component and its child owner link and changes
the Running matcher to Completed. Root and Pod fanout use one exact live
inventory receipt; workload identities and versions must remain unchanged.
Lifecycle validation deletes every fixture chain, verifies publication absence,
recreates same-name objects with new UIDs and checks the full Store.

The overall context is 15 minutes, with bounded startup, API requests, convergence
and cleanup. A 250ms publication-quiet gate is a heuristic, not proof that queues
are empty. Observer locking and retained snapshots add overhead. No CPU or RSS
is measured, and exporter/generator/observer/validation share one process.

The final 20-root development run passed 80/80 samples and lifecycle validation.
The 1,000-root run at 100 updates/s failed on two invalid Pod publications after
the root wave; later scenarios did not run. See the
[comparison and limitations](../LOAD_TESTS.md). Ordinary regression tests run with
`go test -race ./loadtest` from the integration module.
