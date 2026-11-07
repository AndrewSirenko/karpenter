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

package disruptionbench_test

import (
	"context"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"sigs.k8s.io/karpenter/pkg/controllers/disruption"
	"sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/test"
	"sigs.k8s.io/karpenter/pkg/test/disruptionbench"
)

var ctx context.Context

func TestDisruptionBench(t *testing.T) {
	ctx = options.ToContext(context.Background(), test.Options())
	RegisterFailHandler(Fail)
	RunSpecs(t, "DisruptionBench")
}

// smallSpec is small enough to brute-force every move of up to four nodes.
func smallSpec(seed int64) disruptionbench.Spec {
	return disruptionbench.Spec{
		Seed: seed, Nodes: 30, PodsPerNode: 6, NodePools: 1, Zones: 2, DaemonSets: 2,
		AntiAffinity: 0.1, ZonalTSC: 0.1, HostnameTSC: 0.1, PDBs: true,
		Plants: []disruptionbench.Plant{
			{Kind: disruptionbench.PlantComplementPair, Count: 1},
			{Kind: disruptionbench.PlantSmallMerge, Count: 1},
			{Kind: disruptionbench.PlantHiddenDelete, Count: 1},
		},
	}
}

type nopValidator struct{}

func (nopValidator) Validate(_ context.Context, cmd disruption.Command, _ time.Duration) (disruption.Command, error) {
	return cmd, nil
}

func todayMethods(env *disruptionbench.Env) []disruption.Method {
	c := disruption.MakeConsolidation(env.Clock, env.Cluster, env.Client, env.Provisioner, env.CloudProvider, env.Recorder, env.Queue)
	return []disruption.Method{
		disruption.NewEmptiness(c, disruption.WithValidator(nopValidator{})),
		disruption.NewMultiNodeConsolidation(c, disruption.WithValidator(nopValidator{})),
		disruption.NewSingleNodeConsolidation(c, disruption.WithValidator(nopValidator{})),
	}
}

func mustEnv(f *disruptionbench.Fixture) *disruptionbench.Env {
	env, err := disruptionbench.NewEnv(ctx, f)
	Expect(err).ToNot(HaveOccurred())
	return env
}
