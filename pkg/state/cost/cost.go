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

package cost

import (
	"context"
	"fmt"
	"maps"
	"sync"

	opmetrics "github.com/awslabs/operatorpkg/metrics"
	"github.com/awslabs/operatorpkg/serrors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/metrics"
)

// NecessaryLabels defines the set of required Kubernetes labels that must be present
// on NodeClaim objects for cost tracking to function properly.
var NecessaryLabels = []string{corev1.LabelInstanceTypeStable, v1.CapacityTypeLabelKey, corev1.LabelTopologyZone, v1.NodePoolLabelKey}

var (
	CostTrackingErrorsTotal = opmetrics.NewPrometheusCounter(
		crmetrics.Registry,
		prometheus.CounterOpts{
			Namespace: metrics.Namespace,
			Subsystem: metrics.NodePoolSubsystem,
			Name:      "cost_tracker_errors_total",
			Help:      "Number of errors encountered during cost tracking operations. Labeled by nodepool.",
		},
		[]opmetrics.Label{
			metrics.NodePool,
		},
		opmetrics.Alpha,
	)
)

// ClusterCost tracks the cost of compute resources across all NodePools in a cluster.
// This is an alpha-level component and its API may change without notice.
//
// The ClusterCost maintains real-time cost information by:
// - Tracking NodeClaim additions and removals
// - Managing instance type offerings and their prices
// - Calculating aggregate costs per NodePool and cluster-wide
//
// All operations are thread-safe through internal locking mechanisms.
type ClusterCost struct {
	sync.RWMutex
	npCostMap map[string]*NodePoolCost // nodepool.Name -> NodePoolCost
	// nodeClaimMap tracks which NodeClaims are currently being monitored for cost
	nodeClaimMap map[types.NamespacedName]*NodeClaimMetaData // nodeClaim object key -> NodeClaimMetaData

	cloudProvider cloudprovider.CloudProvider
	client        client.Client
}

// NodePoolCost represents the cost tracking information for a single NodePool.
// It maintains the current cost, the instance types its NodeClaims are priced with, and each NodeClaim's price.
type NodePoolCost struct {
	cost float64
	// instanceTypes are the instance types of the NodePool's tracked NodeClaims by name, refreshed when its offerings
	// are updated
	instanceTypes map[string]*trackedInstanceType
	// nodeClaims are the NodePool's tracked NodeClaims and their prices
	nodeClaims map[types.NamespacedName]*NodeClaimMetaData
}

// OfferingKey uniquely identifies a specific compute offering by its zone,
// capacity type (e.g., spot/on-demand), and instance type name.
// This is not a hard invariant
type OfferingKey struct {
	Zone, CapacityType, InstanceName string
}

// OfferingCount tracks the number and cost of instances for a specific offering.
//
// Deprecated: cost is tracked per NodeClaim. See NodeClaimMetaData.
type OfferingCount struct {
	Count int
	Price float64 // Price of the offering, not Price * count
}

// trackedInstanceType is an instance type and the number of tracked NodeClaims priced with it.
type trackedInstanceType struct {
	instanceType *cloudprovider.InstanceType
	count        int
}

type NodeClaimMetaData struct {
	NodePoolName string
	NodeClaimKey OfferingKey
	// Price is the NodeClaim's hourly price from the NodeClaimPriceProvider, or 0 if it's unknown.
	Price float64

	// labels and annotations are the tracked NodeClaim's, kept to reprice it when its NodePool's offerings change
	labels, annotations map[string]string
}

// NewClusterCost creates and initializes a new ClusterCost instance for tracking
// compute costs across the cluster. It requires a cloud provider for accessing
// instance type and pricing information, and a Kubernetes client for NodePool lookups.
func NewClusterCost(ctx context.Context, cloudProvider cloudprovider.CloudProvider, client client.Client) *ClusterCost {
	return &ClusterCost{
		npCostMap:     make(map[string]*NodePoolCost),
		nodeClaimMap:  make(map[types.NamespacedName]*NodeClaimMetaData),
		cloudProvider: cloudProvider,
		client:        client,
	}
}

// UpdateOfferings updates the available instance types and their pricing information
// for a specific NodePool. This method is typically called when NodePool configurations
// change or when cloud provider pricing information is refreshed.
//
// Returns an error if instance type information cannot be updated or if cost
// recalculation fails.
func (cc *ClusterCost) UpdateOfferings(ctx context.Context, np *v1.NodePool, instanceTypes []*cloudprovider.InstanceType) {
	cc.Lock()
	defer cc.Unlock()
	cc.internalUpdateOfferings(ctx, np, instanceTypes)
}

func (cc *ClusterCost) internalUpdateOfferings(ctx context.Context, np *v1.NodePool, instanceTypes []*cloudprovider.InstanceType) {
	npCost, exists := cc.npCostMap[np.Name]
	if !exists {
		cc.npCostMap[np.Name] = newNodePoolCost()
		return
	}
	for _, it := range instanceTypes {
		if it == nil {
			continue
		}
		if tracked, ok := npCost.instanceTypes[it.Name]; ok {
			tracked.instanceType = it
		}
	}

	// Reprice tracked NodeClaims and recalculate cost. A NodeClaim whose price can't be found keeps its last price.
	cost := 0.0
	nodeClaim := &v1.NodeClaim{}
	for nn, metadata := range npCost.nodeClaims {
		nodeClaim.ObjectMeta = metav1.ObjectMeta{Name: nn.Name, Namespace: nn.Namespace, Labels: metadata.labels, Annotations: metadata.annotations}
		var it *cloudprovider.InstanceType
		if tracked, ok := npCost.instanceTypes[metadata.NodeClaimKey.InstanceName]; ok {
			it = tracked.instanceType
		}
		if price, ok := cloudprovider.PriceNodeClaim(ctx, nodeClaim, it); ok {
			metadata.Price = price
		}
		cost += metadata.Price
	}
	npCost.cost = cost
}

// UpdateNodeClaim adds a NodeClaim to cost tracking. The NodeClaim must have
// all required labels or it will be ignored and logged as an error.
func (cc *ClusterCost) UpdateNodeClaim(ctx context.Context, nodeClaim *v1.NodeClaim) error {
	cc.Lock()
	defer cc.Unlock()
	if _, exists := cc.nodeClaimMap[client.ObjectKeyFromObject(nodeClaim)]; exists {
		return nil
	}

	failed := false
	defer func() {
		if failed {
			CostTrackingErrorsTotal.Inc(map[string]string{
				metrics.NodePoolLabel: nodeClaim.Labels[v1.NodePoolLabelKey],
			})
		}
	}()

	// First lets check if the right labels are there
	if nodeClaimMissingLabels(*nodeClaim) {
		// not technically a failure mode as we expect to retry once the
		// labels are propagated
		return nil
	}

	nodePoolName := nodeClaim.Labels[v1.NodePoolLabelKey]
	offeringKey := OfferingKey{CapacityType: nodeClaim.Labels[v1.CapacityTypeLabelKey], Zone: nodeClaim.Labels[corev1.LabelTopologyZone], InstanceName: nodeClaim.Labels[corev1.LabelInstanceTypeStable]}

	metadata, err := cc.internalAddNodeClaim(ctx, nodePoolName, offeringKey, nodeClaim)
	if err != nil {
		failed = true
		return serrors.Wrap(err, "nodeclaim", klog.KObj(nodeClaim), "nodepool", nodePoolName)
	}
	cc.nodeClaimMap[client.ObjectKeyFromObject(nodeClaim)] = metadata
	return nil
}

// DeleteNodeClaim removes a NodeClaim from cost tracking. If the NodeClaim
// was not being tracked, this operation is a no-op.
func (cc *ClusterCost) DeleteNodeClaim(ctx context.Context, nn types.NamespacedName) error {
	cc.Lock()
	defer cc.Unlock()
	metadata, exists := cc.nodeClaimMap[nn]

	if !exists {
		return nil
	}

	failed := false
	defer func() {
		if failed {
			CostTrackingErrorsTotal.Inc(map[string]string{
				metrics.NodePoolLabel: metadata.NodePoolName,
			})
		}
	}()

	err := cc.internalRemoveNodeClaim(metadata.NodePoolName, nn)
	if err != nil {
		failed = true
		return serrors.Wrap(err, "namespacedName", nn, "nodepool", metadata.NodePoolName)
	}

	// If it succeeds, we can remove the metadata
	delete(cc.nodeClaimMap, nn)
	return nil
}

func (cc *ClusterCost) DeleteNodePool(ctx context.Context, npName string) {
	cc.Lock()
	defer cc.Unlock()

	cc.nodeClaimMap = lo.PickBy(cc.nodeClaimMap, func(_ types.NamespacedName, metadata *NodeClaimMetaData) bool {
		return metadata.NodePoolName != npName
	})
	delete(cc.npCostMap, npName)
}

// internalAddNodeClaim updates the internal clusterCost state to include a new NodeClaim for a given nodepool.
// It is used to increment the overall cost when a node joins the cluster. It is only called by UpdateNodeClaim
// after that function has determined if a nodeclaim is new.
func (cc *ClusterCost) internalAddNodeClaim(ctx context.Context, npName string, offeringKey OfferingKey, nodeClaim *v1.NodeClaim) (*NodeClaimMetaData, error) {
	np := &v1.NodePool{}
	if err := cc.client.Get(ctx, client.ObjectKey{Name: npName}, np, &client.GetOptions{}); err != nil {
		return nil, err
	}

	if _, exists := cc.npCostMap[npName]; !exists {
		cc.npCostMap[npName] = newNodePoolCost()
	}
	npCost := cc.npCostMap[npName]

	tracked, exists := npCost.instanceTypes[offeringKey.InstanceName]
	if !exists {
		instanceTypes, err := cc.cloudProvider.GetInstanceTypes(ctx, np)
		if err != nil {
			return nil, fmt.Errorf("failed to get instance types for nodepool %q while adding offering for instance %q, %w", np.Name, offeringKey.InstanceName, err)
		}
		it, _ := lo.Find(instanceTypes, func(it *cloudprovider.InstanceType) bool { return it != nil && it.Name == offeringKey.InstanceName })
		tracked = &trackedInstanceType{instanceType: it}
		npCost.instanceTypes[offeringKey.InstanceName] = tracked
	}
	tracked.count++
	price, found := cloudprovider.PriceNodeClaim(ctx, nodeClaim, tracked.instanceType)
	if !found {
		log.FromContext(ctx).Error(fmt.Errorf("failed to find offering for instance %q in zone %q with capacity %q in nodepool %q", offeringKey.InstanceName, offeringKey.Zone, offeringKey.CapacityType, npName), "offering price unknown — cost tracking will undercount for this nodeclaim until next update")
	}
	metadata := &NodeClaimMetaData{
		NodePoolName: npName,
		NodeClaimKey: offeringKey,
		Price:        price,
		labels:       maps.Clone(nodeClaim.Labels),
		annotations:  maps.Clone(nodeClaim.Annotations),
	}
	npCost.nodeClaims[client.ObjectKeyFromObject(nodeClaim)] = metadata
	npCost.cost += price
	return metadata, nil
}

// internalRemoveNodeClaim updates the internal clusterCost state to remove an existing NodeClaim for a given nodepool.
// It is used to decrement the overall cost when a node leaves the cluster. It is only called by DeleteNodeClaim
// after that function has determined if a nodeclaim is already being accounted for.
func (cc *ClusterCost) internalRemoveNodeClaim(npName string, nn types.NamespacedName) error {
	npc, exists := cc.npCostMap[npName]
	if !exists {
		return fmt.Errorf("attempted to remove nodeclaim %q from nonexistent nodepool %q", nn, npName)
	}

	metadata, exists := npc.nodeClaims[nn]
	if !exists {
		return fmt.Errorf("attempted to remove nonexistent nodeclaim %q from nodepool %q", nn, npName)
	}

	npc.cost -= metadata.Price
	delete(npc.nodeClaims, nn)
	if tracked := npc.instanceTypes[metadata.NodeClaimKey.InstanceName]; tracked != nil {
		if tracked.count--; tracked.count <= 0 {
			delete(npc.instanceTypes, metadata.NodeClaimKey.InstanceName)
		}
	}
	if len(npc.nodeClaims) == 0 {
		delete(cc.npCostMap, npName)
	}
	return nil
}

func (cc *ClusterCost) Reset() {
	cc.Lock()
	defer cc.Unlock()
	cc.npCostMap = make(map[string]*NodePoolCost)
	cc.nodeClaimMap = make(map[types.NamespacedName]*NodeClaimMetaData)
}

// GetClusterCost returns the total cost of all compute resources across
// all NodePools in the cluster.
func (cc *ClusterCost) GetClusterCost() float64 {
	cc.RLock()
	defer cc.RUnlock()
	return lo.SumBy(lo.Values(cc.npCostMap), func(npc *NodePoolCost) float64 { return npc.cost })
}

// GetNodepoolCost returns the total cost of compute resources for a specific
// NodePool. Returns 0 if the NodePool is not being tracked.
func (cc *ClusterCost) GetNodepoolCost(np *v1.NodePool) float64 {
	cc.RLock()
	defer cc.RUnlock()
	npc, exists := cc.npCostMap[np.Name]
	if !exists {
		return 0
	}
	return npc.cost
}

func newNodePoolCost() *NodePoolCost {
	return &NodePoolCost{
		instanceTypes: map[string]*trackedInstanceType{},
		nodeClaims:    map[types.NamespacedName]*NodeClaimMetaData{},
	}
}

func nodeClaimMissingLabels(nc v1.NodeClaim) bool {
	var missingLabels []string
	for _, key := range NecessaryLabels {
		_, exists := nc.Labels[key]
		if !exists {
			missingLabels = append(missingLabels, key)
		}
	}
	return len(missingLabels) > 0
}
