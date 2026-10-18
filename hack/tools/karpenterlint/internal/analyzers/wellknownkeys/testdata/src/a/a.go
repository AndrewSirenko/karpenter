package a

import v1 "pkg/apis/v1"

var (
	_ = v1.DoNotDisruptAnnotationKey
	_ = "karpenter.sh/do-not-disrupt"            // want `reference the constant from pkg/apis`
	_ = "compatibility.karpenter.sh/provider"    // want `reference the constant from pkg/apis`
	_ = "karpenter.sh/disruption=disrupting"     // want `reference the constant from pkg/apis`
	_ = `pod has "karpenter.sh/do-not-disrupt" annotation`
	_ = "https://karpenter.sh/docs/concepts/disruption/"
	_ = "karpenter.sh"
)
