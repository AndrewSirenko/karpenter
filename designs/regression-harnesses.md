# Regression Harnesses

## Motivation

52 of the 172 PRs merged from May to October 2026 were fixes, and many fixed a bug class that had shipped before: #3038 went to 13 release branches, #3335 brought back the #3144 leak, and nine scheduling bugs (#3146, #3333, #3401, #3423-#3425 and others) disagree with kube-scheduler. Reviewers catch some of these by hand. kind-perf-e2e failed 109 of 138 runs on main, so its red is ignored, and perf regressions like #3204 and #3211 are found by users.

## Proposal

### 1. karpenterlint

A golangci-lint module plugin in `hack/tools/karpenterlint`, built into `golangci-lint-karpenter` alongside kube-api-linter. `make karpenterlint` runs it; it joins `make verify` per rule once that rule has no hits.

| Analyzer | Catches | Issues |
|---|---|---|
| `errbranchuse` | reading `x` in the err branch of `x, err = f(x)` | #3350 |
| `copylit` | field-by-field copies that drop fields | #3278 |
| `loopabort` | one NodePool or candidate error aborting the pass | #3002, #3403 |
| `durationfloor` | time differences used as `RequeueAfter` or grace periods without a floor | #3038, #3407 |
| `observedonly` | `StatusConditions()` writing into cached objects on read paths | #3372 |
| `ginkgoonly` | `func TestX(t *testing.T)` outside suite bootstrap | review on #2894, #3217 |

Each rule ships with a test reproducing its bug and is checked against the commit before its fix.

kind e2e also runs the controller with `KUBE_CACHE_MUTATION_DETECTOR=true`, so client-go panics if anything writes into an informer cache object (#3372). Perf runs leave it off since it deep-copies every cached object.

### 2. kube-scheduler parity

A test module `test/parity` (own `go.mod`, so `k8s.io/kubernetes` stays out of the main module) generates seeded random pods, NodePools and nodes, runs Karpenter's scheduling simulation, turns the resulting NodeClaims into fake Nodes, and runs the real scheduler Filter plugins against them. It fails when kube-scheduler rejects a node Karpenter launched for a pod, or accepts an existing node Karpenter passed over, and prints the shrunk scenario as a Ginkgo entry. Intentional divergences live in a reviewed allowlist. A sibling test compares the DRA allocator with `k8s.io/dynamic-resource-allocation`.

### 3. perfTestV2

- Benchmarks in presubmit with `-count=6 -benchmem`, compared with `benchstat`; fail on allocs/op or B/op up over 10%.
- kind-perf thresholds relative to the last 30 main runs instead of literals.
- A nightly flake ledger that files one issue per flaky spec and quarantines it with `Label("flaky")`.

## Open Questions

1. Replace `golangci-lint-kube-api-linter` in `make verify` with `golangci-lint-karpenter` now?
2. Is `k8s.io/kubernetes` in a separate test module acceptable?
3. Where does the perf baseline live?
