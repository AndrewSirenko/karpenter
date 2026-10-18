package metrics

type Label struct {
	Name string
	Help string
}

type Stage string

const GA Stage = "ga"

type CounterMetric interface {
	Inc(labels map[string]string)
	Add(v float64, labels map[string]string)
	Delete(labels map[string]string)
	DeletePartialMatch(labels map[string]string)
}

type GaugeMetric interface {
	Set(v float64, labels map[string]string)
	Delete(labels map[string]string)
}

func NewPrometheusCounter(registry any, opts any, labels []Label, stage Stage) CounterMetric { return nil }

func NewPrometheusGauge(registry any, opts any, labels []Label, stage Stage) GaugeMetric { return nil }
