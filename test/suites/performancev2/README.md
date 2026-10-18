# Performance suite v2

A self-contained, plugin-style perf suite. Shared scenarios live in `perf/scenarios`, the library in `perf`, and copied dependencies in `env`. This directory's `suite_test.go` and `kwok_provider.go` are the core KWOK provider's plugin. Other KWOK-based providers (EKS Auto) write the same two files against `perf`, and don't copy any tests.

## Audit it: `make perf-inventory`

The inventory prints every scenario, its sizes, each phase's level per metric, the sweep grids, and what every metric means. It's generated from the same values the suite runs, and needs no cluster. A level that names an undeclared phase makes it, and every spec, fail.

```
| scenario  | sizes                    | phase      | metric             | level        | bound    |
| pods      | pods=1000                | scaleOut   | total_time_minutes | experimental | < 2 min  |
| nodepools | nodepools=1000 pods=100  | scheduling | total_time_minutes | experimental | < 2 min  |
| topology  | pods=1000                | scaleOut   | cpu_cores          | experimental | < 1 cores|
...
| nodepools | nodepools [1 10 100 1000 10000] x pods [100 1000 5000] | 15 |   (sweeps)
```

## Concepts

- **Vector:** a size knob, for example `pods` or `nodepools`.
- **Scenario:** a workload shape. It declares a default size per vector, can declare several vectors, and can declare a sweep grid over them.
- **Phase:** one measured window of a scenario, for example `pods/scaleOut`.
- **Levels:** set per provider, per phase and metric.
  - `Stable`: a breach fails the spec.
  - `Experimental`: a breach is reported and warned about, but never fails.
  - Unlisted: the phase is off.
- **Metric:** carries its own `Measures` and `OnBreach` text, so alarms are generated rather than hand-written.

| Scenario | Vectors (default) | Phases | What it stresses |
|---|---|---|---|
| `pods` | pods=1000 | `scaleOut`, `consolidation` | provisioning throughput and disruption cost |
| `nodepools` | nodepools=1000, pods=100 | `scheduling`, `startup` | per-NodePool cost: N-1 NodePools carry a tenant taint the pods don't tolerate |
| `topology` | pods=1000 | `scaleOut` | hard constraints: hostname anti-affinity, zonal and hostname topology spread |

## Writing a scenario

```go
var Topology = perf.Scenario{
	Name:    "topology",
	Summary: "N pods with hostname anti-affinity and zonal/hostname topology spread.",
	Vectors: map[perf.Vector]int{perf.VectorPods: 1000},
	Phases:  []perf.PhaseSpec{{Name: "scaleOut", Summary: "Topology groups and domain counting."}},
	Run: func(r *perf.Run) {
		n := r.Size(perf.VectorPods)
		anti := r.Deployment("anti", n/10, "900m", "3100Mi", test.WithPodAntiAffinityHostname())
		...
		p := r.StartPhase("scaleOut")
		r.Create(r.NodeClass, r.NodePool, anti, zone, host)
		p.AwaitScaleOut(n, 20*time.Minute)
	},
}
```

How it works:
- `Run` labels and cleans up everything it creates. A spec fails up front if an earlier one left labeled objects behind.
- `StartPhase` fails on an undeclared phase, and on a phase the provider hasn't set levels for. An empty levels entry means measure only.
- The phase scrapes the leader's `/metrics` every 2 s through the API server proxy, and fails if the controller restarts mid-phase.

## Levels (per provider)

```go
Levels: perf.Levels{
	"pods/scaleOut":     {perf.HeapMB.Stable(150), perf.CPUCores.Experimental(0.7)},
	"nodepools/startup": {perf.StartupSeconds.Stable(60)},
},
```

Promote a gate to Stable when all of these hold:
1. 20 or more main runs.
2. CV of at most 20%.
3. A bound of at least 1.1x the observed max.
4. A patch in `perf/regressions/` breaches it.

Gate wall-clock consolidation time only as Experimental.

## Scaling two vectors together: sweeps and `make perf-plot`

A scenario's `Sweep` declares a grid. `KARPENTER_PERF_SWEEP=nodepools` runs one spec per cell. Sweep cells are measured, never gated, and each report records its `sizes` and the controller `commit`.

```sh
OUTPUT_DIR=/tmp/sweep KARPENTER_PERF_SWEEP=nodepools TEST_SUITE=Performancev2 make e2etests
KARPENTER_PERF_SWEEP_GRID='{"pods":[100,500],"nodepools":[1,100]}' ...   # smaller grid
make perf-plot DIR=/tmp/sweep    # writes perf.html
```

![example sweep on kind](docs/sweep-example.png)

`perf.html` has one heat map per phase and metric, with pods on x and NodePools on y:
- The shade is log-scaled, from green (the panel's lowest value) to red (its highest).
- Every cell prints its value, and hovering shows sizes and run count.
- Each panel has a table view.

## Running

```sh
OUTPUT_DIR=$(mktemp -d) TEST_SUITE=Performancev2 FOCUS=nodepools make e2etests
KARPENTER_PERF_SIZES='{"nodepools":10000}' ...                     # override a vector for every scenario
KARPENTER_PERF_THRESHOLDS='{"pods/scaleOut":{"heap_mb":200}}' ...  # per-runner bounds
```

## Regression patches

`perf/regressions/` holds one-line mutations and clean reverts of known perf fixes, each a patch against main. Prove a gate catches something by applying one, deploying, and running the scenario.

## Review standard

Every PR that adds or changes a perf test shows:
1. It catches a patch from `perf/regressions/`, and doesn't breach on a negative control.
2. 5 base runs with median, max and CV for each gated metric.
3. Where each bound comes from, and the regression size it lets through (bound / median - 1).
4. No Stable wall-clock gate on consolidation or drift.
5. It isn't duplicated in an open PR or an existing scenario.
6. It fails loudly on a controller restart.
7. What it costs, in minutes per run.
