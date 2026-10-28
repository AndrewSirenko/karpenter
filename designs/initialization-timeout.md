# NodeClaim Initialization Timeout

## Motivation

Karpenter bounds every phase of a NodeClaim's startup except the last one. The liveness controller deletes a NodeClaim that doesn't launch within 5 minutes or register within 15 minutes, and the reboot controller fails a reboot that doesn't recover within its observation window. Nothing bounds initialization. A node that registers but never becomes `Initialized` stays that way until `expireAfter` fires, which defaults to 30 days and is often set to `Never`.

Disruption skips uninitialized nodes, so consolidation and drift won't touch the node either. The pending pods that caused the launch can't use it, and Karpenter won't launch for them again because it counts the stuck NodeClaim as in-flight capacity. The only way out is a human deleting the NodeClaim.

Initialization usually blocks on something Karpenter doesn't own: a DaemonSet that removes a startup taint, a device plugin, or a DRA driver. When one of those fails, the right fix is in that component. The user who gets paged is rarely the one who can ship it, though, and the mitigation they reach for is the same every time: delete the NodeClaim and let Karpenter try again. Some of these failures are transient or specific to a host or instance type, and a retry clears them. The ones that aren't should be loud, not silent.

This RFC proposes an opt-in, per-NodePool `initializationTimeout` that bounds initialization the same way launch and registration are already bounded. See [#3242](https://github.com/kubernetes-sigs/karpenter/issues/3242).

### Use Cases

1. **Startup taint never removed.** A DaemonSet sizes hugepages from pod requests, restarts kubelet, then removes a startup taint. If it fails, the taint stays and the node never initializes (from #3242).
2. **DRA driver never publishes.** Pods request a ResourceClaim. The node goes Ready, but the driver never publishes a complete ResourceSlice pool for it, so the NodeClaim sits at `Initialized=Unknown, Reason=DRADriverPoolsNotPublished`.
3. **Device plugin never registers.** A GPU node's device plugin crashes before it advertises `nvidia.com/gpu`, leaving `Reason=ResourceNotRegistered`.
4. **Node never re-initializes after a reboot.** Node repair reboots a node. It comes back with a new boot ID and the reboot succeeds, but a driver doesn't come back up, so `Initialized` stays `Unknown` forever.

### Non-Goals

- **Finding the root cause.** Karpenter surfaces the last `Initialized` reason and message. It doesn't diagnose them.
- **Timeouts on individual startup taints.** A per-taint timeout belongs upstream in Kubernetes. It also wouldn't cover cases 2 through 4.
- **Scheduling around a broken DRA driver.** Letting pods that don't need a driver use a node where that driver failed is a separate improvement. It shrinks the blast radius but doesn't free the stuck node for the pods that do need it.
- **Provisioning while a node reboots.** How long Karpenter waits before launching capacity alongside a rebooting node ([#3464](https://github.com/kubernetes-sigs/karpenter/issues/3464)) is a different action (launch, not delete). See Alternatives.

## Proposal

### Proposed Spec

```yaml
apiVersion: karpenter.sh/v1
kind: NodePool
spec:
  template:
    spec:
      expireAfter: Never
      # Delete and replace a NodeClaim whose node registers but doesn't
      # initialize within this duration. Unset or Never keeps today's behavior.
      initializationTimeout: 1h
```

```go
type NodeClaimTemplateSpec struct {
	// ...
	// +kubebuilder:validation:Pattern=`^(([0-9]+(s|m|h))+|Never)$`
	// +kubebuilder:validation:Type="string"
	// +kubebuilder:validation:Schemaless
	// +optional
	InitializationTimeout NillableDuration `json:"initializationTimeout,omitempty" hash:"ignore"`
}
```

The field is copied onto `NodeClaimSpec` like `expireAfter` and `terminationGracePeriod`, so standalone NodeClaims can set it too. It has no default.

### How It Works

The liveness controller already reconciles every NodeClaim and stops caring once `Registered=True`. With this change, a registered NodeClaim keeps going through liveness until it initializes:

1. If `initializationTimeout` is unset or `Never`, or `Initialized=True`, do nothing.
2. If `Rebooting=True`, do nothing. The reboot controller bounds an in-flight reboot.
3. Start the clock when the current boot started initializing. That is the latest of `Registered`'s transition, `Initialized`'s last transition to `Unknown`, and `Rebooting`'s transition to a terminal outcome. For a fresh node this is registration. After a reboot it's when the reboot finished.
4. If the timeout hasn't passed, requeue for the remainder. Otherwise delete the NodeClaim.

Every timestamp is in status conditions, so a controller restart doesn't reset the clock. Deletion goes through normal termination: cordon, drain honoring PDBs and `karpenter.sh/do-not-disrupt` up to `terminationGracePeriod`, then terminate the instance. The pending pods go back through normal provisioning, which launches a replacement.

### Interaction with Existing Features

- **`expireAfter`** is unchanged and independent. Whichever fires first wins.
- **Launch and registration timeouts** are unchanged. The initialization clock starts at registration, so the three don't overlap.
- **Reboot.** An in-flight reboot is never interrupted. After a reboot finishes, the node gets a full `initializationTimeout` to re-initialize.
- **Disruption budgets** don't apply, matching the launch and registration timeouts. Uninitialized nodes are never disruption candidates, so a budget would block the only path that frees them.
- **Drift.** The field is `hash:"ignore"`. Changing it doesn't drift healthy nodes, which it would never affect. The new value applies to NodeClaims created after the change.
- **`NodeRegistrationHealthy`** is unchanged. An initialization timeout isn't a registration failure.

### Observability

- **Metric.** `karpenter_nodeclaims_disrupted_total{reason="initialization_timeout"}`, labeled by NodePool and capacity type, next to `launch_timeout` and `registration_timeout`.
- **Event.** A Warning `InitializationTimeout` event on the NodeClaim carrying the last `Initialized` reason and message, e.g. `Node did not initialize within 1h0m0s, replacing NodeClaim (last reason DRADriverPoolsNotPublished: DRA driver "example.com/driver" has not published a complete resource pool)`.
- **Log.** The existing `terminating due to timeout` line with `reason=initialization_timeout`.

### Edge Cases

- **Systematic failure.** If every node in a pool hits the same bug, each NodeClaim waits the full timeout before it's replaced, so churn is bounded to one replacement per stuck NodeClaim per timeout. The metric and event make this visible per NodePool. Open question 3 covers going further.
- **Timeout shorter than normal startup.** Some pools take a long time to initialize, for example GPU driver installs. A timeout below that would churn healthy nodes. The docs should recommend a value well above the pool's normal initialization time; open question 5 covers a minimum.
- **Pods already on the node.** Nodes stuck on `ResourceNotRegistered` or `DRADriverPoolsNotPublished` are Ready and untainted, so pods that don't need the missing resource can land there. They are drained like any other deletion.
- **Already-stuck nodes.** Setting the field on a NodePool doesn't rescue NodeClaims that already exist. Open question 4.

## Alternatives Considered

### Alternative 1: A general `giveUpOnNodeClaimAfter`

One knob that bounds how long a NodeClaim can be unusable for any reason: not launched, not registered, not initialized, or rebooting.

Most of that is already bounded. Launch and registration have liveness timeouts, and a reboot that doesn't recover within its observation window is failed and the NodeClaim replaced. The open reboot gap in #3464 is how long to suppress provisioning while a node reboots. The action there is to launch capacity alongside the node, not to give up on it, and folding it into this knob would tie two different actions to one number.

The one reboot case that does leave a stuck NodeClaim is a reboot that succeeds and then never re-initializes (use case 4). This proposal covers it because the clock restarts when a reboot finishes. If a general knob is still wanted later, `initializationTimeout` can be its first member.

### Alternative 2: Timeouts on startup taints

A taint carries its own deadline and is removed or acted on when it passes. This only covers use case 1. DRA pools, extended resources, and `NodeNotReady` aren't taints. It also needs an upstream Kubernetes change.

### Alternative 3: A hardcoded constant like `registrationTimeout`

Normal initialization time varies by pool far more than registration does, from seconds on a plain node to many minutes for GPU drivers or a kubelet restart. One global value would either be too long to help or short enough to churn healthy slow pools, and turning it on would change behavior for every user.

### Alternative 4: Make uninitialized nodes disruption candidates

Let drift, consolidation, or repair act on uninitialized nodes so budgets apply. Disruption simulations assume a candidate's capacity is real and schedulable, which an uninitialized node's isn't. This is a larger change, and it still needs a per-pool notion of how long "too long" is.

### Alternative 5: Read the timeout from the NodePool at reconcile time

Liveness could look up the owning NodePool instead of the NodeClaim spec, so changing the NodePool immediately rescues existing stuck NodeClaims. It would be the only lifecycle field that works this way, and standalone NodeClaims couldn't use it. Open question 4.

## Backward Compatibility

The field is optional with no default, so existing NodePools behave exactly as today. It's excluded from the drift hash, so upgrading Karpenter or setting it doesn't drift nodes. Both CRDs gain a field, and providers vendoring them need to regenerate.

## Graduation Criteria

No feature gate. The field is opt-in and does nothing until set.

## Open Questions

1. **Default.** Unset keeps today's behavior and is proposed for now. A large finite default (for example `4h`) would rescue users who never learn the field exists. Revisit once the metric shows how long real initializations take.
2. **Budgets.** Should initialization timeouts count against disruption budgets? Proposed no, matching launch and registration.
3. **Repeated failures.** Should repeated timeouts in a NodePool set a status condition (or extend `NodeRegistrationHealthy`), or back off launches into that pool, so a systematic failure doesn't turn into steady churn?
4. **Existing NodeClaims.** Should liveness also pick up the NodePool's current value (Alternative 5) so the field can rescue NodeClaims that are already stuck?
5. **Minimum value.** Should validation reject very small values, for example under `5m`?
6. **Name.** `initializationTimeout` matches the issue and the internal launch and registration timeouts. `initializeWithin` would read closer to `expireAfter` and `consolidateAfter`.
