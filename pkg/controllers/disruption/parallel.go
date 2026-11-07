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

package disruption

import (
	"context"
	"sync"

	"sigs.k8s.io/karpenter/pkg/events"
	"sigs.k8s.io/karpenter/pkg/operator/options"
)

// SetParallelism overrides --consolidation-parallelism for this method. n < 1 restores the flag value.
func (c *consolidation) SetParallelism(n int) {
	c.parallelismOverride = n
}

func (c *consolidation) parallelism(ctx context.Context) int {
	if c.parallelismOverride > 0 {
		return c.parallelismOverride
	}
	return max(1, options.FromContext(ctx).ConsolidationParallelism)
}

// probeResult is a computeConsolidation result whose events are held back until the caller knows the sequential
// search would have computed it.
type probeResult struct {
	cmd    Command
	err    error
	events []events.Event
}

func (r probeResult) publish(recorder events.Recorder) {
	if len(r.events) > 0 {
		recorder.Publish(r.events...)
	}
}

type bufferedRecorder struct {
	mu     sync.Mutex
	events []events.Event
}

func (b *bufferedRecorder) Publish(evts ...events.Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.events = append(b.events, evts...)
}

// probe runs computeConsolidation with events buffered, so it is safe to discard speculative results.
func (c *consolidation) probe(ctx context.Context, candidates ...*Candidate) probeResult {
	buf := &bufferedRecorder{}
	cc := *c
	cc.recorder = buf
	cmd, err := cc.computeConsolidation(ctx, candidates...)
	return probeResult{cmd: cmd, err: err, events: buf.events}
}

// probeAll runs one probe per candidate set concurrently and returns results in input order.
func (c *consolidation) probeAll(ctx context.Context, sets [][]*Candidate) []probeResult {
	results := make([]probeResult, len(sets))
	var wg sync.WaitGroup
	for i, set := range sets {
		wg.Go(func() { results[i] = c.probe(ctx, set...) })
	}
	wg.Wait()
	return results
}
