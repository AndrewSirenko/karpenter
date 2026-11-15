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

package disruptionbench

import (
	"fmt"
	"math"
	"math/rand/v2"
	"sort"
	"strconv"

	"github.com/awslabs/operatorpkg/object"
	"github.com/awslabs/operatorpkg/status"
	"github.com/samber/lo"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/fake"
	"sigs.k8s.io/karpenter/pkg/scheduling"
	"sigs.k8s.io/karpenter/pkg/test/v1alpha1"
)

const (
	// GroupLabelKey pins each workload to a node group. Noise uses one group per NodePool; every plant gets its own.
	GroupLabelKey = "disruptionbench.example.com/group"
	Namespace     = "default"
	appLabelKey   = "app"
	maxPods       = 110
	spotDiscount  = 0.4
)

type family struct {
	name      string
	memPerCPU int64
	// odLarge is the on-demand price of the large size, from the us-east-1 c6i/m6i/r6i list price.
	odLarge float64
}

var families = []family{
	{name: "c6i", memPerCPU: 2, odLarge: 0.085},
	{name: "m6i", memPerCPU: 4, odLarge: 0.096},
	{name: "r6i", memPerCPU: 8, odLarge: 0.126},
}

var sizes = []struct {
	name string
	cpu  int64
}{{"large", 2}, {"xlarge", 4}, {"2xlarge", 8}, {"4xlarge", 16}, {"8xlarge", 32}, {"12xlarge", 48}, {"16xlarge", 64}}

// noiseSizes are the sizes noise nodes use, with weights.
var noiseSizes = map[string]float64{"large": 0.15, "xlarge": 0.3, "2xlarge": 0.35, "4xlarge": 0.2}
var noiseFamilies = map[string]float64{"c6i": 0.3, "m6i": 0.45, "r6i": 0.25}

// res is a request in millicores and bytes.
type res struct{ cpu, mem int64 }

func (r res) add(o res) res     { return res{r.cpu + o.cpu, r.mem + o.mem} }
func (r res) sub(o res) res     { return res{r.cpu - o.cpu, r.mem - o.mem} }
func (r res) times(n int64) res { return res{r.cpu * n, r.mem * n} }
func (r res) div(n int64) res   { return res{r.cpu / n, r.mem / n} }
func (r res) fits(in res) bool  { return r.cpu <= in.cpu && r.mem <= in.mem }
func (r res) String() string    { return fmt.Sprintf("%dm/%dMi", r.cpu, r.mem>>20) }
func resOf(l corev1.ResourceList) res {
	return res{l.Cpu().MilliValue(), l.Memory().Value()}
}
func (r res) list() corev1.ResourceList {
	return corev1.ResourceList{
		corev1.ResourceCPU:    *resource.NewMilliQuantity(r.cpu, resource.DecimalSI),
		corev1.ResourceMemory: *resource.NewQuantity(r.mem, resource.BinarySI),
	}
}

type poolSpec struct {
	name         string
	capacityType string
	noiseGroup   string
	groups       []string
}

type deploymentSpec struct {
	name, hash, group string
	req               res
	antiAffinity      bool
	zonalTSC          bool
	hostnameTSC       bool
	pdb               bool
	family            string
	replicas          int
}

func (d *deploymentSpec) onePerNode() bool { return d.antiAffinity || d.hostnameTSC }

type nodeSpec struct {
	name  string
	pool  *poolSpec
	zone  string
	it    *cloudprovider.InstanceType
	group string
	pods  []*deploymentSpec
}

func (n *nodeSpec) contents() res {
	return lo.Reduce(n.pods, func(r res, d *deploymentSpec, _ int) res { return r.add(d.req) }, res{})
}

type builder struct {
	s           Spec
	rng         *rand.Rand
	catalog     []*cloudprovider.InstanceType
	byName      map[string]*cloudprovider.InstanceType
	zones       []string
	pools       []*poolSpec
	odPools     []*poolSpec
	daemonSets  []*appsv1.DaemonSet
	dsReq       res
	nodes       []*nodeSpec
	deployments []*deploymentSpec
	planted     []PlantedMove
	nodeSeq     int
	groupSeq    int
}

// Generate builds a cluster from s. The same Spec always yields the same objects, apart from condition timestamps.
func Generate(s Spec) (*Fixture, error) {
	s = withDefaults(s)
	if s.InstanceTypes < 3*4 {
		return nil, fmt.Errorf("need at least 12 instance types (c/m/r x large..4xlarge), got %d", s.InstanceTypes)
	}
	b := &builder{s: s, rng: rand.New(rand.NewPCG(uint64(s.Seed), 0x5eed)), byName: map[string]*cloudprovider.InstanceType{}}
	b.buildCatalog()
	b.buildPools()
	b.buildDaemonSets()
	for _, p := range s.Plants {
		for range p.Count {
			if err := b.plant(p.Kind); err != nil {
				return nil, err
			}
		}
	}
	noise := s.Nodes - len(b.nodes)
	if noise < 0 {
		return nil, fmt.Errorf("plants need %d nodes, spec has %d", len(b.nodes), s.Nodes)
	}
	if err := b.buildNoise(noise); err != nil {
		return nil, err
	}
	return b.render(), nil
}

func withDefaults(s Spec) Spec {
	if s.Zones <= 0 {
		s.Zones = 3
	}
	if s.NodePools <= 0 {
		s.NodePools = 1
	}
	if s.PodsPerNode <= 0 {
		s.PodsPerNode = 10
	}
	if s.InstanceTypes <= 0 {
		s.InstanceTypes = 12
	}
	return s
}

func (b *builder) buildCatalog() {
	for i := range b.s.Zones {
		b.zones = append(b.zones, fmt.Sprintf("test-zone-%d", i+1))
	}
	nSizes := lo.Clamp(int(math.Ceil(float64(b.s.InstanceTypes)/float64(len(families)))), 4, len(sizes))
	for _, sz := range sizes[:nSizes] {
		for _, f := range families {
			od := math.Round(f.odLarge*float64(sz.cpu)/2*1e4) / 1e4
			var offerings []*cloudprovider.Offering
			for _, z := range b.zones {
				for _, ct := range []string{v1.CapacityTypeOnDemand, v1.CapacityTypeSpot} {
					price := lo.Ternary(ct == v1.CapacityTypeSpot, math.Round(od*spotDiscount*1e4)/1e4, od)
					offerings = append(offerings, &cloudprovider.Offering{
						Available:    true,
						Requirements: scheduling.NewLabelRequirements(map[string]string{v1.CapacityTypeLabelKey: ct, corev1.LabelTopologyZone: z}),
						Price:        price,
					})
				}
			}
			it := fake.NewInstanceType(f.name+"."+sz.name,
				fake.WithResources(corev1.ResourceList{
					corev1.ResourceCPU:    *resource.NewQuantity(sz.cpu, resource.DecimalSI),
					corev1.ResourceMemory: *resource.NewQuantity(sz.cpu*f.memPerCPU<<30, resource.BinarySI),
					corev1.ResourcePods:   *resource.NewQuantity(maxPods, resource.DecimalSI),
				}),
				fake.WithOfferings(lo.FromSlicePtr(offerings)...),
				fake.WithOperatingSystems(string(corev1.Linux)),
			)
			b.catalog = append(b.catalog, it)
			b.byName[it.Name] = it
		}
	}
}

func (b *builder) buildPools() {
	spot := int(math.Round(b.s.SpotFraction * float64(b.s.NodePools)))
	if len(b.s.Plants) > 0 {
		spot = min(spot, b.s.NodePools-1)
	}
	for i := range b.s.NodePools {
		p := &poolSpec{name: fmt.Sprintf("pool-%d", i), capacityType: lo.Ternary(i >= b.s.NodePools-spot, v1.CapacityTypeSpot, v1.CapacityTypeOnDemand)}
		p.noiseGroup = "noise-" + p.name
		p.groups = []string{p.noiseGroup}
		b.pools = append(b.pools, p)
		if p.capacityType == v1.CapacityTypeOnDemand {
			b.odPools = append(b.odPools, p)
		}
	}
}

func (b *builder) buildDaemonSets() {
	for i := range b.s.DaemonSets {
		r := res{cpu: 50 + 10*b.rng.Int64N(11), mem: (64 + 32*b.rng.Int64N(7)) << 20}
		b.dsReq = b.dsReq.add(r)
		name := fmt.Sprintf("ds-%d", i)
		b.daemonSets = append(b.daemonSets, &appsv1.DaemonSet{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: Namespace, UID: types.UID("uid-" + name)},
			Spec: appsv1.DaemonSetSpec{
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{appLabelKey: name}},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{appLabelKey: name}},
					Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "pause", Resources: corev1.ResourceRequirements{Requests: r.list()}}}},
				},
			},
		})
	}
}

func (b *builder) it(fam, size string) *cloudprovider.InstanceType { return b.byName[fam+"."+size] }

// avail is what an instance type has left for workload pods once DaemonSets are placed.
func (b *builder) avail(it *cloudprovider.InstanceType) res {
	return resOf(it.Allocatable()).sub(b.dsReq)
}

func price(it *cloudprovider.InstanceType, capacityType string) float64 {
	return lo.MinBy(lo.Filter(it.Offerings, func(o *cloudprovider.Offering, _ int) bool { return o.CapacityType() == capacityType }),
		func(a, b *cloudprovider.Offering) bool { return a.Price < b.Price }).Price
}

// cheapestFit is the cheapest instance type that holds r on top of DaemonSets.
func (b *builder) cheapestFit(r res, capacityType string) (*cloudprovider.InstanceType, float64) {
	var best *cloudprovider.InstanceType
	bestPrice := math.MaxFloat64
	for _, it := range b.catalog {
		if p := price(it, capacityType); r.fits(b.avail(it)) && p < bestPrice {
			best, bestPrice = it, p
		}
	}
	return best, bestPrice
}

// tight reports that no type cheaper than the node's own holds its pods.
func (b *builder) tight(n *nodeSpec) bool {
	_, p := b.cheapestFit(n.contents(), n.pool.capacityType)
	return p >= price(n.it, n.pool.capacityType)
}

func (b *builder) newGroup(p *poolSpec) string {
	g := fmt.Sprintf("plant-%d", b.groupSeq)
	b.groupSeq++
	p.groups = append(p.groups, g)
	return g
}

func (b *builder) newNode(p *poolSpec, zone string, it *cloudprovider.InstanceType, group string) *nodeSpec {
	n := &nodeSpec{name: fmt.Sprintf("%s-node-%05d", p.name, b.nodeSeq), pool: p, zone: zone, it: it, group: group}
	b.nodeSeq++
	b.nodes = append(b.nodes, n)
	return n
}

func (b *builder) newDeployment(name, group string, req res) *deploymentSpec {
	d := &deploymentSpec{name: name, hash: hashOf(name), group: group, req: req}
	b.deployments = append(b.deployments, d)
	return d
}

func hashOf(name string) string {
	var h uint32 = 2166136261
	for _, c := range []byte(name) {
		h = (h ^ uint32(c)) * 16777619
	}
	return strconv.FormatUint(uint64(h%(1<<30)), 36)
}

func weighted(rng *rand.Rand, w map[string]float64) string {
	keys := lo.Keys(w)
	sort.Strings(keys)
	total := lo.Sum(lo.Values(w))
	x := rng.Float64() * total
	for _, k := range keys {
		if x -= w[k]; x < 0 {
			return k
		}
	}
	return keys[len(keys)-1]
}

// buildNoise packs n nodes so that none can be deleted, replaced with a cheaper type, or merged with another node.
// Each noise node runs one pod of an anti-affine guard Deployment, so two noise nodes never fit on one replacement.
// Nodes are filled with their family's Deployments, then topped up with any of the NodePool's, so no noise pod fits
// in another noise node's leftover room and no cheaper type holds a node's pods.
func (b *builder) buildNoise(n int) error {
	meanAvail := 0.0
	for sz, w := range noiseSizes {
		meanAvail += w * float64(b.avail(b.it("m6i", sz)).cpu)
	}
	meanPod := meanAvail / float64(b.s.PodsPerNode)
	type key struct{ pool, family string }
	deps := map[key][]*deploymentSpec{}
	guards := map[string]*deploymentSpec{}
	poolDeps := map[string][]*deploymentSpec{}
	perPool := float64(n) / float64(len(b.pools))
	for _, p := range b.pools {
		guard := b.newDeployment("guard-"+p.name, p.noiseGroup, res{cpu: 50, mem: 64 << 20})
		guard.antiAffinity = true
		guards[p.name] = guard
		for _, f := range families {
			count := max(4, int(perPool*noiseFamilies[f.name]*float64(b.s.PodsPerNode)/40))
			for i := range count {
				scale := lo.Ternary(i%4 == 3, 0.25, 0.3+1.4*b.rng.Float64())
				cpu := max(100, int64(math.Round(meanPod*scale/10))*10)
				mem := int64(float64(cpu)/1000*float64(f.memPerCPU)*1024*(0.85+0.15*b.rng.Float64())) << 20
				d := b.newDeployment(fmt.Sprintf("svc-%s-%s-%d", p.name, f.name, i), p.noiseGroup, res{cpu: cpu, mem: mem})
				d.family = f.name
				switch u := b.rng.Float64(); {
				case u < b.s.AntiAffinity:
					d.antiAffinity = true
				case u < b.s.AntiAffinity+b.s.HostnameTSC:
					d.hostnameTSC = true
				case u < b.s.AntiAffinity+b.s.HostnameTSC+b.s.ZonalTSC:
					d.zonalTSC = true
				}
				d.pdb = b.s.PDBs
				deps[key{p.name, f.name}] = append(deps[key{p.name, f.name}], d)
				poolDeps[p.name] = append(poolDeps[p.name], d)
			}
		}
	}
	for i := range n {
		p := b.pools[i%len(b.pools)]
		fam := weighted(b.rng, noiseFamilies)
		it := b.it(fam, weighted(b.rng, noiseSizes))
		zone := b.zones[(i/len(b.pools))%len(b.zones)]
		node := b.newNode(p, zone, it, p.noiseGroup)
		var ok bool
		for range 50 {
			node.pods = []*deploymentSpec{guards[p.name]}
			b.fill(node, deps[key{p.name, fam}])
			b.fill(node, poolDeps[p.name])
			if ok = b.tight(node); ok {
				break
			}
		}
		if !ok {
			return fmt.Errorf("couldn't pack noise node %s (%s) tightly, contents %s", node.name, it.Name, node.contents())
		}
	}
	for _, d := range b.deployments {
		d.replicas = 0
	}
	for _, node := range b.nodes {
		for _, d := range node.pods {
			d.replicas++
		}
	}
	return nil
}

func (b *builder) fill(node *nodeSpec, deps []*deploymentSpec) {
	avail := b.avail(node.it)
	used := node.contents()
	onNode := map[*deploymentSpec]bool{}
	for _, d := range node.pods {
		onNode[d] = true
	}
	for {
		fits := lo.Filter(deps, func(d *deploymentSpec, _ int) bool {
			return used.add(d.req).fits(avail) && !(d.onePerNode() && onNode[d])
		})
		if len(fits) == 0 {
			return
		}
		d := fits[b.rng.IntN(len(fits))]
		node.pods = append(node.pods, d)
		onNode[d] = true
		used = used.add(d.req)
	}
}

func (b *builder) render() *Fixture {
	f := &Fixture{InstanceTypes: b.catalog, DaemonSets: b.daemonSets, Planted: b.planted}
	nodeClassRef := &v1.NodeClassReference{Group: object.GVK(&v1alpha1.TestNodeClass{}).Group, Kind: object.GVK(&v1alpha1.TestNodeClass{}).Kind, Name: "default"}
	for _, p := range b.pools {
		np := &v1.NodePool{
			ObjectMeta: metav1.ObjectMeta{Name: p.name, UID: types.UID("uid-" + p.name), Generation: 1},
			Spec: v1.NodePoolSpec{
				Template: v1.NodeClaimTemplate{Spec: v1.NodeClaimTemplateSpec{
					NodeClassRef: nodeClassRef,
					Requirements: []v1.NodeSelectorRequirementWithMinValues{
						{Key: v1.CapacityTypeLabelKey, Operator: corev1.NodeSelectorOpIn, Values: []string{p.capacityType}},
						{Key: GroupLabelKey, Operator: corev1.NodeSelectorOpIn, Values: p.groups},
					},
				}},
				Disruption: v1.Disruption{
					ConsolidationPolicy: v1.ConsolidationPolicyWhenEmptyOrUnderutilized,
					ConsolidateAfter:    v1.MustParseNillableDuration("0s"),
					Budgets:             []v1.Budget{{Nodes: "10%"}},
				},
			},
		}
		np.StatusConditions().SetTrue(v1.ConditionTypeValidationSucceeded)
		np.StatusConditions().SetTrue(v1.ConditionTypeNodeClassReady)
		np.StatusConditions().SetTrue(status.ConditionReady)
		f.NodePools = append(f.NodePools, np)
	}
	podSeq := 0
	for _, n := range b.nodes {
		nc, node := RenderNode(n.name, n.pool.name, n.zone, n.pool.capacityType, n.group, n.it, nodeClassRef)
		f.NodeClaims = append(f.NodeClaims, nc)
		f.Nodes = append(f.Nodes, node)
		for _, ds := range b.daemonSets {
			f.Pods = append(f.Pods, DaemonSetPod(ds, n.name))
		}
		for _, d := range n.pods {
			f.Pods = append(f.Pods, b.renderPod(d, n.name, podSeq))
			podSeq++
		}
	}
	for _, d := range b.deployments {
		if !d.pdb || d.replicas == 0 {
			continue
		}
		f.PDBs = append(f.PDBs, &policyv1.PodDisruptionBudget{
			ObjectMeta: metav1.ObjectMeta{Name: d.name, Namespace: Namespace, UID: types.UID("uid-pdb-" + d.name), Generation: 1},
			Spec: policyv1.PodDisruptionBudgetSpec{
				MaxUnavailable: lo.ToPtr(intstr.FromInt32(1)),
				Selector:       &metav1.LabelSelector{MatchLabels: map[string]string{appLabelKey: d.name}},
			},
			Status: policyv1.PodDisruptionBudgetStatus{
				ObservedGeneration: 1, DisruptionsAllowed: 1,
				CurrentHealthy: int32(d.replicas), DesiredHealthy: int32(d.replicas - 1), ExpectedPods: int32(d.replicas),
			},
		})
	}
	return f
}

// RenderNode returns an initialized, consolidatable NodeClaim and Node for an instance type.
func RenderNode(name, pool, zone, capacityType, group string, it *cloudprovider.InstanceType, nodeClassRef *v1.NodeClassReference) (*v1.NodeClaim, *corev1.Node) {
	labels := map[string]string{
		v1.NodePoolLabelKey:                            pool,
		corev1.LabelInstanceTypeStable:                 it.Name,
		corev1.LabelTopologyZone:                       zone,
		v1.CapacityTypeLabelKey:                        capacityType,
		corev1.LabelArchStable:                         "amd64",
		corev1.LabelOSStable:                           string(corev1.Linux),
		corev1.LabelHostname:                           name,
		GroupLabelKey:                                  group,
		v1.NodeClassLabelKey(nodeClassRef.GroupKind()): nodeClassRef.Name,
	}
	capacity := it.Capacity.DeepCopy()
	allocatable := it.Allocatable().DeepCopy()
	providerID := "fake:///" + name
	nc := &v1.NodeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID("uid-nc-" + name), Labels: lo.Assign(labels), Generation: 1},
		Spec: v1.NodeClaimSpec{
			NodeClassRef: nodeClassRef,
			Requirements: []v1.NodeSelectorRequirementWithMinValues{
				{Key: v1.NodePoolLabelKey, Operator: corev1.NodeSelectorOpIn, Values: []string{pool}},
				{Key: corev1.LabelInstanceTypeStable, Operator: corev1.NodeSelectorOpIn, Values: []string{it.Name}},
				{Key: v1.CapacityTypeLabelKey, Operator: corev1.NodeSelectorOpIn, Values: []string{capacityType}},
				{Key: corev1.LabelTopologyZone, Operator: corev1.NodeSelectorOpIn, Values: []string{zone}},
				{Key: GroupLabelKey, Operator: corev1.NodeSelectorOpIn, Values: []string{group}},
			},
		},
		Status: v1.NodeClaimStatus{ProviderID: providerID, NodeName: name, Capacity: capacity, Allocatable: allocatable},
	}
	for _, c := range []string{v1.ConditionTypeLaunched, v1.ConditionTypeRegistered, v1.ConditionTypeInitialized, v1.ConditionTypeConsolidatable} {
		nc.StatusConditions().SetTrue(c)
	}
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID("uid-node-" + name), Labels: lo.Assign(labels, map[string]string{
			v1.NodeRegisteredLabelKey:  "true",
			v1.NodeInitializedLabelKey: "true",
		})},
		Spec: corev1.NodeSpec{ProviderID: providerID},
		Status: corev1.NodeStatus{
			Capacity:    capacity.DeepCopy(),
			Allocatable: allocatable.DeepCopy(),
			Conditions:  []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue, Reason: "KubeletReady"}},
		},
	}
	return nc, node
}

// DaemonSetPod returns ds's running pod on node.
func DaemonSetPod(ds *appsv1.DaemonSet, node string) *corev1.Pod {
	name := ds.Name + "-" + node
	spec := *ds.Spec.Template.Spec.DeepCopy()
	spec.NodeName = node
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: ds.Namespace, UID: types.UID("uid-pod-" + name),
			Labels:          lo.Assign(ds.Spec.Template.Labels),
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "DaemonSet", Name: ds.Name, UID: ds.UID, Controller: lo.ToPtr(true)}},
		},
		Spec:   spec,
		Status: runningStatus(),
	}
}

func runningStatus() corev1.PodStatus {
	return corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{
		{Type: corev1.PodScheduled, Status: corev1.ConditionTrue},
		{Type: corev1.PodReady, Status: corev1.ConditionTrue},
	}}
}

func (b *builder) renderPod(d *deploymentSpec, node string, seq int) *corev1.Pod {
	rs := d.name + "-" + d.hash
	name := fmt.Sprintf("%s-%s", rs, strconv.FormatInt(int64(seq), 36))
	selector := &metav1.LabelSelector{MatchLabels: map[string]string{appLabelKey: d.name}}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: Namespace, UID: types.UID("uid-pod-" + name),
			Labels: map[string]string{appLabelKey: d.name, appsv1.DefaultDeploymentUniqueLabelKey: d.hash},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1", Kind: "ReplicaSet", Name: rs, UID: types.UID("uid-rs-" + rs), Controller: lo.ToPtr(true), BlockOwnerDeletion: lo.ToPtr(true),
			}},
		},
		Spec: corev1.PodSpec{
			NodeName:   node,
			Containers: []corev1.Container{{Name: "app", Image: "app", Resources: corev1.ResourceRequirements{Requests: d.req.list()}}},
			Affinity: &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
				NodeSelectorTerms: []corev1.NodeSelectorTerm{{MatchExpressions: []corev1.NodeSelectorRequirement{{Key: GroupLabelKey, Operator: corev1.NodeSelectorOpIn, Values: []string{d.group}}}}},
			}}},
		},
		Status: runningStatus(),
	}
	if d.antiAffinity {
		pod.Spec.Affinity.PodAntiAffinity = &corev1.PodAntiAffinity{RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{LabelSelector: selector, TopologyKey: corev1.LabelHostname}}}
	}
	if d.zonalTSC {
		pod.Spec.TopologySpreadConstraints = append(pod.Spec.TopologySpreadConstraints, corev1.TopologySpreadConstraint{MaxSkew: 1, TopologyKey: corev1.LabelTopologyZone, WhenUnsatisfiable: corev1.DoNotSchedule, LabelSelector: selector})
	}
	if d.hostnameTSC {
		pod.Spec.TopologySpreadConstraints = append(pod.Spec.TopologySpreadConstraints, corev1.TopologySpreadConstraint{MaxSkew: 1, TopologyKey: corev1.LabelHostname, WhenUnsatisfiable: corev1.DoNotSchedule, LabelSelector: selector})
	}
	return pod
}
