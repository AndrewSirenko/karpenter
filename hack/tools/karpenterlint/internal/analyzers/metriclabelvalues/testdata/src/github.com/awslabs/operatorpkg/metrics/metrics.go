package metrics

import "github.com/awslabs/operatorpkg/docs"

type Value = docs.Value

type Label struct {
	Name   string
	Values []Value
}

type CounterMetric interface {
	Inc(labels map[string]string)
}

func NewPrometheusCounter(labels []Label) CounterMetric { return nil }
