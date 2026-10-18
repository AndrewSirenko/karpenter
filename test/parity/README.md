# kube-scheduler parity

Generates random clusters, provisions them with Karpenter, and checks every placement with the real kube-scheduler Filter plugins (TaintToleration, NodeAffinity, NodePorts, NodeResourcesFit, PodTopologySpread, InterPodAffinity) from `k8s.io/kubernetes`. It's a separate module so `k8s.io/kubernetes` stays out of Karpenter's `go.mod`.

## Run

```sh
export KUBEBUILDER_ASSETS=~/.local/share/kubebuilder-envtest/k8s/1.35.0-linux-amd64
cd test/parity
go test . -timeout 0 -args -ginkgo.focus='Parity should'                     # 20 seeds, small and medium
PARITY_SEEDS=1000 PARITY_OUT=/tmp/parity go test . -timeout 0 -args -ginkgo.focus='Parity should'
PARITY_SEED=79 PARITY_PROFILES=medium PARITY_SHRINK=1 PARITY_OUT=/tmp/parity go test . -timeout 0 -args -ginkgo.focus='Parity should'
go test . -args -ginkgo.focus='Known divergences'                              # minimized repros
```

| Variable | Default | |
|---|---|---|
| `PARITY_SEED_START`, `PARITY_SEEDS` | 1, 20 | seed range |
| `PARITY_SEED` | | run one seed |
| `PARITY_PROFILES` | `small,medium` | scenario sizes, see `Profiles` in `scenario.go` |
| `PARITY_ROUNDS` | 3 | provisioning rounds; later rounds see earlier nodes as initialized |
| `PARITY_OUT` | | write the report, raw discrepancies and one repro YAML per discrepancy kind |
| `PARITY_SHRINK`, `PARITY_SHRINK_BUDGET`, `PARITY_SHRINK_TRIES` | off, 120, 3 | shrink each repro |
| `PARITY_EXPLAIN`, `PARITY_EXPLAIN_KEY`, `PARITY_EXPLAIN_MAX` | | rerun seeds from a discrepancies JSON and print pods and nodes compactly |
| `PARITY_TRACE` | | print every bind attempt |

## What it checks

- **A: placed on a node kube-scheduler rejects.** kube-scheduler binds pods one at a time, so the check looks for an order in which every placement passes (forward, reverse, self-affinity first, six shuffles). A pod stuck in every order is class A. If every order strands some pod but none in all of them, the placements are reported as A2.
- **B: missed existing capacity.** Karpenter launched a node for a pod while a pre-existing node passes every filter in the final state.
- **C: false unschedulable.** Karpenter left a pod pending while some node passes every filter in the final state.

B and C are checked against the final state, so for pods with spread or pod affinity they're weaker and carry `[topology-dependent]`. A node that only fails once preferences are treated as requirements is labeled `node fails only a preference`.

`Allowlist` in `allowlist.go` drops reviewed, intentional divergences. `KnownIssues` keeps tracked bugs in the report without failing the run. Anything else fails.

## Limitations

- Karpenter isn't fully deterministic, so the same seed can produce different discrepancies. Shrinking retries each candidate.
- The checked plugins don't include volumes, DRA, or `NodeUnschedulable`.
- DaemonSets and pod overhead aren't generated.
- The fake cloud provider picks the cheapest compatible instance type and offering.
