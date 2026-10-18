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

package parity

import (
	"fmt"
	"math/rand"

	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/fake"
	"sigs.k8s.io/karpenter/pkg/scheduling"
	"sigs.k8s.io/karpenter/pkg/test"
)

const (
	TeamLabel      = "parity.test/team"
	AppLabel       = "app"
	DedicatedTaint = "parity.test/dedicated"
	Namespace      = "default"
)

var (
	Zones         = []string{"test-zone-1", "test-zone-2", "test-zone-3"}
	Arches        = []string{v1.ArchitectureAmd64, v1.ArchitectureArm64}
	CapacityTypes = []string{v1.CapacityTypeSpot, v1.CapacityTypeOnDemand}
	Teams         = []string{"red", "blue"}
	Apps          = []string{"a", "b", "c"}
)

// ExistingNode is a Karpenter-managed node that exists before provisioning.
type ExistingNode struct {
	NodeClaim *v1.NodeClaim
	Node      *corev1.Node
}

// Scenario is one generated cluster: NodePools, existing nodes with bound pods, and pending pods.
type Scenario struct {
	Seed          int64
	Profile       string
	NodePools     []*v1.NodePool
	ExistingNodes []ExistingNode
	BoundPods     []*corev1.Pod
	PendingPods   []*corev1.Pod
}

// DeepCopy returns a scenario safe to apply to an API server.
func (s *Scenario) DeepCopy() *Scenario {
	return &Scenario{
		Seed:      s.Seed,
		Profile:   s.Profile,
		NodePools: lo.Map(s.NodePools, func(np *v1.NodePool, _ int) *v1.NodePool { return np.DeepCopy() }),
		ExistingNodes: lo.Map(s.ExistingNodes, func(e ExistingNode, _ int) ExistingNode {
			return ExistingNode{NodeClaim: e.NodeClaim.DeepCopy(), Node: e.Node.DeepCopy()}
		}),
		BoundPods:   lo.Map(s.BoundPods, func(p *corev1.Pod, _ int) *corev1.Pod { return p.DeepCopy() }),
		PendingPods: lo.Map(s.PendingPods, func(p *corev1.Pod, _ int) *corev1.Pod { return p.DeepCopy() }),
	}
}

// Profile bounds the size of a generated scenario.
type Profile struct {
	Name         string
	MaxNodePools int
	MaxExisting  int
	MaxBoundPods int
	MinPending   int
	MaxPending   int
}

var Profiles = map[string]Profile{
	"small":  {Name: "small", MaxNodePools: 2, MaxExisting: 2, MaxBoundPods: 3, MinPending: 1, MaxPending: 6},
	"medium": {Name: "medium", MaxNodePools: 3, MaxExisting: 4, MaxBoundPods: 8, MinPending: 5, MaxPending: 25},
}

// InstanceTypes is the fixed instance type catalog every scenario uses.
func InstanceTypes() []*cloudprovider.InstanceType {
	var its []*cloudprovider.InstanceType
	for _, cpu := range []int{1, 2, 4, 8, 16} {
		for _, memPerCPU := range []int{2, 8} {
			for _, arch := range Arches {
				its = append(its, fake.NewInstanceType(
					fmt.Sprintf("c%d-m%d-%s", cpu, cpu*memPerCPU, arch),
					fake.WithArchitecture(arch),
					fake.WithOperatingSystems(string(corev1.Linux)),
					fake.WithResources(corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse(fmt.Sprint(cpu)),
						corev1.ResourceMemory: resource.MustParse(fmt.Sprintf("%dGi", cpu*memPerCPU)),
						corev1.ResourcePods:   resource.MustParse(fmt.Sprint(min(110, cpu*8))),
					}),
				))
			}
		}
	}
	// One instance type offered in a single zone, so zone sets differ between instance types.
	zonal := corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("32"),
		corev1.ResourceMemory: resource.MustParse("64Gi"),
		corev1.ResourcePods:   resource.MustParse("110"),
	}
	its = append(its, fake.NewInstanceType("c32-m64-zone1-only",
		fake.WithOperatingSystems(string(corev1.Linux)),
		fake.WithResources(zonal),
		fake.WithOfferings(cloudprovider.Offering{
			Available:    true,
			Requirements: scheduling.NewLabelRequirements(map[string]string{v1.CapacityTypeLabelKey: v1.CapacityTypeOnDemand, corev1.LabelTopologyZone: "test-zone-1"}),
			Price:        fake.PriceFromResources(zonal),
		}),
	))
	return its
}

type generator struct {
	r   *rand.Rand
	its []*cloudprovider.InstanceType
}

func (g *generator) chance(p float64) bool { return g.r.Float64() < p }
func (g *generator) intn(n int) int        { return g.r.Intn(n) }
func pick[T any](g *generator, xs []T) T   { return xs[g.r.Intn(len(xs))] }
func (g *generator) subset(xs []string) []string {
	out := lo.Filter(xs, func(_ string, _ int) bool { return g.chance(0.6) })
	if len(out) == 0 {
		out = []string{pick(g, xs)}
	}
	return out
}

// Generate builds a deterministic scenario from seed.
func Generate(seed int64, profile Profile) *Scenario {
	g := &generator{r: rand.New(rand.NewSource(seed)), its: InstanceTypes()}
	s := &Scenario{Seed: seed, Profile: profile.Name}
	for i := range 1 + g.intn(profile.MaxNodePools) {
		s.NodePools = append(s.NodePools, g.nodePool(i))
	}
	for i := range g.intn(profile.MaxExisting + 1) {
		if e, ok := g.existingNode(i, pick(g, s.NodePools)); ok {
			s.ExistingNodes = append(s.ExistingNodes, e)
		}
	}
	if len(s.ExistingNodes) > 0 {
		for i := range g.intn(profile.MaxBoundPods + 1) {
			s.BoundPods = append(s.BoundPods, g.boundPod(i, pick(g, s.ExistingNodes)))
		}
	}
	for i := range profile.MinPending + g.intn(profile.MaxPending-profile.MinPending+1) {
		s.PendingPods = append(s.PendingPods, g.pendingPod(i, s))
	}
	return s
}

// NewNodePool returns a NodePool restricted to linux, the only OS in the catalog.
func NewNodePool(name string) *v1.NodePool {
	np := test.NodePool(v1.NodePool{ObjectMeta: metav1.ObjectMeta{Name: name}})
	np.Spec.Template.Spec.Requirements = []v1.NodeSelectorRequirementWithMinValues{
		{Key: corev1.LabelOSStable, Operator: corev1.NodeSelectorOpIn, Values: []string{string(corev1.Linux)}},
	}
	return np
}

func (g *generator) nodePool(i int) *v1.NodePool {
	np := NewNodePool(fmt.Sprintf("np-%d", i))
	addReq := func(key string, values []string) {
		np.Spec.Template.Spec.Requirements = append(np.Spec.Template.Spec.Requirements,
			v1.NodeSelectorRequirementWithMinValues{Key: key, Operator: corev1.NodeSelectorOpIn, Values: values})
	}
	if g.chance(0.4) {
		addReq(corev1.LabelTopologyZone, g.subset(Zones))
	}
	if g.chance(0.4) {
		addReq(v1.CapacityTypeLabelKey, g.subset(CapacityTypes))
	}
	if g.chance(0.3) {
		addReq(corev1.LabelArchStable, g.subset(Arches))
	}
	if g.chance(0.5) {
		team := pick(g, Teams)
		np.Spec.Template.Labels = map[string]string{TeamLabel: team}
		if g.chance(0.4) {
			np.Spec.Template.Spec.Taints = []corev1.Taint{{Key: DedicatedTaint, Value: team, Effect: corev1.TaintEffectNoSchedule}}
		}
	}
	if g.chance(0.2) {
		np.Spec.Limits = v1.Limits(corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(fmt.Sprint(pick(g, []int{4, 8, 16})))})
	}
	if g.chance(0.3) {
		np.Spec.Weight = lo.ToPtr(int32(1 + g.intn(50)))
	}
	return np
}

// existingNode picks an instance type and offering compatible with the NodePool, the way a launch would.
func (g *generator) existingNode(i int, np *v1.NodePool) (ExistingNode, bool) {
	reqs := scheduling.NewNodeSelectorRequirementsWithMinValues(np.Spec.Template.Spec.Requirements...)
	compatible := lo.Filter(g.its, func(it *cloudprovider.InstanceType, _ int) bool {
		return reqs.IsCompatible(it.Requirements, scheduling.AllowUndefinedWellKnownLabels) && it.Offerings.Available().HasCompatible(reqs)
	})
	if len(compatible) == 0 {
		return ExistingNode{}, false
	}
	it := pick(g, compatible)
	offering := pick(g, it.Offerings.Available().Compatible(reqs))
	return NewExistingNode(fmt.Sprintf("existing-%d", i), np, it, offering), true
}

// NewExistingNode builds an initialized-looking Karpenter node from np on instance type it and offering.
func NewExistingNode(name string, np *v1.NodePool, it *cloudprovider.InstanceType, offering *cloudprovider.Offering) ExistingNode {
	labels := map[string]string{
		v1.NodePoolLabelKey:            np.Name,
		corev1.LabelHostname:           name,
		corev1.LabelInstanceTypeStable: it.Name,
		corev1.LabelTopologyZone:       offering.Requirements.Get(corev1.LabelTopologyZone).Any(),
		v1.CapacityTypeLabelKey:        offering.Requirements.Get(v1.CapacityTypeLabelKey).Any(),
	}
	for key, req := range it.Requirements {
		if req.Operator() == corev1.NodeSelectorOpIn && labels[key] == "" {
			labels[key] = req.Values()[0]
		}
	}
	labels = lo.Assign(labels, np.Spec.Template.Labels)
	nc, node := test.NodeClaimAndNode(v1.NodeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels},
		Spec:       v1.NodeClaimSpec{Taints: np.Spec.Template.Spec.Taints},
		Status: v1.NodeClaimStatus{
			ProviderID:  fmt.Sprintf("fake:///%s", name),
			Capacity:    it.Capacity,
			Allocatable: it.Allocatable(),
		},
	})
	node.Name = name
	return ExistingNode{NodeClaim: nc, Node: node}
}

func (g *generator) boundPod(i int, e ExistingNode) *corev1.Pod {
	pod := test.Pod(test.PodOptions{
		ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("bound-%d", i), Namespace: Namespace, Labels: map[string]string{AppLabel: pick(g, Apps)}},
		NodeName:   e.Node.Name,
		ResourceRequirements: corev1.ResourceRequirements{Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse(pick(g, []string{"100m", "250m"})),
			corev1.ResourceMemory: resource.MustParse("128Mi"),
		}},
		Tolerations: []corev1.Toleration{{Key: DedicatedTaint, Operator: corev1.TolerationOpExists}},
		Phase:       corev1.PodRunning,
	})
	if g.chance(0.2) {
		pod.Spec.Affinity = &corev1.Affinity{PodAntiAffinity: &corev1.PodAntiAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{g.podAffinityTerm()},
		}}
	}
	return pod
}

func (g *generator) pendingPod(i int, s *Scenario) *corev1.Pod {
	app := pick(g, Apps)
	opts := test.PodOptions{
		ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("pod-%d", i), Namespace: Namespace, Labels: map[string]string{AppLabel: app}},
		ResourceRequirements: corev1.ResourceRequirements{Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse(pick(g, []string{"100m", "250m", "500m", "1", "2", "3"})),
			corev1.ResourceMemory: resource.MustParse(pick(g, []string{"128Mi", "512Mi", "1Gi", "2Gi", "4Gi"})),
		}},
	}
	if g.chance(0.15) {
		opts.InitContainers = []corev1.Container{{
			Name: "init",
			Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse(pick(g, []string{"1", "2", "4"})),
				corev1.ResourceMemory: resource.MustParse("1Gi"),
			}},
		}}
	}
	if g.chance(0.25) {
		key, value := g.labelValue()
		opts.NodeSelector = map[string]string{key: value}
	}
	if g.chance(0.4) {
		opts.Tolerations = []corev1.Toleration{{Key: DedicatedTaint, Operator: corev1.TolerationOpEqual, Value: pick(g, Teams), Effect: corev1.TaintEffectNoSchedule}}
	}
	if g.chance(0.1) {
		opts.HostPorts = []int32{pick(g, []int32{80, 8080})}
	}
	pod := test.UnschedulablePod(opts)
	affinity := &corev1.Affinity{}
	if g.chance(0.4) {
		terms := make([]corev1.NodeSelectorTerm, 1+lo.Ternary(g.chance(0.3), 1, 0))
		for t := range terms {
			for range 1 + g.intn(3) {
				terms[t].MatchExpressions = append(terms[t].MatchExpressions, g.nodeSelectorRequirement())
			}
		}
		affinity.NodeAffinity = &corev1.NodeAffinity{RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{NodeSelectorTerms: terms}}
	}
	if g.chance(0.25) {
		if affinity.NodeAffinity == nil {
			affinity.NodeAffinity = &corev1.NodeAffinity{}
		}
		for range 1 + g.intn(2) {
			affinity.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution = append(affinity.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution,
				corev1.PreferredSchedulingTerm{Weight: int32(1 + g.intn(100)), Preference: corev1.NodeSelectorTerm{MatchExpressions: []corev1.NodeSelectorRequirement{g.nodeSelectorRequirement()}}})
		}
	}
	if g.chance(0.15) {
		affinity.PodAffinity = &corev1.PodAffinity{RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{g.podAffinityTerm()}}
	}
	if g.chance(0.2) {
		affinity.PodAntiAffinity = &corev1.PodAntiAffinity{RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{g.podAffinityTerm()}}
	}
	if affinity.NodeAffinity != nil || affinity.PodAffinity != nil || affinity.PodAntiAffinity != nil {
		pod.Spec.Affinity = affinity
	}
	if g.chance(0.35) {
		for range 1 + lo.Ternary(g.chance(0.25), 1, 0) {
			tsc := g.topologySpreadConstraint(app)
			if !lo.ContainsBy(pod.Spec.TopologySpreadConstraints, func(t corev1.TopologySpreadConstraint) bool {
				return t.TopologyKey == tsc.TopologyKey && t.WhenUnsatisfiable == tsc.WhenUnsatisfiable
			}) {
				pod.Spec.TopologySpreadConstraints = append(pod.Spec.TopologySpreadConstraints, tsc)
			}
		}
	}
	return pod
}

func (g *generator) labelValue() (string, string) {
	switch g.intn(8) {
	case 0:
		return corev1.LabelTopologyZone, pick(g, append([]string{"test-zone-4"}, Zones...))
	case 1:
		return corev1.LabelArchStable, pick(g, Arches)
	case 2:
		return v1.CapacityTypeLabelKey, pick(g, CapacityTypes)
	case 3:
		return fake.LabelInstanceSize, pick(g, []string{"small", "large"})
	case 4:
		return fake.ExoticInstanceLabelKey, "optional"
	case 5:
		return fake.IntegerInstanceLabelKey, pick(g, []string{"1", "2", "4", "8", "16", "32"})
	case 6:
		return TeamLabel, pick(g, Teams)
	default:
		return corev1.LabelInstanceTypeStable, pick(g, g.its).Name
	}
}

func (g *generator) nodeSelectorRequirement() corev1.NodeSelectorRequirement {
	key, value := g.labelValue()
	if key == fake.IntegerInstanceLabelKey && g.chance(0.6) {
		return corev1.NodeSelectorRequirement{Key: key, Operator: pick(g, []corev1.NodeSelectorOperator{corev1.NodeSelectorOpGt, corev1.NodeSelectorOpLt}), Values: []string{value}}
	}
	switch g.intn(6) {
	case 0, 1:
		values := []string{value}
		if g.chance(0.4) {
			_, v2 := g.labelValueFor(key)
			values = lo.Uniq(append(values, v2))
		}
		return corev1.NodeSelectorRequirement{Key: key, Operator: corev1.NodeSelectorOpIn, Values: values}
	case 2, 3:
		return corev1.NodeSelectorRequirement{Key: key, Operator: corev1.NodeSelectorOpNotIn, Values: []string{value}}
	case 4:
		return corev1.NodeSelectorRequirement{Key: key, Operator: corev1.NodeSelectorOpExists}
	default:
		return corev1.NodeSelectorRequirement{Key: key, Operator: corev1.NodeSelectorOpDoesNotExist}
	}
}

func (g *generator) labelValueFor(key string) (string, string) {
	for range 20 {
		if k, v := g.labelValue(); k == key {
			return k, v
		}
	}
	return g.labelValue()
}

func (g *generator) podAffinityTerm() corev1.PodAffinityTerm {
	return corev1.PodAffinityTerm{
		LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{AppLabel: pick(g, Apps)}},
		TopologyKey:   pick(g, []string{corev1.LabelTopologyZone, corev1.LabelHostname}),
	}
}

func (g *generator) topologySpreadConstraint(app string) corev1.TopologySpreadConstraint {
	tsc := corev1.TopologySpreadConstraint{
		MaxSkew:           int32(1 + g.intn(2)),
		TopologyKey:       pick(g, []string{corev1.LabelTopologyZone, corev1.LabelTopologyZone, corev1.LabelHostname, v1.CapacityTypeLabelKey}),
		WhenUnsatisfiable: lo.Ternary(g.chance(0.85), corev1.DoNotSchedule, corev1.ScheduleAnyway),
		LabelSelector:     &metav1.LabelSelector{MatchLabels: map[string]string{AppLabel: lo.Ternary(g.chance(0.8), app, pick(g, Apps))}},
	}
	if tsc.WhenUnsatisfiable == corev1.DoNotSchedule && g.chance(0.2) {
		tsc.MinDomains = lo.ToPtr(int32(2 + g.intn(3)))
	}
	if g.chance(0.15) {
		tsc.NodeAffinityPolicy = lo.ToPtr(pick(g, []corev1.NodeInclusionPolicy{corev1.NodeInclusionPolicyHonor, corev1.NodeInclusionPolicyIgnore}))
	}
	if g.chance(0.15) {
		tsc.NodeTaintsPolicy = lo.ToPtr(pick(g, []corev1.NodeInclusionPolicy{corev1.NodeInclusionPolicyHonor, corev1.NodeInclusionPolicyIgnore}))
	}
	return tsc
}
