package state

type NodeClaim struct{ Name string }

type NodePoolState struct{}

func (n *NodePoolState) MarkNodeClaimPendingDisruption(npName, ncName string) {}
func (n *NodePoolState) ReserveNodeCount(np string, limit, wanted int64) int64 { return 0 }
func (n *NodePoolState) UpdateNodeClaim(nc *NodeClaim, markedForDeletion bool) {}

type Cluster struct{ NodePoolState *NodePoolState }

func (c *Cluster) UpdateNodeClaim(nc *NodeClaim) {}
func (c *Cluster) MarkForDeletion(ids ...string) {}
func (c *Cluster) NodeClaimExists(name string) bool { return false }

// The informer controllers own state, so writes in this package are allowed.
func (c *Cluster) informerReconcile(nc *NodeClaim) {
	c.UpdateNodeClaim(nc)
	c.NodePoolState.UpdateNodeClaim(nc, false)
}
