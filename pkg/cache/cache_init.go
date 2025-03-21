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
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"

	prometheusv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/vllm-project/aibrix/pkg/metrics"
)

var (
	store = &Store{} // Global cache store instance
	once  sync.Once  // Singleton pattern control lock
)

// Store contains core data structures and components of the caching system
type Store struct {
	mu            sync.RWMutex     // Read-write lock for concurrency safety
	initialized   bool             // Initialization status flag
	redisClient   *redis.Client    // Redis client instance
	prometheusApi prometheusv1.API // Prometheus API client

	// Metrics related fields
	subscribers       []metrics.MetricSubscriber // List of metric subscribers
	metrics           map[string]any             // Generic metric storage
	requestTrace      *sync.Map                  // Request trace data (model_name: RequestTrace)
	pendingRequests   *sync.Map                  // In-progress request records
	numRequestsTraces int32                      // Request trace counter

	// Pod related storage
	Pods            map[string]*v1.Pod                                   // Pod name to Pod object mapping
	PodMetrics      map[string]map[string]metrics.MetricValue            // Pod metrics (pod_name -> metric_name -> value)
	PodModelMetrics map[string]map[string]map[string]metrics.MetricValue // Pod-model metrics (pod_name -> model_name -> metric_name -> value)

	// Mapping relationships
	PodToModelMapping map[string]map[string]struct{} // Pod to model mapping (pod_name -> model set)
	ModelToPodMapping map[string]map[string]*v1.Pod  // Model to pod mapping (model_name -> pod set)
}

// Get retrieves the cache instance
// Returns:
//
//	Cache: Cache interface instance
//	error: Returns error if cache is not initialized
func Get() (Cache, error) {
	if !store.initialized {
		return nil, errors.New("cache is not initialized")
	}
	return store, nil
}

// New creates a new cache store instance
// Parameters:
//
//	redisClient: Redis client instance
//	prometheusApi: Prometheus API client
//
// Returns:
//
//	Store: Initialized cache store instance
func New(redisClient *redis.Client, prometheusApi prometheusv1.API) *Store {
	return &Store{
		initialized:     true,
		redisClient:     redisClient,
		prometheusApi:   prometheusApi,
		requestTrace:    &sync.Map{},
		pendingRequests: &sync.Map{},

		// Initialize storage maps
		Pods:              make(map[string]*v1.Pod),
		PodMetrics:        make(map[string]map[string]metrics.MetricValue),
		PodModelMetrics:   make(map[string]map[string]map[string]metrics.MetricValue),
		PodToModelMapping: make(map[string]map[string]struct{}),
		ModelToPodMapping: make(map[string]map[string]*v1.Pod),
	}
}

// Init initializes the cache store (singleton pattern)
// Parameters:
//
//	config: Kubernetes configuration
//	stopCh: Stop signal channel
//	redisClient: Redis client instance
//
// Returns:
//
//	*Store: Pointer to initialized store instance
func Init(config *rest.Config, stopCh <-chan struct{}, redisClient *redis.Client) *Store {
	once.Do(func() {
		store = New(redisClient, initPrometheusAPI())

		// Initialize cache components
		if err := initCacheInformers(store, config, stopCh); err != nil {
			panic(err)
		}
		initMetricsCache(store, stopCh)
		initTraceCache(redisClient, stopCh)
	})

	return store
}

// initMetricsCache initializes metrics cache update loop
// Parameters:
//
//	store: Cache store instance
//	stopCh: Stop signal channel
func initMetricsCache(store *Store, stopCh <-chan struct{}) {
	ticker := time.NewTicker(podMetricRefreshInterval)
	go func() {
		for {
			select {
			case <-ticker.C:
				// Periodically update metrics
				store.updatePodMetrics()
				store.updateModelMetrics()
				store.updateDebugInfo()
			case <-stopCh:
				ticker.Stop()
				return
			}
		}
	}()
}

// initTraceCache initializes request tracing cache
// Parameters:
//
//	redisClient: Redis client instance
//	stopCh: Stop signal channel
func initTraceCache(redisClient *redis.Client, stopCh <-chan struct{}) {
	// Calculate time offset for window alignment
	tickerOffset := time.Duration(time.Now().UnixNano()) % RequestTraceWriteInterval
	var traceAlignmentTimer *time.Timer
	var traceTicker *time.Ticker

	// Select alignment method based on offset
	if tickerOffset > MaxRequestTraceIntervalOffset {
		traceAlignmentTimer = time.NewTimer(RequestTraceWriteInterval - tickerOffset)
	} else {
		traceTicker = time.NewTicker(RequestTraceWriteInterval)
	}

	go func() {
		if redisClient == nil {
			return
		}
		if traceAlignmentTimer != nil {
			// Wait for time window alignment
			<-traceAlignmentTimer.C
			traceAlignmentTimer = nil
			traceTicker = time.NewTicker(RequestTraceWriteInterval)
		}
		klog.Infof("trace ticker start at %s", time.Now())
		for {
			select {
			case <-traceTicker.C:
				// Periodically write trace data to storage
				if atomic.LoadInt32(&store.numRequestsTraces) == 0 {
					continue
				}
				t := time.Now().Unix()
				roundT := t - t%int64(RequestTraceWriteInterval/time.Second)
				store.writeRequestTraceToStorage(roundT)
			case <-stopCh:
				traceTicker.Stop()
				return
			}
		}
	}()
}
