package a

import "sigs.k8s.io/karpenter/pkg/controllers/state"

// Shape of kubernetes-sigs/karpenter#3176.
func provision(c *state.Cluster, nc *state.NodeClaim) {
	c.UpdateNodeClaim(nc) // want `Cluster.UpdateNodeClaim writes cluster state outside the informer controllers`
}

// Shape of kubernetes-sigs/karpenter#3250.
func markDisrupted(c *state.Cluster, nc *state.NodeClaim) {
	c.NodePoolState.MarkNodeClaimPendingDisruption("np", nc.Name) // want `NodePoolState.MarkNodeClaimPendingDisruption writes cluster state`
}

func acknowledgedSeed(c *state.Cluster, nc *state.NodeClaim) {
	//karpenter:state-seed make the launch visible before the informer catches up
	c.UpdateNodeClaim(nc)
	c.UpdateNodeClaim(nc) //karpenter:state-seed same line
}

func reads(c *state.Cluster, nc *state.NodeClaim) {
	_ = c.NodeClaimExists(nc.Name)
	_ = c.NodePoolState.ReserveNodeCount("np", 1, 1)
	c.MarkForDeletion("id")
}
