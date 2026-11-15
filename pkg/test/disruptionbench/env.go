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
	"context"
	"fmt"
	"reflect"
	"sync"
	"time"

	"github.com/awslabs/operatorpkg/status"
	"github.com/google/uuid"
	"github.com/samber/lo"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	clocktesting "k8s.io/utils/clock/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	fakecr "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/fake"
	"sigs.k8s.io/karpenter/pkg/controllers/disruption"
	"sigs.k8s.io/karpenter/pkg/controllers/provisioning"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/events"
)

// NewEnv loads f into an in-memory client and a synced state.Cluster. It takes ownership of f's objects and marks
// NodePools ready and NodeClaims and Nodes launched, registered and initialized, so every node is a consolidation
// candidate. ctx must carry operator options.
//
// Reads are served from an indexed store that deep-copies like the controller-runtime cache, since the fake client
// JSON round-trips every List. Writes go through a fake client and are propagated to the store and to cluster state,
// as the state informers would.
func NewEnv(ctx context.Context, f *Fixture) (*Env, error) {
	clk := clocktesting.NewFakeClock(time.Now())
	prepare(f, clk)

	objects := make([]client.Object, 0, len(f.NodePools)+len(f.NodeClaims)+len(f.Nodes)+len(f.Pods)+len(f.PDBs)+len(f.DaemonSets))
	for _, o := range f.NodePools {
		objects = append(objects, o)
	}
	for _, o := range f.NodeClaims {
		objects = append(objects, o)
	}
	for _, o := range f.Nodes {
		objects = append(objects, o)
	}
	for _, o := range f.Pods {
		objects = append(objects, o)
	}
	for _, o := range f.PDBs {
		objects = append(objects, o)
	}
	for _, o := range f.DaemonSets {
		objects = append(objects, o)
	}

	s := newStore(scheme.Scheme)
	s.index(&corev1.Pod{}, "spec.nodeName", func(o client.Object) []string { return []string{o.(*corev1.Pod).Spec.NodeName} })
	s.index(&corev1.Node{}, "spec.providerID", func(o client.Object) []string { return []string{o.(*corev1.Node).Spec.ProviderID} })
	s.index(&v1.NodeClaim{}, "status.providerID", func(o client.Object) []string { return []string{o.(*v1.NodeClaim).Status.ProviderID} })
	s.index(&v1.NodeClaim{}, "spec.nodeClassRef.group", func(o client.Object) []string { return []string{o.(*v1.NodeClaim).Spec.NodeClassRef.Group} })
	s.index(&v1.NodeClaim{}, "spec.nodeClassRef.kind", func(o client.Object) []string { return []string{o.(*v1.NodeClaim).Spec.NodeClassRef.Kind} })
	s.index(&v1.NodeClaim{}, "spec.nodeClassRef.name", func(o client.Object) []string { return []string{o.(*v1.NodeClaim).Spec.NodeClassRef.Name} })
	s.index(&v1.NodePool{}, "spec.template.spec.nodeClassRef.group", func(o client.Object) []string {
		return []string{o.(*v1.NodePool).Spec.Template.Spec.NodeClassRef.Group}
	})
	s.index(&v1.NodePool{}, "spec.template.spec.nodeClassRef.kind", func(o client.Object) []string {
		return []string{o.(*v1.NodePool).Spec.Template.Spec.NodeClassRef.Kind}
	})
	s.index(&v1.NodePool{}, "spec.template.spec.nodeClassRef.name", func(o client.Object) []string {
		return []string{o.(*v1.NodePool).Spec.Template.Spec.NodeClassRef.Name}
	})
	builder := fakecr.NewClientBuilder().
		WithObjects(objects...).
		WithStatusSubresource(&v1.NodeClaim{}, &v1.NodePool{})
	for gvk, fields := range s.indexers {
		obj, err := s.scheme.New(gvk)
		if err != nil {
			return nil, err
		}
		for field, fn := range fields {
			builder = builder.WithIndex(obj.(client.Object), field, fn)
		}
	}
	kubeClient := builder.WithInterceptorFuncs(s.interceptor()).Build()
	for _, o := range objects {
		if err := s.upsert(o); err != nil {
			return nil, err
		}
	}

	cp := fake.NewCloudProvider()
	cp.InstanceTypes = f.InstanceTypes
	cluster := state.NewCluster(clk, kubeClient, cp)
	s.onChange = func(ctx context.Context, obj client.Object, deleted bool) { propagate(ctx, cluster, obj, deleted) }

	for _, nc := range f.NodeClaims {
		cluster.UpdateNodeClaim(nc.DeepCopy())
	}
	for _, n := range f.Nodes {
		if err := cluster.UpdateNode(ctx, n.DeepCopy()); err != nil {
			return nil, fmt.Errorf("updating node %s, %w", n.Name, err)
		}
	}
	for _, p := range f.Pods {
		if err := cluster.UpdatePod(ctx, p.DeepCopy()); err != nil {
			return nil, fmt.Errorf("updating pod %s, %w", client.ObjectKeyFromObject(p), err)
		}
	}
	for _, ds := range f.DaemonSets {
		if err := cluster.UpdateDaemonSet(ctx, ds.DeepCopy()); err != nil {
			return nil, fmt.Errorf("updating daemonset %s, %w", client.ObjectKeyFromObject(ds), err)
		}
	}
	if !cluster.Synced(ctx) {
		return nil, fmt.Errorf("cluster state not synced")
	}

	recorder := events.NewRecorder(&record.FakeRecorder{})
	prov := provisioning.NewProvisioner(kubeClient, recorder, cp, cluster, clk, nil, nil, nil)
	return &Env{
		Fixture:       f,
		Client:        kubeClient,
		Cluster:       cluster,
		CloudProvider: cp,
		Provisioner:   prov,
		Clock:         clk,
		Recorder:      recorder,
		Queue:         disruption.NewQueue(kubeClient, recorder, cluster, clk, prov),
	}, nil
}

// NopValidator accepts every command without waiting out the validation period.
type NopValidator struct{}

func (NopValidator) Validate(_ context.Context, cmd disruption.Command, _ time.Duration) (disruption.Command, error) {
	return cmd, nil
}

// Methods are the consolidation methods built over an Env with NopValidator.
type Methods struct {
	Emptiness  *disruption.Emptiness
	MultiNode  *disruption.MultiNodeConsolidation
	SingleNode *disruption.SingleNodeConsolidation
}

// Methods returns fresh consolidation methods. Each method remembers when it last found nothing, so build new ones to
// rerun a search over an unchanged cluster.
func (e *Env) Methods() Methods {
	c := disruption.MakeConsolidation(e.Clock, e.Cluster, e.Client, e.Provisioner, e.CloudProvider, e.Recorder, e.Queue)
	return Methods{
		Emptiness:  disruption.NewEmptiness(c, disruption.WithValidator(NopValidator{})),
		MultiNode:  disruption.NewMultiNodeConsolidation(c, disruption.WithValidator(NopValidator{})),
		SingleNode: disruption.NewSingleNodeConsolidation(c, disruption.WithValidator(NopValidator{})),
	}
}

// Candidates returns the candidates the consolidation methods filter for.
func (e *Env) Candidates(ctx context.Context) ([]*disruption.Candidate, error) {
	m := e.Methods().SingleNode
	return disruption.GetCandidates(ctx, e.Cluster, e.Client, e.Recorder, e.Clock, e.CloudProvider, m.ShouldDisrupt, m.Class(), e.Queue)
}

// ComputeCommands runs one method the way the disruption controller does, without starting the commands.
func (e *Env) ComputeCommands(ctx context.Context, m disruption.Method) ([]disruption.Command, error) {
	candidates, totals, err := disruption.GetCandidatesWithTotals(ctx, e.Cluster, e.Client, e.Recorder, e.Clock, e.CloudProvider, m.ShouldDisrupt, m.Class(), e.Queue, nil)
	if err != nil {
		return nil, fmt.Errorf("determining candidates, %w", err)
	}
	if len(candidates) == 0 {
		return nil, nil
	}
	if setter, ok := m.(disruption.NodePoolTotalsSetter); ok {
		setter.SetNodePoolTotals(totals)
	}
	budgets, err := disruption.BuildDisruptionBudgetMapping(ctx, e.Cluster, e.Clock, e.Client, e.CloudProvider, e.Recorder, m.Reason())
	if err != nil {
		return nil, fmt.Errorf("building disruption budgets, %w", err)
	}
	cmds, err := m.ComputeCommands(ctx, budgets, candidates...)
	if err != nil {
		return nil, err
	}
	return lo.Filter(cmds, func(c disruption.Command, _ int) bool { return c.Decision() != disruption.NoOpDecision }), nil
}

// StartCommand fills in the fields the disruption controller sets and starts cmd on the queue.
func (e *Env) StartCommand(ctx context.Context, m disruption.Method, cmd *disruption.Command) error {
	cmd.CreationTimestamp = e.Clock.Now()
	cmd.ID = uuid.New()
	cmd.Method = m
	return e.Queue.StartCommand(ctx, cmd)
}

// prepare makes f's objects look like a settled cluster.
func prepare(f *Fixture, clk *clocktesting.FakeClock) {
	for _, np := range f.NodePools {
		np.Namespace = ""
		if np.Spec.Disruption.ConsolidationPolicy == "" {
			np.Spec.Disruption.ConsolidationPolicy = v1.ConsolidationPolicyWhenEmptyOrUnderutilized
		}
		if np.Spec.Disruption.ConsolidateAfter.Duration == nil {
			np.Spec.Disruption.ConsolidateAfter = v1.MustParseNillableDuration("0s")
		}
		if len(np.Spec.Disruption.Budgets) == 0 {
			np.Spec.Disruption.Budgets = []v1.Budget{{Nodes: "10%"}}
		}
		np.StatusConditions(status.WithClock(clk)).SetTrue(status.ConditionReady)
	}
	for _, nc := range f.NodeClaims {
		nc.Namespace = ""
		conditions := nc.StatusConditions(status.WithClock(clk))
		conditions.SetTrue(v1.ConditionTypeLaunched)
		conditions.SetTrue(v1.ConditionTypeRegistered)
		conditions.SetTrue(v1.ConditionTypeInitialized)
		conditions.SetTrue(v1.ConditionTypeConsolidatable)
	}
	for _, n := range f.Nodes {
		// pkg/test gives Nodes a namespace, which cluster-scoped lookups don't carry.
		n.Namespace = ""
		if n.Labels == nil {
			n.Labels = map[string]string{}
		}
		n.Labels[v1.NodeRegisteredLabelKey] = "true"
		n.Labels[v1.NodeInitializedLabelKey] = "true"
		n.Spec.Taints = lo.Reject(n.Spec.Taints, func(t corev1.Taint, _ int) bool { return t.MatchTaint(&v1.UnregisteredNoExecuteTaint) })
		n.Status.Phase = corev1.NodeRunning
		n.Status.Conditions = []corev1.NodeCondition{{
			Type:               corev1.NodeReady,
			Status:             corev1.ConditionTrue,
			LastHeartbeatTime:  metav1.NewTime(clk.Now()),
			LastTransitionTime: metav1.NewTime(clk.Now()),
			Reason:             "KubeletReady",
		}}
	}
	for _, p := range f.Pods {
		if p.Spec.NodeName != "" && p.Status.Phase == corev1.PodPending {
			p.Status.Phase = corev1.PodRunning
		}
	}
}

// propagate updates cluster state for a written object, as the state informers would.
func propagate(ctx context.Context, cluster *state.Cluster, obj client.Object, deleted bool) {
	switch o := obj.(type) {
	case *corev1.Node:
		if deleted {
			cluster.DeleteNode(o.Name)
			return
		}
		_ = cluster.UpdateNode(ctx, o.DeepCopy())
	case *v1.NodeClaim:
		if deleted {
			cluster.DeleteNodeClaim(o.Name)
			return
		}
		cluster.UpdateNodeClaim(o.DeepCopy())
	case *corev1.Pod:
		if deleted {
			cluster.DeletePod(client.ObjectKeyFromObject(o))
			return
		}
		_ = cluster.UpdatePod(ctx, o.DeepCopy())
	case *appsv1.DaemonSet:
		if deleted {
			cluster.DeleteDaemonSet(client.ObjectKeyFromObject(o))
			return
		}
		_ = cluster.UpdateDaemonSet(ctx, o.DeepCopy())
	}
}

// store is an indexed object cache with controller-runtime cache read semantics.
type store struct {
	mu       sync.RWMutex
	scheme   *runtime.Scheme
	kinds    map[schema.GroupVersionKind]*kindStore
	indexers map[schema.GroupVersionKind]map[string]client.IndexerFunc
	onChange func(ctx context.Context, obj client.Object, deleted bool)
}

type kindStore struct {
	objs  map[client.ObjectKey]client.Object
	pos   map[client.ObjectKey]int
	order []client.ObjectKey
	dead  int
	// byField maps field -> value -> keys, in insertion order.
	byField map[string]map[string][]client.ObjectKey
}

func newStore(s *runtime.Scheme) *store {
	return &store{
		scheme:   s,
		kinds:    map[schema.GroupVersionKind]*kindStore{},
		indexers: map[schema.GroupVersionKind]map[string]client.IndexerFunc{},
	}
}

func (s *store) index(obj client.Object, field string, fn client.IndexerFunc) {
	gvk := lo.Must(apiutil.GVKForObject(obj, s.scheme))
	if s.indexers[gvk] == nil {
		s.indexers[gvk] = map[string]client.IndexerFunc{}
	}
	s.indexers[gvk][field] = fn
}

func (s *store) kind(gvk schema.GroupVersionKind) *kindStore {
	ks, ok := s.kinds[gvk]
	if !ok {
		ks = &kindStore{objs: map[client.ObjectKey]client.Object{}, pos: map[client.ObjectKey]int{}, byField: map[string]map[string][]client.ObjectKey{}}
		s.kinds[gvk] = ks
	}
	return ks
}

func (s *store) upsert(obj client.Object) error {
	gvk, err := apiutil.GVKForObject(obj, s.scheme)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ks := s.kind(gvk)
	key := client.ObjectKeyFromObject(obj)
	if old, ok := ks.objs[key]; ok {
		s.unindex(gvk, ks, key, old)
	} else {
		ks.pos[key] = len(ks.order)
		ks.order = append(ks.order, key)
	}
	ks.objs[key] = obj
	for field, fn := range s.indexers[gvk] {
		if ks.byField[field] == nil {
			ks.byField[field] = map[string][]client.ObjectKey{}
		}
		for _, v := range fn(obj) {
			ks.byField[field][v] = append(ks.byField[field][v], key)
		}
	}
	return nil
}

func (s *store) remove(gvk schema.GroupVersionKind, key client.ObjectKey) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ks := s.kind(gvk)
	old, ok := ks.objs[key]
	if !ok {
		return
	}
	s.unindex(gvk, ks, key, old)
	delete(ks.objs, key)
	ks.order[ks.pos[key]] = client.ObjectKey{}
	delete(ks.pos, key)
	if ks.dead++; ks.dead > len(ks.order)/2 {
		ks.order = lo.Filter(ks.order, func(k client.ObjectKey, _ int) bool { return k != client.ObjectKey{} })
		for i, k := range ks.order {
			ks.pos[k] = i
		}
		ks.dead = 0
	}
}

func (s *store) unindex(gvk schema.GroupVersionKind, ks *kindStore, key client.ObjectKey, old client.Object) {
	for field, fn := range s.indexers[gvk] {
		for _, v := range fn(old) {
			ks.byField[field][v] = lo.Without(ks.byField[field][v], key)
		}
	}
}

func (s *store) get(key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	gvk, err := apiutil.GVKForObject(obj, s.scheme)
	if err != nil {
		return err
	}
	o := client.GetOptions{}
	o.ApplyOptions(opts)
	s.mu.RLock()
	ks := s.kind(gvk)
	stored, ok := ks.objs[key]
	if !ok && key.Namespace != "" {
		// The API server ignores the namespace when getting a cluster-scoped object.
		stored, ok = ks.objs[client.ObjectKey{Name: key.Name}]
	}
	s.mu.RUnlock()
	if !ok {
		gvr, _ := meta.UnsafeGuessKindToResource(gvk)
		return apierrors.NewNotFound(gvr.GroupResource(), key.Name)
	}
	if o.UnsafeDisableDeepCopy == nil || !*o.UnsafeDisableDeepCopy {
		stored = stored.DeepCopyObject().(client.Object)
	}
	reflect.Indirect(reflect.ValueOf(obj)).Set(reflect.Indirect(reflect.ValueOf(stored)))
	obj.GetObjectKind().SetGroupVersionKind(gvk)
	return nil
}

func (s *store) list(list client.ObjectList, opts ...client.ListOption) error {
	listGVK, err := apiutil.GVKForObject(list, s.scheme)
	if err != nil {
		return err
	}
	gvk := listGVK.GroupVersion().WithKind(listGVK.Kind[:len(listGVK.Kind)-len("List")])
	o := client.ListOptions{}
	o.ApplyOptions(opts)
	var match []fieldMatch
	if o.FieldSelector != nil {
		for _, req := range o.FieldSelector.Requirements() {
			if req.Operator != selection.Equals && req.Operator != selection.DoubleEquals {
				return fmt.Errorf("field selector %s is not an exact match", o.FieldSelector)
			}
			fn, ok := s.indexers[gvk][req.Field]
			if !ok {
				return fmt.Errorf("no index %s for %s", req.Field, gvk)
			}
			match = append(match, fieldMatch{field: req.Field, value: req.Value, fn: fn})
		}
	}
	deepCopy := o.UnsafeDisableDeepCopy == nil || !*o.UnsafeDisableDeepCopy

	s.mu.RLock()
	ks := s.kind(gvk)
	keys := ks.order
	if len(match) > 0 {
		keys = ks.byField[match[0].field][match[0].value]
	}
	out := make([]runtime.Object, 0, len(keys))
	for _, key := range keys {
		obj, ok := ks.objs[key]
		if !ok {
			continue
		}
		if o.Namespace != "" && key.Namespace != o.Namespace {
			continue
		}
		if o.LabelSelector != nil && !o.LabelSelector.Matches(labels.Set(obj.GetLabels())) {
			continue
		}
		if !lo.EveryBy(match[min(1, len(match)):], func(m fieldMatch) bool { return lo.Contains(m.fn(obj), m.value) }) {
			continue
		}
		if deepCopy {
			out = append(out, obj.DeepCopyObject())
		} else {
			out = append(out, obj)
		}
	}
	s.mu.RUnlock()
	return meta.SetList(list, out)
}

type fieldMatch struct {
	field string
	value string
	fn    client.IndexerFunc
}

// sync refreshes the store from the backing client after a write and propagates the change.
func (s *store) sync(ctx context.Context, c client.Reader, obj client.Object) {
	gvk, err := apiutil.GVKForObject(obj, s.scheme)
	if err != nil {
		return
	}
	key := client.ObjectKeyFromObject(obj)
	fresh := obj.DeepCopyObject().(client.Object)
	if err := c.Get(ctx, key, fresh); err != nil {
		if apierrors.IsNotFound(err) {
			s.remove(gvk, key)
			if s.onChange != nil {
				s.onChange(ctx, obj, true)
			}
		}
		return
	}
	if err := s.upsert(fresh); err != nil {
		return
	}
	if s.onChange != nil {
		s.onChange(ctx, fresh, false)
	}
}

func (s *store) interceptor() interceptor.Funcs {
	return interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if !s.typed(obj) {
				return c.Get(ctx, key, obj, opts...)
			}
			return s.get(key, obj, opts...)
		},
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if !s.typed(list) {
				return c.List(ctx, list, opts...)
			}
			return s.list(list, opts...)
		},
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if err := c.Create(ctx, obj, opts...); err != nil {
				return err
			}
			s.sync(ctx, c, obj)
			return nil
		},
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if err := c.Update(ctx, obj, opts...); err != nil {
				return err
			}
			s.sync(ctx, c, obj)
			return nil
		},
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if err := c.Patch(ctx, obj, patch, opts...); err != nil {
				return err
			}
			s.sync(ctx, c, obj)
			return nil
		},
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			if err := c.Delete(ctx, obj, opts...); err != nil {
				return err
			}
			s.sync(ctx, c, obj)
			return nil
		},
		DeleteAllOf: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteAllOfOption) error {
			return fmt.Errorf("DeleteAllOf is not supported by disruptionbench")
		},
		SubResourceCreate: func(ctx context.Context, c client.Client, sub string, obj client.Object, subResource client.Object, opts ...client.SubResourceCreateOption) error {
			if err := c.SubResource(sub).Create(ctx, obj, subResource, opts...); err != nil {
				return err
			}
			s.sync(ctx, c, obj)
			return nil
		},
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			if err := c.SubResource(sub).Update(ctx, obj, opts...); err != nil {
				return err
			}
			s.sync(ctx, c, obj)
			return nil
		},
		SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
			if err := c.SubResource(sub).Patch(ctx, obj, patch, opts...); err != nil {
				return err
			}
			s.sync(ctx, c, obj)
			return nil
		},
	}
}

// typed reports whether the store can serve obj. Unstructured and metadata-only reads go to the backing client.
func (s *store) typed(obj runtime.Object) bool {
	switch obj.(type) {
	case runtime.Unstructured, *metav1.PartialObjectMetadata, *metav1.PartialObjectMetadataList:
		return false
	}
	return true
}
