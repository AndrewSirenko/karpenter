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

// Package disruptionbench builds large in-memory clusters for consolidation benchmarks.
package disruptionbench

import (
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"

	"k8s.io/utils/clock/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/fake"
	"sigs.k8s.io/karpenter/pkg/controllers/disruption"
	"sigs.k8s.io/karpenter/pkg/controllers/provisioning"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/events"
)

// Spec describes a cluster to generate. Owned by generate.go.
type Spec struct {
	Seed          int64
	Nodes         int
	PodsPerNode   int
	NodePools     int
	InstanceTypes int
	Zones         int
	DaemonSets    int
	// Fractions of workload pods carrying each constraint.
	AntiAffinity float64
	ZonalTSC     float64
	HostnameTSC  float64
	PDBs         bool
	SpotFraction float64
	Plants       []Plant
}

// PlantKind is a known-good move hidden in the generated cluster.
type PlantKind string

const (
	// PlantComplementPair is two half-full nodes with opposite cpu:mem skew that fit one cheaper node (kubernetes-sigs/karpenter#1962).
	PlantComplementPair PlantKind = "complement-pair"
	// PlantSmallMerge is a group of small nodes that fit one bigger, cheaper-in-total node.
	PlantSmallMerge PlantKind = "small-merge"
	// PlantHiddenDelete is a node whose pods fit spare capacity elsewhere, placed late in the default sort order.
	PlantHiddenDelete PlantKind = "hidden-delete"
)

type Plant struct {
	Kind  PlantKind
	Count int
}

// PlantedMove is ground truth: removing NodeNames saves SavingsPerHour.
type PlantedMove struct {
	Kind           PlantKind
	NodeNames      []string
	SavingsPerHour float64
}

// Fixture is plain objects; it knows nothing about controllers. Produced by Generate, consumed by NewEnv.
type Fixture struct {
	NodePools     []*v1.NodePool
	NodeClaims    []*v1.NodeClaim
	Nodes         []*corev1.Node
	Pods          []*corev1.Pod
	PDBs          []*policyv1.PodDisruptionBudget
	DaemonSets    []*appsv1.DaemonSet
	InstanceTypes []*cloudprovider.InstanceType
	Planted       []PlantedMove
}

// Env is a Fixture loaded into a fake client and a synced state.Cluster. NewEnv lives in env.go.
type Env struct {
	Fixture       *Fixture
	Client        client.Client
	Cluster       *state.Cluster
	CloudProvider *fake.CloudProvider
	Provisioner   *provisioning.Provisioner
	Clock         *testing.FakeClock
	Recorder      events.Recorder
	Queue         *disruption.Queue
}
