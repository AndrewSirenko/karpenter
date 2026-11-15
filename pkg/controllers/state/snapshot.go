/*
Copyright The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package state

import (
	"slices"
	"strings"

	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"

	"sigs.k8s.io/karpenter/pkg/scheduling"
)

// NodeSnapshot is a point-in-time deep copy of the cluster's nodes plus values derived from each node.
//
// A snapshot is immutable once returned and is shared by every caller of SnapshotNodes until cluster state changes, so
// it is safe for any number of concurrent readers. Callers must not mutate the nodes, the NodeInfo values, or anything
// reachable from them; copy first (e.g. StateNode.DeepCopy) if a mutable node is needed.
type NodeSnapshot struct {
	version uint64
	nodes   StateNodes
	entries map[string]*snapshotEntry
	info    map[*StateNode]*NodeInfo
}

type snapshotEntry struct {
	src  *StateNode
	gen  uint64
	node *StateNode
	info *NodeInfo
}

// NodeInfo holds values the scheduler derives from a StateNode. All fields are read-only.
type NodeInfo struct {
	Name        string
	HostName    string
	Initialized bool
	// MarkedForDeletion is StateNode.MarkedForDeletion().
	MarkedForDeletion bool
	Taints            []corev1.Taint
	// LabelRequirements is scheduling.NewLabelRequirements(Labels()).
	LabelRequirements scheduling.Requirements
	// NodeLabelRequirements is scheduling.NewLabelRequirements(Node.Labels), nil without a Node.
	NodeLabelRequirements scheduling.Requirements
	// Requirements is LabelRequirements plus hostname In HostName().
	Requirements      scheduling.Requirements
	Available         corev1.ResourceList
	Capacity          corev1.ResourceList
	DaemonSetRequests corev1.ResourceList
}

// NewNodeInfo computes the NodeInfo for n.
func NewNodeInfo(n *StateNode) *NodeInfo {
	reqs := scheduling.NewLabelRequirements(n.Labels())
	reqs.Add(scheduling.NewRequirement(corev1.LabelHostname, corev1.NodeSelectorOpIn, n.HostName()))
	var nodeReqs scheduling.Requirements
	if n.Node != nil {
		nodeReqs = scheduling.NewLabelRequirements(n.Node.Labels)
	}
	return &NodeInfo{
		NodeLabelRequirements: nodeReqs,
		Name:                  n.Name(),
		HostName:              n.HostName(),
		Initialized:           n.Initialized(),
		MarkedForDeletion:     n.MarkedForDeletion(),
		Taints:                n.Taints(),
		LabelRequirements:     scheduling.NewLabelRequirements(n.Labels()),
		Requirements:          reqs,
		Available:             n.Available(),
		Capacity:              n.Capacity(),
		DaemonSetRequests:     n.DaemonSetRequests(),
	}
}

// Nodes returns the snapshot's nodes, initialized first and then by name. The slice is the caller's; the nodes are not.
func (s *NodeSnapshot) Nodes() StateNodes {
	return slices.Clone(s.nodes)
}

// Active returns Nodes().Active() without re-evaluating each node.
func (s *NodeSnapshot) Active() StateNodes {
	return lo.Filter(s.nodes, func(n *StateNode, _ int) bool { return !s.info[n].MarkedForDeletion })
}

// Deleting returns Nodes().Deleting() without re-evaluating each node.
func (s *NodeSnapshot) Deleting() StateNodes {
	return lo.Filter(s.nodes, func(n *StateNode, _ int) bool { return s.info[n].MarkedForDeletion })
}

// Info returns the derived values for a node of this snapshot, or nil if n is not one of its nodes.
func (s *NodeSnapshot) Info(n *StateNode) *NodeInfo {
	if s == nil {
		return nil
	}
	return s.info[n]
}

// SnapshotNodes returns a read-only deep copy of the cluster's nodes. While nothing has written to cluster state since the
// last call, it returns the same snapshot. Otherwise it builds a new one, deep-copying only the nodes written since the
// previous snapshot and reusing the rest.
func (c *Cluster) SnapshotNodes() *NodeSnapshot {
	c.mu.RLock()
	defer c.mu.RUnlock()
	c.snapshotMu.Lock()
	defer c.snapshotMu.Unlock()

	prev := c.snapshot
	if prev != nil && prev.version == c.nodesVersion {
		return prev
	}
	next := &NodeSnapshot{
		version: c.nodesVersion,
		nodes:   make(StateNodes, 0, len(c.nodes)),
		entries: make(map[string]*snapshotEntry, len(c.nodes)),
		info:    make(map[*StateNode]*NodeInfo, len(c.nodes)),
	}
	for id, n := range c.nodes {
		var e *snapshotEntry
		if prev != nil {
			if old, ok := prev.entries[id]; ok && old.src == n && n.gen != 0 && old.gen == n.gen {
				e = old
			}
		}
		if e == nil {
			cp := n.DeepCopy()
			e = &snapshotEntry{src: n, gen: n.gen, node: cp, info: NewNodeInfo(cp)}
		}
		next.entries[id] = e
		next.nodes = append(next.nodes, e.node)
		next.info[e.node] = e.info
	}
	sortNodes(next.nodes, func(n *StateNode) *NodeInfo { return next.info[n] })
	c.snapshot = next
	return next
}

// SortNodes orders nodes the way NodeSnapshot.Nodes does: initialized first, then by name, then by provider id.
func SortNodes(nodes StateNodes) {
	sortNodes(nodes, NewNodeInfo)
}

func sortNodes(nodes StateNodes, info func(*StateNode) *NodeInfo) {
	keys := make(map[*StateNode]*NodeInfo, len(nodes))
	for _, n := range nodes {
		keys[n] = info(n)
	}
	slices.SortFunc(nodes, func(a, b *StateNode) int {
		ia, ib := keys[a], keys[b]
		if ia.Initialized != ib.Initialized {
			if ia.Initialized {
				return -1
			}
			return 1
		}
		if r := strings.Compare(ia.Name, ib.Name); r != 0 {
			return r
		}
		return strings.Compare(a.ProviderID(), b.ProviderID())
	})
}

// touch stamps n as written in the current write section. Call it, holding mu, for every in-place write to a node.
func (c *Cluster) touch(n *StateNode) {
	n.gen = c.nodesVersion
}
