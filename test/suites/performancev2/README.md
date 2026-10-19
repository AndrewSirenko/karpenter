# Performance suite v2

A self-contained, plugin-style perf suite. Shared scenarios live in `perf/scenarios`, the library in `perf`, and copied dependencies in `env`. This directory's `suite_test.go` and `kwok_provider.go` are the core KWOK provider's plugin. Other KWOK-based providers (EKS Auto) write the same two files against `perf`, and don't copy any tests.

## Audit it: `make perf-inventory`

The inventory prints every scenario, its sizes, each phase's level per metric, the sweep grids, and what every metric means. It's generated from the same values the suite runs, and needs no cluster. A level that names an undeclared phase makes it, and every spec, fail.

```
| scenario | sizes | phase | metric | level | bound |
|---|---|---|---|---|---|
| pods | pods=1000 | scaleOut | pod_ready_seconds_p95 | experimental | < 18 s |
| pods | pods=1000 | scaleOut | pod_ready_seconds_p99 | experimental | < 19 s |
| pods | pods=1000 | scaleOut | pod_ready_seconds_max | stable | < 20 s |
| pods | pods=1000 | scaleOut | heap_mb | stable | < 70.2 MB |
| pods | pods=1000 | scaleOut | cpu_cores | experimental | < 0.87 cores |
| pods | pods=1000 | consolidation | heap_mb | experimental | < 84.4 MB |
| pods | pods=1000 | consolidation | cpu_cores | experimental | < 0.24 cores |
| pods | pods=1000 | consolidation | pod_ready_seconds_p95 | experimental | < 60 s |
| pods | pods=1000 | consolidation | pod_ready_seconds_max | experimental | < 120 s |
| nodepools | nodepools=1000 pods=100 | scheduling | heap_mb | experimental | < 97 MB |
| nodepools | nodepools=1000 pods=100 | scheduling | cpu_cores | experimental | < 0.73 cores |
| nodepools | nodepools=1000 pods=100 | scheduling | pod_ready_seconds_p95 | experimental | < 30 s |
| nodepools | nodepools=1000 pods=100 | scheduling | pod_ready_seconds_max | experimental | < 60 s |
| nodepools | nodepools=1000 pods=100 | startup | startup_seconds | experimental | < 23.8 s |
| nodepools | nodepools=1000 pods=100 | startup | heap_mb | experimental | < 80 MB |
| topology | pods=1000 | scaleOut | heap_mb | experimental | < 250 MB |
| topology | pods=1000 | scaleOut | cpu_cores | experimental | < 1 cores |
| topology | pods=1000 | scaleOut | pod_ready_seconds_p95 | experimental | < 120 s |
| topology | pods=1000 | scaleOut | pod_ready_seconds_max | experimental | < 300 s |

| scenario/phase | p50 | p95 | p99 | max (p100) |
|---|---|---|---|---|
| pods/scaleOut | - | experimental < 18 s | experimental < 19 s | stable < 20 s |
| pods/consolidation | - | experimental < 60 s | - | experimental < 120 s |
| nodepools/scheduling | - | experimental < 30 s | - | experimental < 60 s |
| topology/scaleOut | - | experimental < 120 s | - | experimental < 300 s |

| scenario | grid | cells |
|---|---|---|
| nodepools | nodepools [1 10 100 1000 10000] x pods [100 1000 5000] | 15 |
```

## Concepts

- **Vector:** a size knob, for example `pods` or `nodepools`.
- **Scenario:** a workload shape. It declares a default size per vector, can declare several vectors, and can declare a sweep grid over them.
- **Phase:** one measured window of a scenario, for example `pods/scaleOut`.
- **Levels:** set per provider, per phase and metric.
  - `Stable`: a breach fails the spec.
  - `Experimental`: a breach is reported and warned about, but never fails.
  - Unlisted: the phase is off.
- **Metric:** carries its own `Measures` and `OnBreach` text, so alarms are generated rather than hand-written. Every metric is in seconds, MB or cores.
- **Pod latency quantiles:** `perf.PodReady.P50/P95/P99/Max` are separate metrics. Each one measures pods' creation-to-Ready seconds over pods created during the phase, and each gets its own level.
  - Typically you gate p95 and warn on p99 and max: max is the slowest customer's experience, but it's noisier.
  - `Validate` rejects bounds that decrease across quantiles.
  - It also rejects a phase that gates both `total_time_seconds` and `pod_ready_seconds_max`, since they measure nearly the same thing.

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

![30-cell sweep on EKS](docs/sweep-eks.png)

This is the 30-cell `nodepools` sweep on EKS. Setup:
- **Cluster:** `karpenter-perf-v2`, tier-xl provisioned control plane.
- **Controller:** 14 CPU / 56 Gi on a dedicated m7i.4xlarge.
- **Grid:** pods [100…20000] × nodepools [1…10000], all 30 cells passing.
- **Commit:** upstream main 6eac93ba.

What it shows:
- **Scale-out time is bound by node registration.** It's about 180 s at 20k pods regardless of NodePool count.
- **NodePool cost lands in heap and CPU.** That's why the `nodepools` levels gate on those.

  | | 1 NodePool | 10k NodePools |
  |---|---|---|
  | Heap at 100 pods | 25 MB | 275 MB |
  | Heap at 20k pods | 703 MB | 1114 MB |
  | CPU at 20k pods | 3.0 cores | 5.9 cores |

- **Startup has a cliff only where both vectors are high.** It's about 20 s across most of the grid, but 50-55 s at 3000+ NodePools × 20k pods, with 1 GB of heap at 10k × 20k.

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

`perf/regressions/` holds one-line mutations and clean reverts of known perf fixes, each a patch against main. Its [README](perf/regressions/README.md) has the measured table of which layer catches which regression: Go microbenchmarks, kind e2e, and v2 on EKS at default and large sizes.

The KWOK levels above come from 5 baselines on the `karpenter-perf-v2` EKS cluster. `pods/scaleOut` time and heap are Stable because a planted regression breaches them: concurrency forced to 1 breaches time, and reverting #1973 or #3211 breaches heap. Everything else stays Experimental until 20 main runs hold.

## Review standard

Every PR that adds or changes a perf test shows:
1. It catches a patch from `perf/regressions/`, and doesn't breach on a negative control.
2. 5 base runs with median, max and CV for each gated metric.
3. Where each bound comes from, and the regression size it lets through (bound / median - 1).
4. No Stable wall-clock gate on consolidation or drift.
5. It isn't duplicated in an open PR or an existing scenario.
6. It fails loudly on a controller restart.
7. What it costs, in minutes per run.
