package client

import "context"

type Object interface{ GetName() string }

type Patch interface{ Type() string }

type mergeFromPatch struct{}

func (mergeFromPatch) Type() string { return "merge" }

type MergeFromOption interface{ ApplyToMergeFrom() }

type MergeFromWithOptimisticLock struct{}

func (MergeFromWithOptimisticLock) ApplyToMergeFrom() {}

func MergeFrom(obj Object) Patch { return mergeFromPatch{} }

func MergeFromWithOptions(obj Object, opts ...MergeFromOption) Patch { return mergeFromPatch{} }

type SubResourceWriter interface {
	Patch(ctx context.Context, obj Object, patch Patch) error
}

type Client interface {
	Patch(ctx context.Context, obj Object, patch Patch) error
	Status() SubResourceWriter
}
