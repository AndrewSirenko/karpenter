package b

import "a"

// Shape of kubernetes-sigs/karpenter#3359: declaration gained a label, another package's call site did not.
func emit() {
	a.TerminatedTotal.Inc(map[string]string{a.NodePoolLabel: "x"}) // want `missing zone`
	a.TerminatedTotal.Inc(map[string]string{a.NodePoolLabel: "x", a.ZoneLabel: "y"})
}
