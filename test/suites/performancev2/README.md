# Performance suite v2

KWOK end-to-end performance tests. This package replaces `test/suites/performance` one test at a time. When v1 is empty it gets deleted and this package is renamed.

## Pick the right layer

| You want to catch | Write |
|---|---|
| CPU or allocation cost of one function | A Go benchmark next to the code, with a `test_performance` build tag. Compare runs with `benchstat` at `-count=10`. These can see a 10% change. |
| Behavior across controllers: queueing, concurrency, informer churn, memory growth with scale | A phase in this suite. These only see large changes; avg CPU varies 10-25% run to run, and consolidation time 30-60%. |
| Real cloud API latency or quotas | The provider's scale suite |

If the regression would also show in a microbenchmark, write the microbenchmark first.

## Writing a test

```go
It("should scale out and consolidate", func() {
	dep := test.Deployment(test.CreateDeploymentOptions("app", 1000, "900m", "3100Mi"))

	scaleOut := StartPhase(env, "myTest/scaleOut")
	env.ExpectCreated(nodePool, nodeClass, dep)
	scaleOut.AwaitScaleOut(1000, 15*time.Minute)

	consolidation := StartPhase(env, "myTest/consolidation")
	dep.Spec.Replicas = lo.ToPtr(int32(700))
	env.ExpectUpdated(dep)
	consolidation.AwaitConsolidation(1000, 700, 20*time.Minute)
})
```

How `StartPhase` and the `Await` methods work:

- **Start before the action under test.** `StartPhase` starts the heap/CPU poller, the pprof profiler and the metrics-delta harness, and records the node and disruption baselines.
- **One `Await` method per phase kind.** `AwaitScaleOut`, `AwaitConsolidation` and `AwaitDrift` wait for the end state.
- **What every `Await` method does when it finishes.**
  1. Builds the `PerformanceReport`.
  2. Writes `<phase>_performance_report.json` and the profiles under `OUTPUT_DIR`.
  3. Fails if no metrics samples were collected.
  4. Checks the gates.
- **The phase ID** (`<test>/<phase>`) names everything: the gates, the overrides and the artifacts. `StartPhase` fails on an ID with no gates.

## Gates and tiers

Every bound lives in `gates.go`:

```go
"myTest/scaleOut": {Experimental(HeapMB, 130), Experimental(CPUCores, 0.50)},
```

Each gate has a tier:

- **`Experimental`.** Breaches are recorded in the report's `gates` array, as a Ginkgo report entry and as a GitHub warning. They never fail CI. New gates start here.
- **`Stable`.** A breach fails the test.

**Promote** a gate to `Stable` in its own PR, once all of these are true:

1. It has at least 20 main-branch runs of data.
2. Its CV over those runs is at most 20%.
3. The bound is at least 1.1x the highest value seen. State the regression size it lets through, computed as bound / median - 1.
4. A planted regression breaches it.

**Demote** a gate that fails 2 or more of the last 20 main runs with no regression found. Don't bump its bound instead.

Gate wall-clock consolidation and drift time only as `Experimental`.

Providers override bounds through `KARPENTER_PERF_THRESHOLDS`, keyed by phase ID and then metric name:

```json
{"basic/scaleOut": {"heap_mb": 200, "cpu_cores": 1.2, "total_time_minutes": 3}}
```

An unknown phase or metric fails the suite in `BeforeSuite`.

## Author checklist

- [ ] Layer chosen per the table above. For e2e, say why a microbenchmark can't see it.
- [ ] The scenario matches a real customer pattern.
- [ ] One `StartPhase` per measured window; gates added as `Experimental`.
- [ ] Bounds come from data. Give median, max and CV over N runs (`active_time`, `total_time`, heap, CPU from the report JSON).
- [ ] A planted regression breaches a gate, with numbers before and after. A one-line hand mutation of a known perf fix is enough.
- [ ] An unrelated change does not breach.
- [ ] No new helpers in `test/pkg/environment/common`. Reuse `Phase` and `test.CreateDeploymentOptions`.
- [ ] Ginkgo only; no plain-Go tests of unexported helpers; comments trimmed.
- [ ] The CI cost (minutes per run) is in the PR description.

## Reviewer checklist

1. Does it catch anything? Look for the planted regression.
   - Log-only checks catch nothing.
   - So do runs against an empty cluster.
   - So do gates that pass on zero samples.
2. Where did each bound come from? A bound below the observed median is a guaranteed flake.
3. Is wall-clock time gated as `Stable`? It shouldn't be.
4. Does an open PR already cover the same scenario?
5. Will a Karpenter restart mid-phase fail loudly?

## Running locally

```sh
make setup-kind KIND_CLUSTER_NAME=perf
KWOK_REPO=kind.local KIND_CLUSTER_NAME=perf make install-kwok apply-with-kind
OUTPUT_DIR=$(mktemp -d) TEST_SUITE=Performancev2 FOCUS="Basic Deployment" make e2etests
```
