# Karpenter Monitoring and Alerting Guide

Karpenter exposes Prometheus metrics that tell you whether pending pods are getting nodes, whether those nodes launch
and join the cluster, whether disruption is making progress or running away, and whether the controller itself is
healthy. This guide picks a small set of service level objectives and alerts that cover those four areas, with PromQL
you can copy and thresholds to start from.

You scrape the Karpenter controller with Prometheus or any agent that speaks the Prometheus format, and you route the
alerts with whatever you already use. The examples use the `PrometheusRule` resource from the Prometheus Operator, but
the expressions work in any PromQL-compatible backend. Every threshold here is a starting point. Tune it against a few
weeks of your own data.

Note that this guide covers core Karpenter only. It CANNOT tell you about cloud-provider details such as interruption
queues or cloud API throttling, because those metrics come from your provider. Read the provider's metrics reference
alongside this guide, for example the [AWS provider metrics reference](https://karpenter.sh/docs/reference/metrics/).

---

## 1. How Karpenter metrics work

Karpenter is the node autoscaler. It watches for pods the Kubernetes scheduler can't place, launches nodes for them,
and removes or replaces nodes it no longer needs. What Karpenter may launch is described by NodePools (a set of rules
for the nodes Karpenter creates). Each node Karpenter launches is first represented by a NodeClaim (Karpenter's record
of a node it asked the cloud provider for), which you see in metric names and labels.

The controller serves metrics on port `8080` at `/metrics` (set with `METRICS_PORT`). Karpenter runs several replicas,
but only the leader runs controllers, so almost every `karpenter_*` and `operator_*` series comes from one pod at a
time. Aggregate with `sum` or `max` across pods rather than alerting per pod, and expect series to move to a new pod
after a leader change.

Metrics prefixed `operator_nodeclaim_` and `operator_nodepool_` describe status conditions on those objects (for
example how long a NodeClaim took to become `Initialized`). They, the per-pod and per-node gauges, and the NodePool
usage gauges are turned off by `--disable-cluster-state-observability` (`DISABLE_CLUSTER_STATE_OBSERVABILITY`). If you
set that flag, several alerts below have no data.

Most Karpenter metrics are marked Alpha or Beta and can change between minor releases. Check the metrics reference for
your version when you upgrade, and keep alerts that use Alpha metrics at ticket severity until you trust them.

---

## 2. What to measure

Alert on what your users feel, and use the rest to explain why. For a node autoscaler, users feel pods that wait too
long for capacity. Everything else (launch failures, budgets, API errors) is a cause, and is worth a ticket rather than
a page unless pods are also waiting.

The table below lists the service level indicators (SLIs) worth tracking and an objective to start from.

| SLI | Metric | Starting objective |
|---|---|---|
| Time from Karpenter's scheduling decision to pod bound | `karpenter_pods_provisioning_bound_duration_seconds` | 95% of pods bound within 3 minutes |
| Time from NodeClaim creation to node initialized | `operator_nodeclaim_status_condition_transition_seconds{type="Initialized"}` | 95% within 5 minutes |
| Share of NodeClaim launches that fail | `karpenter_cloudprovider_errors_total{method="Create"}` over `karpenter_cloudprovider_duration_seconds_count{method="Create"}` | below 5% over 1 hour, excluding insufficient capacity |
| Karpenter is able to act | `karpenter_cluster_state_synced`, `leader_election_master_status` | synced with one leader 99.9% of the time |

The launch SLI depends heavily on your provider, image and node bootstrap, so measure your own baseline before you set
an objective.

Three habits keep these alerts quiet when nothing is wrong. Use ratios rather than raw error rates, so a large cluster
doesn't page more than a small one. Require a minimum volume before you alert on a ratio or a quantile, because a p99
over four events is noise. Put a `for` duration on alerts over gauges, and use a window of at least 15 minutes on
counters, so a single slow loop doesn't fire them.

---

## 3. Before you start

You need Prometheus, or a compatible agent, scraping the Karpenter controller pods. With the Prometheus Operator that
is a `ServiceMonitor` or `PodMonitor` selecting the Karpenter pods on the `http-metrics` port. The Karpenter Helm
charts create one for you when you set `serviceMonitor.enabled=true`. Check that the scrape works by querying `karpenter_build_info`, which every Karpenter pod exports with its version.

Two controller health alerts need [kube-state-metrics](https://github.com/kubernetes/kube-state-metrics), because
Karpenter can't report its own restarts or out-of-memory kills. Most clusters running Prometheus already have it.

The examples assume the scrape job is called `karpenter`. Change the `job` matcher if yours differs.

---

## 4. Your first alerts

Start with four alerts. Each one means Karpenter can't do its job right now, and together they catch most incidents
before users report them. The rule below is also in [Appendix A.1](#a1-karpenter-alertsyaml) as
`karpenter-alerts.yaml`, together with the alerts from later sections.

```yaml
apiVersion: monitoring.coreos.com/v1
kind: PrometheusRule
metadata:
  name: karpenter-core
spec:
  groups:
    - name: karpenter.core
      rules:
        - alert: KarpenterClusterStateUnsynced
          expr: max(karpenter_cluster_state_unsynced_time_seconds) > 300   # Karpenter does nothing while unsynced
          for: 5m
          labels: { severity: page }
          annotations:
            summary: Karpenter cluster state has not synced for over 5 minutes, so provisioning and disruption are paused.
        - alert: KarpenterPodsWaitingForCapacity
          expr: max(karpenter_pods_provisioning_unbound_time_seconds) > 900   # Karpenter decided to place the pod, but it is still unbound
          for: 5m
          labels: { severity: page }
          annotations:
            summary: A pod Karpenter is provisioning for has waited more than 15 minutes for a node.
        - alert: KarpenterNoLeader
          expr: sum(leader_election_master_status{job="karpenter", name="karpenter-leader-election"}) < 1 or absent(leader_election_master_status{job="karpenter"})
          for: 5m
          labels: { severity: page }
          annotations:
            summary: No Karpenter replica holds the leader lease, so nothing is being provisioned or disrupted.
        - alert: KarpenterNodeRegistrationUnhealthy
          expr: max by (name) (operator_nodepool_status_condition_count{type="NodeRegistrationHealthy", status="False"}) > 0
          for: 15m
          labels: { severity: page }
          annotations:
            summary: Nodes launched for NodePool {{ $labels.name }} are failing to join the cluster.
```

Apply it with `kubectl apply -f karpenter-alerts.yaml` in the namespace your Prometheus watches for rules, and check
that Prometheus loaded it under Status, Rules. To remove it, run `kubectl delete -f karpenter-alerts.yaml`.

`KarpenterClusterStateUnsynced` fires when Karpenter's in-memory view of nodes and NodeClaims doesn't match the API
server. While that lasts, Karpenter launches and removes nothing, and it does so silently. Look at the controller logs
for the NodeClaims or nodes it can't reconcile.

`KarpenterPodsWaitingForCapacity` fires when Karpenter has decided which node a pod should land on, but the pod still
isn't bound 15 minutes later. That usually means launches are failing or nodes aren't joining. Section 6 covers the
alerts that tell you which.

`KarpenterNoLeader` fires when no replica is the leader, including when every replica is down. Check the Karpenter
pods and their `Lease` in the Karpenter namespace.

`KarpenterNodeRegistrationUnhealthy` fires when Karpenter has marked a NodePool's `NodeRegistrationHealthy` condition
`False`, which happens when nodes it launches for that NodePool repeatedly fail to register. A bad image, broken node
bootstrap or a networking change are the usual causes. Don't filter the other alerts on this condition. A NodePool
that can't register nodes is exactly when you want the pending-pod alerts to fire.

---

## 5. Provisioning: pods that stay pending

Karpenter tracks how long each pending pod has waited, in a gauge that exists only while the pod is pending. Three
gauges answer different questions. `karpenter_pods_unbound_time_seconds` is the age of every pending pod, whether or
not Karpenter can help it. `karpenter_pods_provisioning_unbound_time_seconds` covers only pods Karpenter has decided to
provision for. `karpenter_pods_provisioning_scheduling_undecided_time_seconds` covers pods Karpenter has seen but not
yet made a decision on. These gauges carry `name` and `namespace` labels, so aggregate them before you alert.

```yaml
- alert: KarpenterPodsUnschedulable
  expr: min_over_time(karpenter_scheduler_unschedulable_pods_count{controller="provisioner"}[15m]) > 0   # every loop for 15m had pods it couldn't place
  for: 15m
  labels: { severity: ticket }
- alert: KarpenterPodsUndecided
  expr: max(karpenter_pods_provisioning_scheduling_undecided_time_seconds) > 300
  for: 5m
  labels: { severity: ticket }
- alert: KarpenterNodePoolNearLimit
  expr: max by (nodepool, resource_type) (karpenter_nodepools_usage / karpenter_nodepools_limit) > 0.9
  for: 15m
  labels: { severity: ticket }
```

If `KarpenterPodsUnschedulable` fires, Karpenter found pods that no NodePool can satisfy. The cause is usually in the
workload or NodePool, for example a node selector no NodePool allows, a request larger than any instance type, or a
NodePool at its limit. Run `kubectl get events --field-selector reason=FailedScheduling` and read Karpenter's events on
the pod for the reason. Pods Karpenter ignores entirely, for example pods with an invalid volume, are counted in
`karpenter_scheduler_ignored_pods_count`.

If `KarpenterPodsUndecided` fires, Karpenter has seen pods but hasn't reached a scheduling decision for them in five
minutes. That points at a slow or stuck provisioning loop. Check `karpenter_scheduler_scheduling_duration_seconds` and
`karpenter_scheduler_unfinished_work_seconds`, which show a simulation that is still running.

If `KarpenterNodePoolNearLimit` fires, a NodePool is about to hit `spec.limits`. Once it does, Karpenter stops
launching for that NodePool and pods stay pending with no error metric. Raise the limit or add another NodePool.

---

## 6. Node launch and registration

After Karpenter creates a NodeClaim, the cloud provider has to launch an instance (`Launched`), the node has to join
the cluster (`Registered`), and it has to become ready with its resources reported (`Initialized`). Karpenter deletes a
NodeClaim that isn't launched within 5 minutes or registered within 15 minutes, and counts it in
`karpenter_nodeclaims_disrupted_total` with reason `launch_timeout` or `registration_timeout`.

```yaml
- record: karpenter:nodeclaim_initialized_seconds:p95
  expr: histogram_quantile(0.95, sum by (le) (rate(operator_nodeclaim_status_condition_transition_seconds_bucket{type="Initialized", status="Unknown", to_status="True"}[30m])))
- alert: KarpenterNodeClaimsSlowToInitialize
  expr: |
    karpenter:nodeclaim_initialized_seconds:p95 > 600
    and on() sum(increase(operator_nodeclaim_status_condition_transition_seconds_count{type="Initialized", status="Unknown", to_status="True"}[30m])) >= 5
  for: 15m
  labels: { severity: ticket }
- alert: KarpenterNodeClaimLivenessTimeouts
  expr: sum by (nodepool, reason) (increase(karpenter_nodeclaims_disrupted_total{reason=~"launch_timeout|registration_timeout"}[30m])) > 0
  labels: { severity: ticket }
- alert: KarpenterLaunchErrorRatio
  expr: |
    sum(rate(karpenter_cloudprovider_errors_total{method="Create", error!="InsufficientCapacityError"}[15m]))
      / sum(rate(karpenter_cloudprovider_duration_seconds_count{method="Create"}[15m])) > 0.2
    and on() sum(increase(karpenter_cloudprovider_duration_seconds_count{method="Create"}[15m])) >= 10
  for: 15m
  labels: { severity: ticket }
- alert: KarpenterInsufficientCapacity
  expr: sum by (nodepool) (increase(karpenter_nodeclaims_disrupted_total{reason="insufficient_capacity"}[30m])) > 10
  for: 15m
  labels: { severity: ticket }
```

If `KarpenterNodeClaimsSlowToInitialize` fires, nodes take more than 10 minutes from NodeClaim creation to initialized.
Compare the `Launched` and `Registered` transitions in the same histogram to see which step is slow. A slow `Launched`
points at the cloud provider. A slow `Registered` or `Initialized` points at the node image, bootstrap, or a DaemonSet
or device plugin that never reports ready.

If `KarpenterNodeClaimLivenessTimeouts` fires, Karpenter is deleting NodeClaims that never launched or never joined.
One or two can be transient. A steady stream for one NodePool is the same failure `KarpenterNodeRegistrationUnhealthy`
pages on, caught earlier.

If `KarpenterLaunchErrorRatio` fires, more than a fifth of launch calls are failing for reasons other than capacity.
Group `karpenter_cloudprovider_errors_total` by `error` to see which. `NodeClassNotReadyError` means the NodeClass
referenced by the NodePool isn't ready. `unknown` covers everything the provider doesn't classify, including
permission errors and throttling, and your provider's metrics have the detail.

If `KarpenterInsufficientCapacity` fires, the cloud provider is out of the instance types the NodePool allows.
Karpenter retries with other offerings, so this matters most when pods are also waiting. Widen the NodePool's instance
type, zone or capacity type requirements so Karpenter has more to choose from.

---

## 7. Disruption

Disruption is how Karpenter removes or replaces nodes: consolidation (removing empty or underused nodes), drift
(replacing nodes that no longer match their NodePool or NodeClass), expiration and repair. Disruption budgets on each
NodePool cap how many nodes can be disrupted at once. Two failure modes matter. Disruption can stall, which costs
money and leaves drifted nodes in place. Or it can run away, which churns workloads.

```yaml
- alert: KarpenterNodeClaimStuckTerminating
  expr: max by (name, karpenter_sh_nodepool) (operator_nodeclaim_termination_current_time_seconds) > 3600
  for: 15m
  labels: { severity: ticket }
- alert: KarpenterEvictionsBlockedByPDB
  expr: |
    sum(increase(karpenter_pods_eviction_requests_total{code="429"}[30m])) > 0
    and on() max(operator_nodeclaim_termination_current_time_seconds) > 1800
  for: 30m
  labels: { severity: ticket }
- alert: KarpenterDisruptionBudgetExhausted
  expr: |
    max by (nodepool, reason) (karpenter_nodepools_allowed_disruptions - karpenter_nodepools_nodes_consuming_budgets) <= 0
    and on(reason) max by (reason) (karpenter_voluntary_disruption_eligible_nodes) > 0
  for: 6h
  labels: { severity: ticket }
- alert: KarpenterDisruptionQueueFailures
  expr: sum by (reason, decision) (increase(karpenter_voluntary_disruption_queue_failures_total[1h])) > 0
  labels: { severity: ticket }
- alert: KarpenterDisruptionChurn
  expr: |
    sum(increase(karpenter_nodeclaims_disrupted_total{reason=~"empty|underutilized|drifted"}[1h]))
      / max(karpenter_cluster_state_node_count) > 0.25
  for: 1h
  labels: { severity: ticket }
```

If `KarpenterNodeClaimStuckTerminating` fires, a NodeClaim has been deleting for over an hour. The usual causes are a
PodDisruptionBudget (PDB) that allows no evictions, a pod with the `karpenter.sh/do-not-disrupt` annotation, a volume
that won't detach, or a finalizer on the node. Run `kubectl describe nodeclaim <name>` and read the events on the node.
Stuck NodeClaims count against the disruption budget, so one of them can quietly stop consolidation for the whole
NodePool.

If `KarpenterEvictionsBlockedByPDB` fires, the Kubernetes eviction API is refusing Karpenter's evictions because of a
PDB, and a node has been draining for over 30 minutes. Find the PDB with `kubectl get pdb -A` and look for
`ALLOWED DISRUPTIONS` of 0. A pod covered by more than one PDB is rejected with HTTP 500 rather than 429, so also check
`code="500"` if evictions aren't progressing.

If `KarpenterDisruptionBudgetExhausted` fires, nodes have been eligible for disruption for six hours but the NodePool's
budget allows none. That can be intended, for example a budget of `0` outside a maintenance window. If it isn't,
nodes stuck terminating or `NotReady` nodes are using up the budget. Karpenter also emits a `DisruptionBlocked` event
on the NodePool.

If `KarpenterDisruptionQueueFailures` fires, Karpenter started a disruption, usually a replacement, and gave up because
the replacement node never initialized. The old node stays. Repeated failures for `drifted` usually mean the new
NodeClass or image is broken, and every replacement will fail the same way.

If `KarpenterDisruptionChurn` fires, Karpenter disrupted more than a quarter of the cluster's nodes in an hour.
Short node lifetimes in `karpenter_nodes_lifetime_duration_seconds` confirm it. Consolidation that keeps replacing the
same capacity usually points at `consolidateAfter` set too low, or at pods whose requests change often. A drift wave
after a NodeClass or image change is expected, so add a disruption budget scoped to `Drifted` if you want it slower.

### Forced terminations after terminationGracePeriod

A NodePool's `terminationGracePeriod` is the longest Karpenter will wait to drain a node. As the deadline approaches,
Karpenter deletes each remaining pod early enough for the pod's own `terminationGracePeriodSeconds` to end by the
deadline, without honoring PDBs or `karpenter.sh/do-not-disrupt`. Any such deletion is worth a look, because it usually
means a PDB or a do-not-disrupt pod would otherwise have blocked the node forever, and the workload lost pods it was
trying to protect.

Karpenter has no metric that counts these deletions today. The closest signal is a node that has been terminating for
most of its grace period. Set the threshold to your `terminationGracePeriod` minus your longest pod
`terminationGracePeriodSeconds`, so the alert fires around when forced deletions start:

```yaml
- alert: KarpenterTerminationGracePeriodNearlyElapsed
  expr: max by (name, karpenter_sh_nodepool) (operator_nodeclaim_termination_current_time_seconds) > 0.9 * 86400   # 90% of a 24h terminationGracePeriod
  labels: { severity: ticket }
```

Each forced deletion also produces a `Disrupted` event on the pod whose message contains `This bypasses the PDB of the
pod and the do-not-disrupt annotation`. If you export Kubernetes events to your log or alerting system, alert on that
message. The `TerminationGracePeriodExpiring` Warning event on the node is not the same signal. Karpenter emits it as
soon as a node with a `terminationGracePeriod` starts draining.

---

## 8. Controller health

Karpenter is a single active controller, so its health is a dependency for every node in the cluster. Large clusters
need more memory and CPU than the chart defaults, and out-of-memory kills or CPU throttling show up first as slow
provisioning.

```yaml
- alert: KarpenterRestarting
  expr: sum(increase(kube_pod_container_status_restarts_total{container="controller", namespace="karpenter"}[1h])) > 2
  labels: { severity: ticket }
- alert: KarpenterOOMKilled
  expr: max(kube_pod_container_status_last_terminated_reason{container="controller", namespace="karpenter", reason="OOMKilled"}) > 0
  labels: { severity: ticket }
- alert: KarpenterReconcileErrorRatio
  expr: |
    sum by (controller) (rate(controller_runtime_reconcile_errors_total{job="karpenter"}[15m]))
      / sum by (controller) (rate(controller_runtime_reconcile_total{job="karpenter"}[15m])) > 0.1
    and sum by (controller) (increase(controller_runtime_reconcile_total{job="karpenter"}[15m])) >= 20
  for: 15m
  labels: { severity: ticket }
- alert: KarpenterReconcilePanics
  expr: sum by (controller) (increase(controller_runtime_reconcile_panics_total{job="karpenter"}[30m])) > 0
  labels: { severity: ticket }
- alert: KarpenterControllerHung
  expr: max by (controller) (workqueue_unfinished_work_seconds{job="karpenter", controller=~"provisioner|disruption|disruption.queue|eviction-queue|node.termination|nodeclaim.lifecycle"}) > 900
  for: 15m
  labels: { severity: page }
- alert: KarpenterKubeAPIThrottled
  expr: sum(rate(client_go_request_total{job="karpenter", code="429"}[5m])) > 0
  for: 15m
  labels: { severity: ticket }
```

The restart and out-of-memory alerts use kube-state-metrics. Adjust `namespace` and `container` to match your install.
If they fire, raise the controller's memory request and limit, and check the release notes for your version.

If `KarpenterReconcileErrorRatio` fires, one controller is failing more than 10% of its reconciles. The `controller`
label names it, and the controller logs show the error. A spike right after an upgrade or a CRD change is the most
common case.

If `KarpenterControllerHung` fires, a key controller has had work in progress for 15 minutes without finishing. The
reconcile rate for that controller in `controller_runtime_reconcile_total` usually goes flat at the same time. A
restart of the leader clears it, but capture a goroutine dump from the pprof endpoint first if profiling is enabled, and
report it.

If `KarpenterKubeAPIThrottled` fires, the API server is rate limiting Karpenter. Karpenter's own client-side limiter
shows up in `client_go_rate_limiter_duration_seconds`. Use `client_go_request_total`, not `rest_client_requests_total`,
for API server errors, because Karpenter doesn't populate the latter.

---

## 9. Cloud provider signals

Core Karpenter wraps every call to the cloud provider and reports it in two metrics, whatever the provider.
`karpenter_cloudprovider_duration_seconds` records how long each call took, labelled by `method` (`Create`, `Delete`,
`Get`, `List`, `GetInstanceTypes`, `IsDrifted`) and `provider`. `karpenter_cloudprovider_errors_total` counts errors by
`method` and `error`. Section 6 uses both for launches.

`NodeClaimNotFoundError` on `Get` and `Delete` is normal while nodes scale in, so leave it out of error ratios. The
duration histogram's largest bucket is 10 seconds, so slow `Create` calls land in `+Inf` and quantiles above that are
not meaningful.

Interruption handling, API throttling and instance launch failure reasons are provider-specific. Look them up in your
provider's metrics reference. For AWS, that is the [metrics reference](https://karpenter.sh/docs/reference/metrics/)
and the interruption section of the [disruption docs](https://karpenter.sh/docs/concepts/disruption/#interruption).

---

## 10. Gaps

Some signals that would make these alerts simpler or more precise have no metric today. They are candidates for
follow-up issues.

Pods deleted because a node's `terminationGracePeriod` elapsed are counted in `karpenter_pods_drained_total` with the
same labels as normal evictions, so you can't tell them apart. A counter for deletions by deadline, by NodePool, would
make the most useful alert in Section 7 direct.

Disruption candidates skipped because of PDBs, `karpenter.sh/do-not-disrupt` or an exhausted budget only show up as
events. A counter by blocking reason, and how long each NodePool has been blocked, would replace the gauge arithmetic
in `KarpenterDisruptionBudgetExhausted`.

`karpenter_scheduler_unschedulable_pods_count` has no reason or NodePool label, and nothing records how long pods stay
unschedulable. A NodePool that hits `spec.limits` produces no metric at all.

Karpenter has no histogram from NodeClaim creation to node ready of its own. The `operator_nodeclaim_` transition
histograms are best effort, reset on leader change, and disappear with `--disable-cluster-state-observability`. Nothing
links a pending pod to the NodeClaim launched for it.

The pod batching window (`BATCH_IDLE_DURATION` and `BATCH_MAX_DURATION`) and the time from a disruption decision to the
replacement being ready have no metric. Only disruption failures are counted.

Drift has no reason label, so you can't tell an image change from a NodePool change without reading each NodeClaim.

Leader election exposes only `leader_election_master_status`. There is no count of leader changes or lease renewal
latency.

`karpenter_cloudprovider_duration_seconds` tops out at 10 seconds, and `karpenter_cloudprovider_errors_total` reports
throttling as `unknown`.

---

## Appendix A: example manifests

### A.1 karpenter-alerts.yaml

All the alerts and the recording rule in this guide in one `PrometheusRule`. Adjust the `job`, `namespace` and
`container` matchers and the `terminationGracePeriod` threshold to match your install.

```yaml
apiVersion: monitoring.coreos.com/v1
kind: PrometheusRule
metadata:
  name: karpenter-core
spec:
  groups:
    - name: karpenter.core
      rules:
        - alert: KarpenterClusterStateUnsynced
          expr: max(karpenter_cluster_state_unsynced_time_seconds) > 300
          for: 5m
          labels: { severity: page }
          annotations:
            summary: Karpenter cluster state has not synced for over 5 minutes, so provisioning and disruption are paused.
        - alert: KarpenterPodsWaitingForCapacity
          expr: max(karpenter_pods_provisioning_unbound_time_seconds) > 900
          for: 5m
          labels: { severity: page }
          annotations:
            summary: A pod Karpenter is provisioning for has waited more than 15 minutes for a node.
        - alert: KarpenterNoLeader
          expr: sum(leader_election_master_status{job="karpenter", name="karpenter-leader-election"}) < 1 or absent(leader_election_master_status{job="karpenter"})
          for: 5m
          labels: { severity: page }
          annotations:
            summary: No Karpenter replica holds the leader lease, so nothing is being provisioned or disrupted.
        - alert: KarpenterNodeRegistrationUnhealthy
          expr: max by (name) (operator_nodepool_status_condition_count{type="NodeRegistrationHealthy", status="False"}) > 0
          for: 15m
          labels: { severity: page }
          annotations:
            summary: Nodes launched for NodePool {{ $labels.name }} are failing to join the cluster.
        - alert: KarpenterControllerHung
          expr: max by (controller) (workqueue_unfinished_work_seconds{job="karpenter", controller=~"provisioner|disruption|disruption.queue|eviction-queue|node.termination|nodeclaim.lifecycle"}) > 900
          for: 15m
          labels: { severity: page }
          annotations:
            summary: Karpenter controller {{ $labels.controller }} has had work in progress for over 15 minutes.
    - name: karpenter.provisioning
      rules:
        - alert: KarpenterPodsUnschedulable
          expr: min_over_time(karpenter_scheduler_unschedulable_pods_count{controller="provisioner"}[15m]) > 0
          for: 15m
          labels: { severity: ticket }
          annotations:
            summary: Karpenter has found pods no NodePool can satisfy for over 30 minutes.
        - alert: KarpenterPodsUndecided
          expr: max(karpenter_pods_provisioning_scheduling_undecided_time_seconds) > 300
          for: 5m
          labels: { severity: ticket }
          annotations:
            summary: Karpenter has seen a pending pod but not decided where to place it for over 5 minutes.
        - alert: KarpenterNodePoolNearLimit
          expr: max by (nodepool, resource_type) (karpenter_nodepools_usage / karpenter_nodepools_limit) > 0.9
          for: 15m
          labels: { severity: ticket }
          annotations:
            summary: NodePool {{ $labels.nodepool }} is above 90% of its {{ $labels.resource_type }} limit.
    - name: karpenter.launch
      rules:
        - record: karpenter:nodeclaim_initialized_seconds:p95
          expr: histogram_quantile(0.95, sum by (le) (rate(operator_nodeclaim_status_condition_transition_seconds_bucket{type="Initialized", status="Unknown", to_status="True"}[30m])))
        - alert: KarpenterNodeClaimsSlowToInitialize
          expr: |
            karpenter:nodeclaim_initialized_seconds:p95 > 600
            and on() sum(increase(operator_nodeclaim_status_condition_transition_seconds_count{type="Initialized", status="Unknown", to_status="True"}[30m])) >= 5
          for: 15m
          labels: { severity: ticket }
          annotations:
            summary: The 95th percentile time from NodeClaim creation to initialized is over 10 minutes.
        - alert: KarpenterNodeClaimLivenessTimeouts
          expr: sum by (nodepool, reason) (increase(karpenter_nodeclaims_disrupted_total{reason=~"launch_timeout|registration_timeout"}[30m])) > 0
          labels: { severity: ticket }
          annotations:
            summary: Karpenter deleted NodeClaims in NodePool {{ $labels.nodepool }} for {{ $labels.reason }}.
        - alert: KarpenterLaunchErrorRatio
          expr: |
            sum(rate(karpenter_cloudprovider_errors_total{method="Create", error!="InsufficientCapacityError"}[15m]))
              / sum(rate(karpenter_cloudprovider_duration_seconds_count{method="Create"}[15m])) > 0.2
            and on() sum(increase(karpenter_cloudprovider_duration_seconds_count{method="Create"}[15m])) >= 10
          for: 15m
          labels: { severity: ticket }
          annotations:
            summary: More than 20% of cloud provider launch calls are failing for reasons other than capacity.
        - alert: KarpenterInsufficientCapacity
          expr: sum by (nodepool) (increase(karpenter_nodeclaims_disrupted_total{reason="insufficient_capacity"}[30m])) > 10
          for: 15m
          labels: { severity: ticket }
          annotations:
            summary: NodePool {{ $labels.nodepool }} keeps hitting insufficient capacity.
    - name: karpenter.disruption
      rules:
        - alert: KarpenterNodeClaimStuckTerminating
          expr: max by (name, karpenter_sh_nodepool) (operator_nodeclaim_termination_current_time_seconds) > 3600
          for: 15m
          labels: { severity: ticket }
          annotations:
            summary: NodeClaim {{ $labels.name }} has been terminating for over an hour.
        - alert: KarpenterEvictionsBlockedByPDB
          expr: |
            sum(increase(karpenter_pods_eviction_requests_total{code="429"}[30m])) > 0
            and on() max(operator_nodeclaim_termination_current_time_seconds) > 1800
          for: 30m
          labels: { severity: ticket }
          annotations:
            summary: PodDisruptionBudgets are blocking Karpenter from draining a node.
        - alert: KarpenterDisruptionBudgetExhausted
          expr: |
            max by (nodepool, reason) (karpenter_nodepools_allowed_disruptions - karpenter_nodepools_nodes_consuming_budgets) <= 0
            and on(reason) max by (reason) (karpenter_voluntary_disruption_eligible_nodes) > 0
          for: 6h
          labels: { severity: ticket }
          annotations:
            summary: NodePool {{ $labels.nodepool }} has had no {{ $labels.reason }} disruption budget for 6 hours while nodes are eligible.
        - alert: KarpenterDisruptionQueueFailures
          expr: sum by (reason, decision) (increase(karpenter_voluntary_disruption_queue_failures_total[1h])) > 0
          labels: { severity: ticket }
          annotations:
            summary: Karpenter abandoned {{ $labels.reason }} disruptions in the last hour.
        - alert: KarpenterDisruptionChurn
          expr: |
            sum(increase(karpenter_nodeclaims_disrupted_total{reason=~"empty|underutilized|drifted"}[1h]))
              / max(karpenter_cluster_state_node_count) > 0.25
          for: 1h
          labels: { severity: ticket }
          annotations:
            summary: Karpenter disrupted more than a quarter of the cluster's nodes in the last hour.
        - alert: KarpenterTerminationGracePeriodNearlyElapsed
          expr: max by (name, karpenter_sh_nodepool) (operator_nodeclaim_termination_current_time_seconds) > 0.9 * 86400
          labels: { severity: ticket }
          annotations:
            summary: NodeClaim {{ $labels.name }} is close to its terminationGracePeriod, after which Karpenter deletes its pods regardless of PDBs.
    - name: karpenter.controller
      rules:
        - alert: KarpenterRestarting
          expr: sum(increase(kube_pod_container_status_restarts_total{container="controller", namespace="karpenter"}[1h])) > 2
          labels: { severity: ticket }
          annotations:
            summary: The Karpenter controller restarted more than twice in the last hour.
        - alert: KarpenterOOMKilled
          expr: max(kube_pod_container_status_last_terminated_reason{container="controller", namespace="karpenter", reason="OOMKilled"}) > 0
          labels: { severity: ticket }
          annotations:
            summary: The Karpenter controller was last terminated for running out of memory.
        - alert: KarpenterReconcileErrorRatio
          expr: |
            sum by (controller) (rate(controller_runtime_reconcile_errors_total{job="karpenter"}[15m]))
              / sum by (controller) (rate(controller_runtime_reconcile_total{job="karpenter"}[15m])) > 0.1
            and sum by (controller) (increase(controller_runtime_reconcile_total{job="karpenter"}[15m])) >= 20
          for: 15m
          labels: { severity: ticket }
          annotations:
            summary: Karpenter controller {{ $labels.controller }} is failing more than 10% of reconciles.
        - alert: KarpenterReconcilePanics
          expr: sum by (controller) (increase(controller_runtime_reconcile_panics_total{job="karpenter"}[30m])) > 0
          labels: { severity: ticket }
          annotations:
            summary: Karpenter controller {{ $labels.controller }} panicked.
        - alert: KarpenterKubeAPIThrottled
          expr: sum(rate(client_go_request_total{job="karpenter", code="429"}[5m])) > 0
          for: 15m
          labels: { severity: ticket }
          annotations:
            summary: The Kubernetes API server is rate limiting Karpenter.
```
