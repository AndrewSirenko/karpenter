# Regression Harnesses

Catch the bug classes that keep coming back in review and CI instead of in clusters.

## Motivation

Between May and October 2026, 172 PRs merged into karpenter and 52 of them were fixes. Many fixed the same few shapes of bug:

- A fix to one call site, then the same bug at the next one. #3038 clamped a negative pod grace period and was cherry-picked to 13 release branches (#3043, #3050-#3062); the annotation writer that caused it is still in `legacyrepair`. #3063 fixed an eviction queue leak (#3144) and #3335 added a new return path with the same leak.
- A new field silently dropped by a hand-written copy. #3278 (`DynamicResources` in node overlay apply), #3174 and #3166 (capacity buffer pod template labels and affinity), #3189 (StatefulSet `volumeClaimTemplates`).
- Shared objects mutated through read-looking calls. #3372 (`StatusConditions()` writing Unknown conditions into a cached NodeClaim), #3104 (label normalization rewriting the caller's slice), #3116 (DRA counter race).
- Metric contracts broken by renames and casing changes: #3284, #3294, #3323, #3348, #3359.
- Scheduling decisions that disagree with kube-scheduler. #3065, #3146, #3205, #3274, #3333, #3401, #3423, #3424, #3425 are all cases where Karpenter launched or refused a node on a different answer than the scheduler that later binds the pod.
- Perf regressions found by users, not CI. #3204 (v1.13 doubled CPU), #3211 (OOM at 1000 pods x 24 NodePools). Meanwhile kind-perf-e2e failed 109 of 138 runs on main, so nobody trusts its red.

Reviewers caught many of these and asked for the same changes over and over: use the injected clock (4 PRs), use Ginkgo (5 PRs), register annotations and metric values as constants (6 PRs), don't mutate cache objects. `AGENTS.md` states several of these rules but nothing enforces them, and agent-written PRs break them most often.

### Use Cases

1. A contributor writes `cmd, err = validate(cmd); if err != nil { cmd.EmitRejectedEvents() }`. Lint fails before review (#3350).
2. A contributor adds a field to `InstanceType`. Lint fails on every hand-written copy that does not set it (#3278).
3. A scheduling change makes Karpenter think a pod fits a NodeClaim that kube-scheduler's `PodTopologySpread` filter would reject. A differential test fails in presubmit (#3333).
4. A change adds a `hashstructure.Hash` to the scheduling loop. The benchmark gate fails on allocs/op before it ships (#3204).

### Non-Goals

- Fixing today's violations. This RFC adds the harnesses with a baseline; cleanup is follow-up work.
- Replacing the e2e suites. The harnesses here run in unit-test time.
- Provider-specific rules. karpenter-provider-aws can import `karpenterlint` and add its own analyzers.

## Proposal

Three harnesses, each aimed at a different layer.

### 1. karpenterlint

A golangci-lint v2 module plugin in `hack/tools/karpenterlint`, built into a `golangci-lint-karpenter` binary the same way `kube-api-linter` is today. The binary bundles both plugins, so it can replace `golangci-lint-kube-api-linter` in `make verify` once the baseline is clean.

Each analyzer is one package whose doc comment lists the issues that motivated it. Initial rules:

| Analyzer | Catches | Motivating issues |
|---|---|---|
| `errbranchuse` | reading `x` in the err branch of `x, err = f(x)` | #3350 |
| `observedonly` | `StatusConditions()` with no options on read-only paths and in predicates | #3372 |
| `condpatch` | conditions set, then patched without optimistic lock | #3360, #3371 |
| `statewriter` | cluster-state mutators called outside `pkg/controllers/state` | #3176, #3190, #3250 |
| `nsnamekey` | namespaced objects keyed by name only | #3102 |
| `loopabort` | one NodePool or candidate error aborting the whole pass | #3002, #3398, #3403 |
| `durationfloor` | time differences used as `RequeueAfter` or grace periods without a floor | #3038, #3407 |
| `copylit` | field-by-field copies missing fields | #3278 |
| `hashunexported` | `hashstructure.Hash` on types with unexported fields it silently skips | `TopologyGroup.Hash` collision |
| `parammut` | functions writing into or sorting slice/map parameters | #3104, #3022, #3116 |
| `metriclabels` | label keys at emission and delete sites that differ from the declared labels | #3359 |
| `metriclabelvalues` | label values that are empty or not lowercase snake_case | #3284, #3323, #3179, #3404 |
| `metrichelp` | "Labeled by" prose in metric Help that drifts from the real labels | #3295 |
| `wellknownkeys` | `karpenter.sh/` string literals outside `pkg/apis` | #3364, #3375, #3376 |
| `ginkgoonly` | `func TestX(t *testing.T)` that is not a suite bootstrap | review on #2894, #2962, #3028, #3217, #3247 |
| `eventdedupe` | events without `DedupeValues` | review on #2962, #3028, #3311, #3367 |

The same config turns on built-in linters for the other recurring hits: `forbidigo` for `time.Now`/`Since`/`Until` and `clock.RealClock` (#3009, #3045, #3406), `ginkgolinter` for assertions with no matcher (17 on main), `modernize` (drift after #3024's `go fix`), `prealloc` and gocritic `rangeValCopy` (#3211, #3310).

#### How It Works

- `.golangci.karpenterlint.yaml` enables only the new linters. `make karpenterlint` runs it and `make test-karpenterlint` runs the analyzer tests (`analysistest`, each with the buggy shape from its motivating issue).
- Rollout: report-only first. Then a ratchet that fails when a rule's count on a PR exceeds main's. Then fold into `.golangci.yaml` per rule as each reaches zero.
- New rules require a test reproducing the motivating bug and a run against the commit before its fix showing the rule fires. Of the first seven motivating fixes checked this way, six fire before the fix and go quiet after it; the capacity buffer template bug (#3174) converts between types and needs a different rule.

#### Edge Cases

- Intentional violations use `//nolint:karpenterlint // <reason>`; `copylit` also accepts `//karpenterlint:partial-copy <reason>`.
- Rules that need a call graph (NotFound on the cluster-sync path, #3169; queue entries released on every exit, #3144) are left for a second phase using `go/ssa`.

### 2. kube-scheduler filters against Karpenter's simulation

Karpenter re-implements node affinity, topology spread, inter-pod affinity, host ports, resource fit, volume limits and DRA allocation. There are 17 code comments claiming parity with upstream and no test that checks it. Most open scheduling bugs are divergences: #3146, #3333, #3401, #3423, #3424, #3425, #3065, #3196, #3212, #3213, #3156.

#### How It Works

A test module `test/parity` (separate `go.mod`, so `k8s.io/kubernetes` never enters the main module):

1. Generate random scenarios: pods with requirements, affinities, spreads and claims; NodePools; existing nodes. Seeded so failures reproduce.
2. Run Karpenter's provisioning simulation (`scheduling.NewScheduler(...).Solve`).
3. Turn each resulting NodeClaim into a fake `corev1.Node` with the labels, taints, capacity and allocatable it would register with.
4. Run the real scheduler framework's Filter plugins (NodeAffinity, PodTopologySpread, InterPodAffinity, NodePorts, NodeResourcesFit, NodeVolumeLimits, DynamicResources) against those nodes plus the existing ones.
5. Fail when:
   - Karpenter put a pod on NodeClaim X and kube-scheduler rejects X (Karpenter launches a node the pod can't use: #3423, #3333).
   - kube-scheduler accepts an existing node that Karpenter rejected and launched for anyway (wasted capacity: #3146, #3401).
6. Shrink the failing scenario to a minimal one and print it as a Ginkgo `Entry`, so the fix PR can add a normal regression test.

A smaller sibling compares `pkg/scheduling/dynamicresources` with the upstream structured allocator in `k8s.io/dynamic-resource-allocation` on the same ResourceSlices and claims (#3209, #3212, #3213, #3156).

Requirement algebra gets a property test alongside: intersect random pairs over a small universe and check that `Intersection(a,b).Matches(v) == a.Matches(v) && b.Matches(v)` for every `v`. #3423 and #3425 are both counterexamples.

#### Interaction with Existing Features

Some divergences are intentional, for example Karpenter's own handling of preferences during relaxation. Those live in an explicit allowlist keyed by plugin and reason, so each one is a reviewed decision instead of drift.

### 3. perfTestV2

The perf suite (`test/suites/performance`) runs real kind clusters and gates on absolute thresholds like "avg CPU < 0.30 cores". On main it failed 109 of 138 runs. The flaky gate moved from memory to CPU after #3382, and #3165 added per-test overrides that providers tune by hand. Allocation regressions (#3204, #3211, #3310) were found by users anyway.

#### How It Works

1. **Benchmark gate in presubmit.** Run every `Benchmark*` (scheduling, nodeoverlay, state, node/health, plus #3299's `NewScheduler`/`NewTopology` sweeps) with `-count=6 -benchmem` on base and head, compare with `benchstat`, and fail on allocs/op or B/op up more than 10%. Allocation counts barely vary between runs, so this gate is quiet. Report sec/op without gating.
2. **Relative thresholds for kind perf.** Keep the last 30 main runs' metrics as an artifact. A PR fails when it's outside that run's P95 plus a margin, not when it crosses a literal. Literal thresholds remain only as an outer bound.
3. **Flake ledger.** A nightly job reads junit from recent kind-e2e and kind-perf-e2e runs, counts failures per spec and Kubernetes version, and keeps one issue per spec above a threshold. Quarantine with Ginkgo `Label("flaky")` and `--label-filter='!flaky'` on the gate.
4. **Scale sweep.** Run the scheduling benchmarks across pods x NodePools x DaemonSets (#3211 was 1000 x 24) and fail on super-linear growth between steps.
5. **Profile diff.** On a gate failure, attach a pprof diff of base vs head so the PR author sees the hot path without rerunning.

#### Observability

Every run uploads benchstat output, the rolling baseline it compared against, and the profile diff.

## Alternatives Considered

### Ruleguard or semgrep instead of go/analysis

Cheaper to write, but most rules need types (which object came from the informer cache, which metric var declared which labels) or cross-package facts. Simple pattern rules can still use `forbidigo` inside the same config.

### Fix everything first, then enable

The cleanup would be thousands of lines across many owners and would rot before it merged. A baseline and ratchet lets each rule land now and burn down separately.

### Envtest with the real scheduler instead of calling Filter plugins

Higher fidelity but minutes per scenario, so it can't explore enough random cases. Calling the plugins directly runs thousands of scenarios per second.

## Backward Compatibility

No user-facing change. `make verify` is unchanged until rules reach zero violations. The tools module upgrades `go.tools.mod` dependencies to what golangci-lint v2.12.2 already required through its existing `replace`.

## Graduation Criteria

- karpenterlint: each rule moves from report-only to the ratchet when it has a test reproducing its motivating bug, and into `make verify` at zero violations.
- Parity harness: starts nightly; becomes a required presubmit once it has run clean for two weeks with the allowlist reviewed.
- perfTestV2: the benchmark gate is required from day one (alloc counts are stable); the relative kind-perf thresholds replace the literals after 30 baseline runs.

## Open Questions

1. Should `golangci-lint-karpenter` replace `golangci-lint-kube-api-linter` in `make verify` now, with the new rules disabled there?
2. Is `k8s.io/kubernetes` in a separate test module acceptable to maintainers, or should the parity harness vendor only the plugin packages it needs?
3. Where should the rolling perf baseline live: a GitHub Actions cache, a release artifact, or a small bucket?
4. Which intentional divergences from kube-scheduler should seed the parity allowlist?
