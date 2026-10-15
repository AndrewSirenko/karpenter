#!/usr/bin/env bash
# Usage: run.sh <scenario> [path-regex] [delay]
# Existing kind cluster (KCFG), state reset per run. kwok Karpenter runs locally behind a proxy that delays GETs on one resource.
set -uo pipefail

SCENARIO=$1
MATCH=${2:-}
DELAY=${3:-0s}
RUN_FOR=${RUN_FOR:-240}
FEATURE_GATES=${FEATURE_GATES:-PredictionEnabled=true}
REPO=$(cd "$(dirname "$0")/../.." && pwd)
OUT=${OUT:-$REPO/hack/startup-deps-repro/out}/$SCENARIO
KCFG=${KCFG:?set KCFG to the kind cluster kubeconfig}
PCFG=$OUT/proxy.kubeconfig
VPA_CRD=${VPA_CRD:-$(go env GOMODCACHE)/k8s.io/autoscaler/vertical-pod-autoscaler@v1.7.0/deploy/vpa-v1-crd-gen.yaml}

rm -rf "$OUT" && mkdir -p "$OUT"
export GOFLAGS=-mod=mod GOPROXY=off GOSUMDB=off
go build -C "$REPO" -o "$OUT/karpenter" ./kwok || exit 1
go build -C "$REPO" -o "$OUT/proxy" ./hack/startup-deps-repro/proxy || exit 1

k() { kubectl --kubeconfig "$KCFG" "$@"; }
k delete deployment inflate --ignore-not-found --wait=false >/dev/null
for r in nodeclaims nodes; do
  for o in $(k get $r -l karpenter.sh/nodepool -o name 2>/dev/null); do
    k patch $o --type=merge -p '{"metadata":{"finalizers":null}}' >/dev/null 2>&1
    k delete $o --ignore-not-found --wait=false >/dev/null 2>&1
  done
done
k delete nodepool default --ignore-not-found >/dev/null 2>&1
k delete lease -n kube-system karpenter-leader-election --ignore-not-found >/dev/null 2>&1
k apply -f "$REPO/kwok/charts/crds" -f "$VPA_CRD" >/dev/null
k wait --for condition=established crd --all --timeout=60s >/dev/null
k apply -f "$REPO/test/pkg/environment/common/default_kowknodeclass.yaml" -f "$REPO/test/pkg/environment/common/default_nodepool.yaml" >/dev/null
k patch kwoknodeclass default --subresource=status --type=merge -p \
  '{"status":{"conditions":[{"type":"Ready","status":"True","reason":"Ready","message":"","lastTransitionTime":"2026-01-01T00:00:00Z"}]}}' >/dev/null
k create deployment inflate --image=registry.k8s.io/pause:3.10 --replicas=1 >/dev/null
k patch deployment inflate --type=merge -p \
  '{"spec":{"template":{"spec":{"nodeSelector":{"karpenter.sh/nodepool":"default"},"containers":[{"name":"pause","image":"registry.k8s.io/pause:3.10","resources":{"requests":{"cpu":"1"}}}]}}}}' >/dev/null

cat >"$PCFG" <<EOF
apiVersion: v1
kind: Config
clusters: [{name: p, cluster: {server: "http://127.0.0.1:18443"}}]
contexts: [{name: p, context: {cluster: p, user: u}}]
users: [{name: u, user: {}}]
current-context: p
EOF

"$OUT/proxy" -kubeconfig "$KCFG" -match "${MATCH:-^$}" -delay "$DELAY" >"$OUT/proxy.log" 2>&1 &
PROXY=$!
sleep 1

START=$(date +%s)
KUBECONFIG=$PCFG SYSTEM_NAMESPACE=kube-system LEADER_ELECTION_NAMESPACE=kube-system FEATURE_GATES=$FEATURE_GATES \
  METRICS_PORT=18080 HEALTH_PROBE_PORT=18081 LOG_LEVEL=info \
  "$OUT/karpenter" >"$OUT/karpenter.log" 2>&1 &
KARP=$!

{
  echo "scenario=$SCENARIO match=${MATCH:-none} delay=$DELAY feature_gates=$FEATURE_GATES"
  while :; do
    t=$(($(date +%s) - START))
    alive=yes; kill -0 $KARP 2>/dev/null || alive=no
    ready=$(curl -s -o /dev/null -w '%{http_code}' --max-time 2 http://127.0.0.1:18081/readyz)
    ncs=$(k get nodeclaims --no-headers 2>/dev/null | wc -l)
    echo "t=${t}s alive=$alive readyz=$ready nodeclaims=$ncs"
    [ "$alive" = no ] && { wait $KARP; echo "t=${t}s exit_code=$?"; break; }
    [ "$t" -ge "$RUN_FOR" ] && break
    sleep 5
  done
} > >(tee "$OUT/timeline.log")

kill $KARP $PROXY 2>/dev/null
wait 2>/dev/null

echo "--- key log lines"
grep -E 'timed out waiting|did not sync|panic|prediction store|found provisionable|created nodeclaim|leader|"level":"ERROR"' "$OUT/karpenter.log" \
  | cut -c1-400 | head -40 | tee "$OUT/key.log"
