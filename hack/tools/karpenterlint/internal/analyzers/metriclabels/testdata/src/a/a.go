package a

import opmetrics "github.com/awslabs/operatorpkg/metrics"

const (
	NodePoolLabel = "nodepool"
	ZoneLabel     = "zone"
)

var (
	NodePool = opmetrics.Label{Name: NodePoolLabel} // want NodePool:`label\(nodepool\)`
	Zone     = opmetrics.Label{Name: ZoneLabel}     // want Zone:`label\(zone\)`
)

var (
	TerminatedTotal = opmetrics.NewPrometheusCounter(nil, nil, []opmetrics.Label{NodePool, Zone}, opmetrics.GA) // want TerminatedTotal:`metric\(nodepool,zone\)`
	Allowed         = opmetrics.NewPrometheusGauge(nil, nil, []opmetrics.Label{NodePool, {Name: "reason"}}, opmetrics.GA) // want Allowed:`metric\(nodepool,reason\)`
)

func emit(labels map[string]string) {
	TerminatedTotal.Inc(map[string]string{NodePoolLabel: "a", ZoneLabel: "b"})
	TerminatedTotal.Inc(map[string]string{NodePoolLabel: "a"}) // want `TerminatedTotal.Inc labels do not match the declaration \(missing zone\)`
	TerminatedTotal.Add(1, map[string]string{NodePoolLabel: "a", ZoneLabel: "b", "extra": "c"}) // want `undeclared extra`
	TerminatedTotal.Delete(map[string]string{NodePoolLabel: "a"}) // want `missing zone`
	TerminatedTotal.DeletePartialMatch(map[string]string{NodePoolLabel: "a"})
	TerminatedTotal.Inc(labels)
	Allowed.Set(1, map[string]string{NodePoolLabel: "a", "reason": "b"})
	Allowed.Set(1, map[string]string{"reason": "b"}) // want `missing nodepool`
}
