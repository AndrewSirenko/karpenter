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

	"github.com/awslabs/operatorpkg/option"
	corev1 "k8s.io/api/core/v1"
)

// CommandPhase is the point in a disruption command's life an observer is told about.
type CommandPhase string

const (
	// CommandStarted means the candidates are marked and the replacements are created.
	CommandStarted CommandPhase = "started"
	// CommandStartFailed means the command was dropped before it disrupted anything.
	CommandStartFailed CommandPhase = "start_failed"
	// CommandCompleted means the command left the queue. Command.Succeeded tells whether it finished.
	CommandCompleted CommandPhase = "completed"
)

// CommandObserver is told about each disruption command, for example to record it outside the cluster.
type CommandObserver interface {
	// OnCommand must not block or modify the command.
	OnCommand(ctx context.Context, cmd *Command, phase CommandPhase)
}

type QueueOptions struct {
	observer CommandObserver
}

// WithCommandObserver registers an observer of disruption commands.
func WithCommandObserver(observer CommandObserver) option.Function[QueueOptions] {
	return func(o *QueueOptions) {
		o.observer = observer
	}
}

func (q *Queue) observeCommand(ctx context.Context, cmd *Command, phase CommandPhase) {
	if q.observer == nil {
		return
	}
	q.observer.OnCommand(ctx, cmd, phase)
}

// InstanceTypeName is the candidate's instance type, or "" if it's unknown.
func (c *Candidate) InstanceTypeName() string {
	if c.instanceType == nil {
		return ""
	}
	return c.instanceType.Name
}

func (c *Candidate) Zone() string { return c.zone }

func (c *Candidate) CapacityType() string { return c.capacityType }

// ReschedulablePods are the pods that would move off the candidate.
func (c *Candidate) ReschedulablePods() []*corev1.Pod { return c.reschedulablePods }
