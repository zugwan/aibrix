/*
Copyright 2024 The Aibrix Team.

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
package cache

import (
	"context"
	"fmt"
	"strconv"
	"time"

	prometheusv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	dto "github.com/prometheus/client_model/go"
	"github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/utils"
	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

const (
	podPort                             = 8000
	defaultPodMetricRefreshIntervalInMS = 50
)

var (
	counterGaugeMetricNames = []string{
		metrics.NumRequestsRunning,
		metrics.NumRequestsWaiting,
		metrics.NumRequestsSwapped,
		metrics.AvgPromptThroughputToksPerS,
		metrics.AvgGenerationThroughputToksPerS,
		metrics.GPUCacheUsagePerc,
		metrics.CPUCacheUsagePerc,
	}

	// histogram metric example - time_to_first_token_seconds, _sum, _bucket _count.
	histogramMetricNames = []string{
		metrics.IterationTokensTotal,
		metrics.TimeToFirstTokenSeconds,
		metrics.TimePerOutputTokenSeconds,
		metrics.E2ERequestLatencySeconds,
		metrics.RequestQueueTimeSeconds,
		metrics.RequestInferenceTimeSeconds,
		metrics.RequestDecodeTimeSeconds,
		metrics.RequestPrefillTimeSeconds,
	}

	prometheusMetricNames = []string{
		metrics.P95TTFT5m,
		metrics.P95TTFT5mPod,
		metrics.AvgTTFT5mPod,
		metrics.P95TPOT5mPod,
		metrics.AvgTPOT5mPod,
		metrics.AvgPromptToksPerReq,
		metrics.AvgGenerationToksPerReq,
		metrics.AvgE2ELatencyPod,
		metrics.AvgRequestsPerMinPod,
		metrics.AvgPromptThroughputToksPerMinPod,
		metrics.AvgGenerationThroughputToksPerMinPod,
	}

	labelQueryMetricNames = []string{
		metrics.MaxLora,
		metrics.WaitingLoraAdapters,
		metrics.RunningLoraAdapters,
	}
	// TODO: add a helper function for get methods.
	podMetricRefreshInterval = getPodMetricRefreshInterval()
)

func initPrometheusAPI() prometheusv1.API {
	// Load environment variables
	prometheusEndpoint := utils.LoadEnv("PROMETHEUS_ENDPOINT", "")
	prometheusBasicAuthUsername := utils.LoadEnv("PROMETHEUS_BASIC_AUTH_USERNAME", "")
	prometheusBasicAuthPassword := utils.LoadEnv("PROMETHEUS_BASIC_AUTH_PASSWORD", "")

	// Initialize Prometheus API
	var prometheusApi prometheusv1.API
	if prometheusEndpoint != "" {
		api, err := metrics.InitializePrometheusAPI(prometheusEndpoint, prometheusBasicAuthUsername, prometheusBasicAuthPassword)
		if err != nil {
			klog.Errorf("Error initializing Prometheus API: %v", err)
		} else {
			prometheusApi = api
			klog.Infof("Prometheus API initialized successfully")
		}
	}
	return prometheusApi
}

func getPodMetricRefreshInterval() time.Duration {
	value := utils.LoadEnv("AIBRIX_POD_METRIC_REFRESH_INTERVAL_MS", "")
	if value != "" {
		intValue, err := strconv.Atoi(value)
		if err != nil || intValue <= 0 {
			klog.Infof("invalid AIBRIX_POD_METRIC_REFRESH_INTERVAL_MS: %s, falling back to default", value)
		} else {
			klog.Infof("using AIBRIX_POD_METRIC_REFRESH_INTERVAL_MS env value for pod metrics refresh interval: %d ms", intValue)
			return time.Duration(intValue) * time.Millisecond
		}
	}
	klog.Infof("using default refresh interval: %d ms", defaultPodMetricRefreshIntervalInMS)
	return defaultPodMetricRefreshIntervalInMS * time.Millisecond
}

func (c *Store) updatePodMetrics() {
	c.mu.Lock()
	defer c.mu.Unlock()

	readyPods := utils.FilterReadyPods(c.Pods)
	if len(readyPods) == 0 {
		return
	}

	for _, pod := range readyPods {
		podName := pod.Name
		if len(c.PodMetrics[podName]) == 0 {
			c.PodMetrics[podName] = map[string]metrics.MetricValue{}
		}
		if len(c.PodModelMetrics[podName]) == 0 {
			c.PodModelMetrics[podName] = make(map[string]map[string]metrics.MetricValue)
		}

		// We should use the primary container port. In the future, we can decide whether to use sidecar container's port
		url := fmt.Sprintf("http://%s:%d/metrics", pod.Status.PodIP, podPort)
		allMetrics, err := metrics.ParseMetricsURL(url)
		if err != nil {
			klog.V(4).Infof("Error parsing metric families: %v\n", err)
		}

		// parse counterGaugeMetricsNames
		c.updateSimpleMetricFromRawMetricsLocked(pod, allMetrics)

		// parse histogramMetrics
		c.updateHistogramMetricFromRawMetricsLocked(pod, allMetrics)

		// parse QueryLabel metrics
		c.updateQueryLabelMetricFromRawMetricsLocked(pod, allMetrics)

		if c.prometheusApi == nil {
			klog.V(4).InfoS("Prometheus api is not initialized, PROMETHEUS_ENDPOINT is not configured, skip fetching prometheus metrics")
			continue
		}
		// parse prometheus metrics
		c.updateMetricFromPromQLLocked(pod)
	}
}

func (c *Store) updateSimpleMetricFromRawMetricsLocked(pod *v1.Pod, allMetrics map[string]*dto.MetricFamily) {
	podName := pod.Name
	for _, metricName := range counterGaugeMetricNames {
		metric, exists := metrics.Metrics[metricName]
		if !exists {
			klog.V(4).Infof("Cannot find %v in the metric list", metricName)
			continue
		}

		// TODO: we should refact metricName to fit other engine
		metricFamily, exists := allMetrics[fmt.Sprintf("vllm:%s", metricName)]
		if !exists {
			klog.V(4).Infof("Cannot find %v in the pod metrics", metricName)
			continue
		}
		scope := metric.MetricScope
		for _, familyMetric := range metricFamily.Metric {
			modelName, _ := metrics.GetLabelValueForKey(familyMetric, "model_name")

			metricValue, err := metrics.GetCounterGaugeValue(familyMetric, metricFamily.GetType())
			if err != nil {
				klog.V(4).Infof("failed to parse metrics %s from pod %s %s %d: %v", metricName, podName, pod.Status.PodIP, podPort, err)
				continue
			}

			err = c.updatePodRecordLocked(podName, modelName, metricName, scope, &metrics.SimpleMetricValue{Value: metricValue})
			if err != nil {
				klog.V(4).Infof("Failed to update metrics %s from pod %s %s %d: %v", metricName, podName, pod.Status.PodIP, podPort, err)
				continue
			}

			klog.V(5).InfoS("Successfully parsed metrics", "metric", metricName, "model", modelName, "PodIP", pod.Status.PodIP, "Port", podPort, "metricValue", metricValue)
		}
	}
}

func (c *Store) updateHistogramMetricFromRawMetricsLocked(pod *v1.Pod, allMetrics map[string]*dto.MetricFamily) {
	podName := pod.Name
	for _, metricName := range histogramMetricNames {
		metric, exists := metrics.Metrics[metricName]
		if !exists {
			klog.V(4).Infof("Cannot find %v in the metric list", metricName)
			continue
		}

		metricFamily, exists := allMetrics[fmt.Sprintf("vllm:%s", metricName)]
		if !exists {
			klog.V(4).Infof("Cannot find %v in the pod metrics", metricName)
			continue
		}
		scope := metric.MetricScope
		for _, familyMetric := range metricFamily.Metric {
			modelName, _ := metrics.GetLabelValueForKey(familyMetric, "model_name")
			metricValue, err := metrics.GetHistogramValue(familyMetric)
			if err != nil {
				klog.V(4).Infof("failed to parse metrics %s from pod %s %s %d: %v", metricName, pod.Name, pod.Status.PodIP, podPort, err)
				continue
			}

			histogramValue := &metrics.HistogramMetricValue{
				Sum:     metricValue.Sum,
				Count:   metricValue.Count,
				Buckets: metricValue.Buckets,
			}
			err = c.updatePodRecordLocked(podName, modelName, metricName, scope, histogramValue)
			if err != nil {
				klog.V(4).Infof("Failed to update metrics %s from pod %s %s %d: %v", metricName, podName, pod.Status.PodIP, podPort, err)
				continue
			}

			klog.V(5).InfoS("Successfully parsed metrics", "metric", metricName, "model", modelName, "PodIP", pod.Status.PodIP, "Port", podPort, "metricValue", metricValue)

		}
	}
}

func (c *Store) updateQueryLabelMetricFromRawMetricsLocked(pod *v1.Pod, allMetrics map[string]*dto.MetricFamily) {
	podName := pod.Name

	for _, labelMetricName := range labelQueryMetricNames {
		metric, exists := metrics.Metrics[labelMetricName]
		if !exists {
			klog.V(4).Infof("Cannot find %v in the metric list", labelMetricName)
			continue
		}
		rawMetricName := metric.RawMetricName
		scope := metric.MetricScope
		metricFamily, exists := allMetrics[fmt.Sprintf("vllm:%s", rawMetricName)]
		if !exists {
			klog.V(4).Infof("Cannot find %v in the pod metrics", rawMetricName)
			continue
		}
		for _, familyMetric := range metricFamily.Metric {
			modelName, _ := metrics.GetLabelValueForKey(familyMetric, "model_name")
			labelValue, _ := metrics.GetLabelValueForKey(familyMetric, labelMetricName)
			err := c.updatePodRecordLocked(podName, modelName, labelMetricName, scope, &metrics.LabelValueMetricValue{Value: labelValue})
			if err != nil {
				klog.V(4).Infof("Failed to update metrics %s from pod %s %s %d: %v", labelMetricName, podName, pod.Status.PodIP, podPort, err)
				continue
			}

			klog.V(5).InfoS("Successfully parsed metrics", "metric", labelMetricName, "model", modelName, "PodIP", pod.Status.PodIP, "Port", podPort, "metricValue", labelValue)
		}
	}
}

func (c *Store) updateMetricFromPromQLLocked(pod *v1.Pod) {
	podName := pod.Name

	for _, metricName := range prometheusMetricNames {
		queryLabels := map[string]string{
			"instance": fmt.Sprintf("%s:%d", pod.Status.PodIP, podPort),
		}
		metric, ok := metrics.Metrics[metricName]
		if !ok {
			klog.V(4).Infof("Cannot find %v in the metric list", metricName)
			continue
		}
		scope := metric.MetricScope
		if scope == metrics.PodMetricScope {
			err := c.queryUpdatePromQLMetricsLocked(metric, queryLabels, podName, "", metricName)
			if err != nil {
				klog.V(4).Infof("Failed to query and update PromQL metrics: %v", err)
				continue
			}
		} else if scope == metrics.PodModelMetricScope {
			if modelNames, ok := c.PodToModelMapping[podName]; ok {
				for modelName := range modelNames {
					queryLabels["model_name"] = modelName
					err := c.queryUpdatePromQLMetricsLocked(metric, queryLabels, podName, modelName, metricName)
					if err != nil {
						klog.V(4).Infof("Failed to query and update PromQL metrics: %v", err)
						continue
					}
				}
			} else {
				klog.V(4).Infof("Cannot find model names for pod %s", podName)
			}
		} else {
			klog.V(4).Infof("Scope %v is not supported", scope)
		}
	}
}

func (c *Store) queryUpdatePromQLMetricsLocked(metric metrics.Metric, queryLabels map[string]string, podName string, modelName string, metricName string) error {
	scope := metric.MetricScope
	query := metrics.BuildQuery(metric.PromQL, queryLabels)
	// Querying metrics
	result, warnings, err := c.prometheusApi.Query(context.Background(), query, time.Now())
	if err != nil {
		// Skip this model fetching if an error is thrown
		return fmt.Errorf("error executing query: %v", err)
	}
	if len(warnings) > 0 {
		klog.V(4).Infof("Warnings: %v\n", warnings)
	}

	// Update metrics
	metricValue := &metrics.PrometheusMetricValue{Result: &result}
	err = c.updatePodRecordLocked(podName, modelName, metricName, scope, metricValue)
	if err != nil {
		return fmt.Errorf("failed to update metrics %s from prometheus %s: %v", metricName, podName, err)
	}
	klog.V(5).InfoS("Successfully parsed metrics from prometheus", "metric", metricName, "model", modelName, "PodName", podName, "Port", podPort, "metricValue", metricValue)
	return nil
}

// Update `PodMetrics` and `PodModelMetrics` according to the metric scope
// TODO: replace in-place metric update podMetrics and podModelMetrics to fresh copy for preventing stale metric keys
func (c *Store) updatePodRecordLocked(podName string, modelName string, metricName string, scope metrics.MetricScope, metricValue metrics.MetricValue) error {
	if scope == metrics.PodMetricScope {
		if modelName != "" {
			return fmt.Errorf("modelName should be empty for scope %v", scope)
		}
		c.PodMetrics[podName][metricName] = metricValue
	} else if scope == metrics.PodModelMetricScope {
		if modelName == "" {
			return fmt.Errorf("modelName should not be empty for scope %v", scope)
		}
		if len(c.PodModelMetrics[podName][modelName]) == 0 {
			c.PodModelMetrics[podName][modelName] = map[string]metrics.MetricValue{}
		}
		c.PodModelMetrics[podName][modelName][metricName] = metricValue
	} else {
		return fmt.Errorf("scope %v is not supported", scope)
	}
	return nil
}

func (c *Store) updateModelMetrics() {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.prometheusApi == nil {
		klog.V(4).InfoS("Prometheus api is not initialized, PROMETHEUS_ENDPOINT is not configured, skip fetching prometheus metrics")
		return
	}
}

func (c *Store) aggregateMetrics() {
	for _, subscriber := range c.subscribers {
		for _, metric := range subscriber.SubscribedMetrics() {
			if _, exists := c.metrics[metric]; !exists {
				// TODO: refactor to
				c.metrics[metric] = "yes"
			}
		}
	}
}
