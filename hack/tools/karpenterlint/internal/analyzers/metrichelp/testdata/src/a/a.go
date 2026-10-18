package a

import "github.com/prometheus/client_golang/prometheus"

var (
	_ = prometheus.CounterOpts{Name: "queue_failures_total", Help: "Number of failures. Labeled by disruption method."} // want `Help lists labels in prose`
	_ = prometheus.GaugeOpts{Name: "x", Help: "Allowed disruptions, labeled by nodepool."}                            // want `Help lists labels in prose`
	_ = prometheus.GaugeOpts{Name: "y", Help: "Number of allowed disruptions."}
)
