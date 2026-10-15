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

package performancev2

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	"github.com/samber/lo"

	"sigs.k8s.io/karpenter/test/pkg/environment/common"
)

// getPodsDisruptedCount fetches the total pod disruption count from Prometheus metrics
func getPodsDisruptedCount(env *common.Environment) int {
	metrics := env.ExpectPodMetrics()
	disruptionMetrics := lo.Filter(metrics, func(m common.PrometheusMetric, _ int) bool {
		return m.Name == "karpenter_pods_drained_total"
	})

	totalDisrupted := 0
	for _, metric := range disruptionMetrics {
		totalDisrupted += int(metric.Value)
	}
	return totalDisrupted
}

// OutputPerformanceReport outputs a performance report to console and file
func OutputPerformanceReport(report *PerformanceReport, filePrefix string) {
	GinkgoWriter.Printf("\n=== %s PERFORMANCE REPORT ===\n", report.TestType)
	GinkgoWriter.Printf("Test: %s\n", report.TestName)
	GinkgoWriter.Printf("Type: %s\n", report.TestType)
	GinkgoWriter.Printf("Total Time: %v\n", report.TotalTime)
	GinkgoWriter.Printf("Total Pods: %d (Net Change: %+d)\n", report.TotalPods, report.PodsNetChange)
	GinkgoWriter.Printf("Total Nodes: %d (Net Change: %+d)\n", report.TotalNodes, report.NodesNetChange)
	GinkgoWriter.Printf("Pods Disrupted: %d\n", report.PodsDisrupted)
	GinkgoWriter.Printf("CPU Utilization: %.2f%%\n", report.TotalReservedCPUUtil*100)
	GinkgoWriter.Printf("Memory Utilization: %.2f%%\n", report.TotalReservedMemoryUtil*100)
	GinkgoWriter.Printf("Efficiency Score: %.1f%%\n", report.ResourceEfficiencyScore)
	GinkgoWriter.Printf("Pods per Node: %.1f\n", report.PodsPerNode)
	GinkgoWriter.Printf("Rounds: %d\n", report.Rounds)

	// Karpenter pod resource usage (from Kubernetes Metrics API)
	if report.MetricsSampleCount > 0 {
		GinkgoWriter.Printf("Karpenter Memory (P95/Avg/Max): %.2f / %.2f / %.2f MB (%d samples)\n",
			report.KarpenterP95MemoryMB, report.KarpenterAvgMemoryMB, report.KarpenterMaxMemoryMB, report.MetricsSampleCount)
		GinkgoWriter.Printf("Karpenter Heap (P95/Avg/Max): %.2f / %.2f / %.2f MB (%d samples)\n",
			report.KarpenterP95HeapMB, report.KarpenterAvgHeapMB, report.KarpenterMaxHeapMB, report.MetricsSampleCount)
		GinkgoWriter.Printf("Karpenter CPU (P95/Avg/Max): %.4f / %.4f / %.4f cores (%d samples)\n",
			report.KarpenterP95CPUCores, report.KarpenterAvgCPUCores, report.KarpenterMaxCPUCores, report.MetricsSampleCount)
	} else {
		GinkgoWriter.Printf("Karpenter Metrics: Not available (0 samples collected)\n")
	}

	// File output
	if outputDir := os.Getenv("OUTPUT_DIR"); outputDir != "" {
		writeReportFiles(report, filePrefix, outputDir)
	}
}

// writeReportFiles persists the report JSON and any profile data under
// outputDir. The caller-supplied filePrefix is sanitized to prevent path
// traversal via the OUTPUT_DIR environment variable or the prefix itself.
func writeReportFiles(report *PerformanceReport, filePrefix, outputDir string) {
	safePrefix := filepath.Base(filepath.Clean(filePrefix))
	writeUnder := func(name string, data []byte) (string, error) {
		return common.WriteArtifactUnder(outputDir, name, data)
	}
	if reportJSON, err := json.MarshalIndent(report, "", "  "); err == nil {
		if path, err := writeUnder(fmt.Sprintf("%s_performance_report.json", safePrefix), reportJSON); err == nil {
			GinkgoWriter.Printf("Report written to: %s\n", path)
		}
	}
	if len(report.MemoryProfileData) > 0 {
		if path, err := writeUnder(fmt.Sprintf("karpenter_memory_profile_%s.pb.gz", safePrefix), report.MemoryProfileData); err == nil {
			GinkgoWriter.Printf("Memory profile saved to: %s\n", path)
		}
	}
	if len(report.CPUProfileData) > 0 {
		if path, err := writeUnder(fmt.Sprintf("karpenter_cpu_profile_%s.pb.gz", safePrefix), report.CPUProfileData); err == nil {
			GinkgoWriter.Printf("CPU profile saved to: %s\n", path)
		}
	}
}
