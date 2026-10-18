# Performance suite v2

The shared scenarios live in `test/suites/performancev2/perf/scenarios`. This directory is the core KWOK provider's plugin for them: `suite_test.go` plus `kwok_provider.go`. Other KWOK-based providers write the same two files against `test/suites/performancev2/perf`, and don't copy any tests.

## Scenarios and vectors

| Scenario | Vector (size) | Phases | Failure modes covered |
|---|---|---|---|
| `pods` | pods (default 1000) | `scaleOut`, `consolidation` | latency, CPU and memory |
| `nodepools` | NodePools (default 1000; N-1 carry a tenant taint the pods don't tolerate) | `scheduling` (100 pods onto the open pool), `startup` | startup, latency, memory |

Each scenario's `Help` is the alarm text printed when a Stable gate breaches.

## Levels

Levels are per provider:

```go
Levels: perf.Levels{
	"pods/scaleOut":        {perf.HeapMB.Stable(150), perf.CPUCores.Experimental(0.7)},
	"nodepools/startup":    {perf.StartupSeconds.Stable(60)},
},
```

- **Stable:** a breach fails the spec.
- **Experimental:** a breach is written to the report JSON, then flagged as a GitHub warning and a Ginkgo report entry.
- **Off:** a scenario with no listed phases doesn't run.

Promote a gate to Stable when all of these hold:
1. 20 or more main runs.
2. CV of at most 20%.
3. A bound of at least 1.1x the observed max.
4. A patch in `test/suites/performancev2/perf/regressions/` breaches it.

## Writing a scenario

```go
var Daemonsets = perf.Scenario{
	Name: "daemonsets", Vector: "daemonsets", DefaultSize: 50,
	Help: `ALARM daemonsets: ...what breached, likely code, how to reproduce...`,
	Run: func(r *perf.Run) {
		app := r.Deployment("app", 500, "1", "2Gi")
		p := r.StartPhase("scaleOut")
		r.Create(r.NodeClass, r.NodePool, app)
		p.AwaitScaleOut(500, 15*time.Minute)
	},
}
```

How a scenario's run works:
- `Run.Create` labels and tracks every object, and cleanup deletes them all.
- A spec fails before measuring if an earlier spec left labeled objects behind.
- `StartPhase` fails if the provider has no levels entry for the phase. An empty entry means measure only.
- `Phase` scrapes the leader's `/metrics` every 2 seconds through the API server proxy. It fails if the controller restarted mid-phase.

## Running

```sh
OUTPUT_DIR=$(mktemp -d) TEST_SUITE=Performancev2 FOCUS=nodepools make e2etests
KARPENTER_PERF_SIZES='{"nodepools":10000}' ...      # sweep a vector
KARPENTER_PERF_THRESHOLDS='{"pods/scaleOut":{"heap_mb":200}}' ...   # per-runner bounds
```

## Review standard

Every PR that adds or changes a perf test needs to show all of these:

1. It catches a patch from `test/suites/performancev2/perf/regressions/`, and doesn't breach on a negative control.
2. 5 base runs with median, max and CV for each gated metric.
3. Where each bound comes from, and the regression size it lets through (bound / median - 1).
4. Wall-clock time isn't Stable-gated for consolidation or drift.
5. It isn't duplicated in an open PR or an existing scenario.
6. It fails loudly on a controller restart.
7. What it costs, in minutes per run.
