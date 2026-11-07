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
	"sort"

	"github.com/samber/lo"
)

// Every plant lives in its own node group: its pods select the group label, so neither noise nor another plant can
// use its spare room, and its pods can't use theirs.
func (b *builder) plant(kind PlantKind) error {
	if len(b.odPools) == 0 {
		return fmt.Errorf("plants need an on-demand NodePool")
	}
	pool := b.odPools[b.groupSeq%len(b.odPools)]
	zone := b.zones[b.rng.IntN(len(b.zones))]
	group := b.newGroup(pool)
	var move PlantedMove
	var err error
	switch kind {
	case PlantComplementPair:
		move, err = b.complementPair(pool, zone, group)
	case PlantSmallMerge:
		move, err = b.smallMerge(pool, zone, group)
	case PlantHiddenDelete:
		move, err = b.hiddenDelete(pool, zone, group)
	default:
		return fmt.Errorf("unknown plant kind %q", kind)
	}
	if err != nil {
		return fmt.Errorf("planting %s in %s, %w", kind, group, err)
	}
	b.planted = append(b.planted, move)
	return nil
}

// complementPair plants kubernetes-sigs/karpenter#1962: a c node full of memory-heavy pods at about half CPU, and an r
// node full of CPU-heavy pods at about half memory. Neither has a cheaper single replacement, neither's pods fit the
// other's spare room, and together they fill exactly one m node of the c node's size.
func (b *builder) complementPair(pool *poolSpec, zone, group string) (PlantedMove, error) {
	big, small := "xlarge", "large"
	if b.rng.IntN(2) == 1 {
		big, small = "2xlarge", "xlarge"
	}
	a, r, t := b.it("c6i", big), b.it("r6i", small), b.it("m6i", big)
	availA, availB, availT := b.avail(a), b.avail(r), b.avail(t)
	const pods = 4
	bPod := res{availB.cpu, availT.mem - availA.mem}.div(pods)
	aPod := res{availT.cpu - bPod.times(pods).cpu, availA.mem}.div(pods)
	nodeA := b.newNode(pool, zone, a, group)
	nodeB := b.newNode(pool, zone, r, group)
	dA := b.newDeployment(group+"-memory-heavy", group, aPod)
	dB := b.newDeployment(group+"-cpu-heavy", group, bPod)
	for range pods {
		nodeA.pods = append(nodeA.pods, dA)
		nodeB.pods = append(nodeB.pods, dB)
	}
	dA.replicas, dB.replicas = pods, pods
	return b.mergeMove(PlantComplementPair, pool, nodeA, nodeB)
}

// smallMerge plants the small-node pile from the 1.14 starvation case: 3 or 4 xlarge nodes whose pods fill one
// 2xlarge. No node can be replaced or drained alone, and no two fit on one xlarge.
func (b *builder) smallMerge(pool *poolSpec, zone, group string) (PlantedMove, error) {
	fam := families[b.rng.IntN(len(families))].name
	k, per := int64(4), int64(1)
	if b.rng.IntN(2) == 1 {
		k, per = 3, 2
	}
	it, t := b.it(fam, "xlarge"), b.it(fam, "2xlarge")
	pod := b.avail(t).div(k * per)
	d := b.newDeployment(group+"-svc", group, pod)
	var nodes []*nodeSpec
	for range k {
		n := b.newNode(pool, zone, it, group)
		for range per {
			n.pods = append(n.pods, d)
		}
		nodes = append(nodes, n)
	}
	d.replicas = int(k * per)
	return b.mergeMove(PlantSmallMerge, pool, nodes...)
}

// hiddenDelete plants a cheap node with many small pods, so SavingsRatio ranks it late. Its pods fit exactly into room
// left on two big target nodes, which themselves can't be removed.
func (b *builder) hiddenDelete(pool *poolSpec, zone, group string) (PlantedMove, error) {
	const hPods, targets, perTarget = 6, 2, 3
	hPod := res{cpu: 50, mem: 256 << 20}
	h := b.newNode(pool, zone, b.it("c6i", "large"), group)
	dh := b.newDeployment(group+"-small", group, hPod)
	for range hPods {
		h.pods = append(h.pods, dh)
	}
	dh.replicas = hPods
	t := b.it("m6i", "2xlarge")
	big := b.avail(t).sub(hPod.times(perTarget)).div(2)
	dt := b.newDeployment(group+"-big", group, big)
	var spares []*nodeSpec
	for range targets {
		n := b.newNode(pool, b.zones[b.rng.IntN(len(b.zones))], t, group)
		n.pods = []*deploymentSpec{dt, dt}
		spares = append(spares, n)
	}
	dt.replicas = 2 * targets
	for _, n := range append([]*nodeSpec{h}, spares...) {
		if !b.tight(n) {
			return PlantedMove{}, fmt.Errorf("%s has a cheaper replacement", n.name)
		}
	}
	for _, n := range spares {
		if b.drainable(n, lo.Without(append(spares, h), n)) {
			return PlantedMove{}, fmt.Errorf("target %s can be drained", n.name)
		}
	}
	if !b.drainable(h, spares) {
		return PlantedMove{}, fmt.Errorf("%s doesn't fit its targets", h.name)
	}
	return PlantedMove{Kind: PlantHiddenDelete, NodeNames: []string{h.name}, SavingsPerHour: price(h.it, pool.capacityType)}, nil
}

// mergeMove checks that nodes merge into one cheaper node and that none can be removed alone.
func (b *builder) mergeMove(kind PlantKind, pool *poolSpec, nodes ...*nodeSpec) (PlantedMove, error) {
	var union res
	var cost float64
	for _, n := range nodes {
		if !b.tight(n) {
			return PlantedMove{}, fmt.Errorf("%s has a cheaper replacement", n.name)
		}
		if b.drainable(n, lo.Without(nodes, n)) {
			return PlantedMove{}, fmt.Errorf("%s can be drained onto its group", n.name)
		}
		union = union.add(n.contents())
		cost += price(n.it, pool.capacityType)
	}
	it, p := b.cheapestFit(union, pool.capacityType)
	if it == nil || p >= cost {
		return PlantedMove{}, fmt.Errorf("union %s has no cheaper single node than %.4f", union, cost)
	}
	return PlantedMove{Kind: kind, NodeNames: lo.Map(nodes, func(n *nodeSpec, _ int) string { return n.name }), SavingsPerHour: cost - p}, nil
}

// drainable reports that n's pods first-fit, largest first, into the spare room of others.
func (b *builder) drainable(n *nodeSpec, others []*nodeSpec) bool {
	spare := lo.Map(others, func(o *nodeSpec, _ int) res { return b.avail(o.it).sub(o.contents()) })
	pods := lo.Map(n.pods, func(d *deploymentSpec, _ int) res { return d.req })
	sort.Slice(pods, func(i, j int) bool { return pods[i].cpu > pods[j].cpu })
	for _, p := range pods {
		i := lo.IndexOf(lo.Map(spare, func(s res, _ int) bool { return p.fits(s) }), true)
		if i < 0 {
			return false
		}
		spare[i] = spare[i].sub(p)
	}
	return true
}
