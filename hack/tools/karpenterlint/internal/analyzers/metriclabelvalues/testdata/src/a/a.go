package a

import (
	opmetrics "github.com/awslabs/operatorpkg/metrics"

	v1 "example.com/pkg/apis/v1"
)

const (
	DecisionNoOp        = "no-op"
	PolicyWhenEmpty     = "WhenEmptyOrUnderutilized"
	ReasonDrifted       = "drifted"
	ConsolidationPolicy = ""
)

var Policy = opmetrics.Label{
	Name: "consolidation_policy",
	Values: []opmetrics.Value{
		{Name: "when_empty"},
		{Name: string(v1.PhasePending)},
		{Name: PolicyWhenEmpty}, // want `"WhenEmptyOrUnderutilized" is not lowercase snake_case`
	},
}

var Total = opmetrics.NewPrometheusCounter(nil)

func emit(name string) {
	Total.Inc(map[string]string{"reason": ReasonDrifted, "nodepool": name, "capacity_type": v1.CapacityTypeOnDemand})
	Total.Inc(map[string]string{"decision": DecisionNoOp})             // want `"no-op" is not lowercase snake_case`
	Total.Inc(map[string]string{"policy": PolicyWhenEmpty})            // want `not lowercase snake_case`
	Total.Inc(map[string]string{"consolidation_policy": ConsolidationPolicy}) // want `metric label value is empty`
	Total.Inc(map[string]string{"error": "NodeClaimNotFoundError"})    // want `not lowercase snake_case`
}
