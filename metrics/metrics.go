package metrics

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	koopercontroller "github.com/spotahome/kooper/v2/controller"
	kooperprometheus "github.com/spotahome/kooper/v2/metrics/prometheus"
	"github.com/freshworks/redis-operator/log"
)

const (
	promControllerSubsystem  = "controller"
	metricsGCIntervalMinutes = 5
)

// Constants for metric labels
const (
	SUCCESS                                = "SUCCESS"
	FAIL                                   = "FAIL"
	STATUS_HEALTHY                         = "HEALTHY"
	STATUS_UNHEALTHY                       = "UNHEALTHY"
	NOT_APPLICABLE                         = "NA"
	UNHEALTHY                              = 1.0
	HEALTHY                                = 0.0
	REDIS_REPLICA_MISMATCH                 = "REDIS_STATEFULSET_REPLICAS_MISMATCH"
	SENTINEL_REPLICA_MISMATCH              = "SENTINEL_DEPLOYMENT_REPLICAS_MISMATCH"
	NO_MASTER                              = "NO_MASTER_AVAILABLE"
	NUMBER_OF_MASTERS                      = "MASTER_COUNT_IS_NOT_ONE"
	SENTINEL_WRONG_MASTER                  = "SENTINEL_IS_CONFIGURED_WITH_WRONG_MASTER_IP"
	SLAVE_WRONG_MASTER                     = "SLAVE_IS_CONFIGURED_WITH_WRONG_MASTER_IP"
	SENTINEL_NOT_READY                     = "SENTINEL_NOT_READY"
	REGEX_NOT_FOUND                        = "SENTINEL_REGEX_NOT_FOUND"
	MISC                                   = "MISC_ERROR"
	SENTINEL_NUMBER_IN_MEMORY_MISMATCH     = "SENTINEL_NUMBER_IN_MEMORY_MISMATCH"
	REDIS_SLAVES_NUMBER_IN_MEMORY_MISMATCH = "REDIS_SLAVES_NUMBER_IN_MEMORY_MISMATCH"

	// Redis connection related errors
	WRONG_PASSWORD_USED = "WRONG_PASSWORD_USED"
	NOAUTH              = "AUTH_CREDENTIALS_NOT_PROVIDED"
	NOPERM              = "REDIS_USER_DOES_NOT_HAVE_PERMISSIONS"
	IO_TIMEOUT          = "CONNECTION_TIMEDOUT"
	CONNECTION_REFUSED  = "CONNECTION_REFUSED"

	// Kubernetes related errors
	K8S_FORBIDDEN_ERR = "USER_FORBIDDEN_TO_PERFORM_ACTION"
	K8S_UNAUTH        = "CLIENT_NOT_AUTHORISED"
	K8S_MISC          = "MISC_ERROR_CHECK_LOGS"
	K8S_NOT_FOUND     = "RESOURCE_NOT_FOUND"

	// Operation types
	KIND_REDIS                  = "REDIS"
	KIND_SENTINEL               = "SENTINEL"
	APPLY_REDIS_CONFIG          = "APPLY_REDIS_CONFIG"
	APPLY_EXTERNAL_MASTER       = "APPLY_EXT_MASTER_ALL"
	APPLY_SENTINEL_CONFIG       = "APPLY_SENTINEL_CONFIG"
	MONITOR_REDIS_WITH_PORT     = "SET_SENTINEL_TO_MONITOR_REDIS_WITH_GIVEN_PORT"
	RESET_SENTINEL              = "RESET_ALL_SENTINEL_CONFIG"
	GET_NUM_SENTINELS_IN_MEM    = "GET_NUMBER_OF_SENTINELS_IN_MEMORY"    // `info sentinel` command on a sentinel machine > grep sentinel
	GET_NUM_REDIS_SLAVES_IN_MEM = "GET_NUMBER_OF_REDIS_SLAVES_IN_MEMORY" // `info sentinel` command on a sentinel machine > grep slaves
	GET_SLAVE_OF                = "GET_MASTER_OF_GIVEN_SLAVE_INSTANCE"
	IS_MASTER                   = "CHECK_IF_INSTANCE_IS_MASTER"
	MAKE_MASTER                 = "MAKE_INSTANCE_AS_MASTER"
	MAKE_SLAVE_OF               = "MAKE_SLAVE_OF_GIVEN_MASTER_INSTANCE"
	GET_SENTINEL_MONITOR        = "SENTINEL_GET_MASTER_INSTANCE"
	CHECK_SENTINEL_QUORUM       = "SENTINEL_CKQUORUM"
	SLAVE_IS_READY              = "CHECK_IF_SLAVE_IS_READY"
)

// MetricsTracker handles thread-safe tracking of metric updates
type MetricsTracker struct {
	mu              sync.RWMutex
	resourceMetrics map[string]time.Time
	instanceMetrics map[string]time.Time
}

// NewMetricsTracker creates a new MetricsTracker
func NewMetricsTracker() *MetricsTracker {
	return &MetricsTracker{
		resourceMetrics: make(map[string]time.Time),
		instanceMetrics: make(map[string]time.Time),
	}
}

// Recorder interface defines methods for metric collection
type Recorder interface {
	koopercontroller.MetricsRecorder

	// ClusterOK metrics
	SetClusterOK(namespace string, name string)
	SetClusterError(namespace string, name string)
	DeleteCluster(namespace string, name string)

	// Indicate redis instances being monitored
	RecordEnsureOperation(objectNamespace string, objectName string, objectKind string, resourceName string, status string)

	RecordRedisCheck(namespace string, resource string, indicator /* aspect of redis that is unhealthy */ string, instance string, status string)
	RecordSentinelCheck(namespace string, resource string, indicator /* aspect of sentinel that is unhealthy */ string, instance string, status string)

	RecordK8sOperation(namespace string, kind string, name string, operation string, status string, err string)
	RecordRedisOperation(kind string, IP string, operation string, status string, err string)
}

// PromMetrics implements the instrumenter so the metrics can be managed by Prometheus.
type recorder struct {
	// Metrics fields.
	metricsTracker       *MetricsTracker
	clusterOK            *prometheus.GaugeVec   // clusterOk is the status of a cluster
	ensureResource       *prometheus.CounterVec // number of successful "ensure" operators performed by the controller.
	redisCheck           *prometheus.CounterVec // indicates any error encountered in managed redis instance(s)
	sentinelCheck        *prometheus.CounterVec // indicates any error encountered in managed sentinel instance(s)
	k8sServiceOperations *prometheus.CounterVec // number of operations performed on k8s
	redisOperations      *prometheus.CounterVec // number of operations performed on redis/sentinel instances
	koopercontroller.MetricsRecorder
}

// NewPrometheusMetrics returns a new PromMetrics object.
func NewRecorder(namespace string, reg prometheus.Registerer) Recorder {
	metricsTracker := NewMetricsTracker()

	clusterOK := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Subsystem: promControllerSubsystem,
		Name:      "cluster_ok",
		Help:      "Number of failover clusters managed by the operator.",
	}, []string{"namespace", "name"})

	ensureResource := prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Subsystem: promControllerSubsystem,
		Name:      "ensure_resource_total",
		Help:      "number of 'ensure' operations on a resource performed by the controller.",
	}, []string{"namespace", "name", "kind", "resource_name", "status"})

	redisCheck := prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Subsystem: promControllerSubsystem,
		Name:      "redis_checks_total",
		Help:      "indicates any error encountered in managed redis instance(s)",
	}, []string{"namespace", "resource", "indicator", "instance", "status"})

	sentinelCheck := prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Subsystem: promControllerSubsystem,
		Name:      "sentinel_checks_total",
		Help:      "indicates any error encountered in managed sentinel instance(s)",
	}, []string{"namespace", "resource", "indicator", "instance", "status"})

	redisOperations := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: promControllerSubsystem,
			Name:      "redis_operations_total",
			Help:      "number of operations performed on redis",
		}, []string{"kind" /* redis/sentinel? */, "IP", "operation", "status", "err"})

	k8sServiceOperations := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: promControllerSubsystem,
			Name:      "k8s_operations_total",
			Help:      "number of operations performed on k8s",
		}, []string{"namespace", "kind", "name", "operation", "status", "err"})

	// Create the instance.
	r := &recorder{
		metricsTracker:       metricsTracker,
		clusterOK:            clusterOK,
		ensureResource:       ensureResource,
		redisCheck:           redisCheck,
		sentinelCheck:        sentinelCheck,
		k8sServiceOperations: k8sServiceOperations,
		redisOperations:      redisOperations,
		MetricsRecorder: kooperprometheus.New(kooperprometheus.Config{
			Registerer: reg,
		}),
	}

	// Register metrics.
	reg.MustRegister(
		r.clusterOK,
		r.ensureResource,
		r.redisCheck,
		r.sentinelCheck,
		r.k8sServiceOperations,
		r.redisOperations,
	)

	// Start the metrics garbage collector
	go r.removeStaleMetrics()

	return r
}

func (r *recorder) SetClusterOK(namespace string, name string) {
	r.clusterOK.WithLabelValues(namespace, name).Set(1)
}

func (r *recorder) SetClusterError(namespace string, name string) {
	r.clusterOK.WithLabelValues(namespace, name).Set(0)
}

func (r *recorder) DeleteCluster(namespace string, name string) {
	r.clusterOK.DeleteLabelValues(namespace, name)
}

func (r *recorder) RecordEnsureOperation(objectNamespace string, objectName string, objectKind string, resourceName string, status string) {
	r.ensureResource.WithLabelValues(objectNamespace, objectName, objectKind, resourceName, status).Add(1)
	r.metricsTracker.UpdateResourceMetric(objectNamespace, objectKind, objectName)
}

func (r *recorder) RecordRedisCheck(namespace string, resource string, indicator /* aspect of redis that is unhealthy */ string, instance string, status string) {
	r.redisCheck.WithLabelValues(namespace, resource, indicator, instance, status).Add(1)
	r.metricsTracker.UpdateResourceMetric(namespace, "redisfailover", resource)
}

func (r *recorder) RecordSentinelCheck(namespace string, resource string, indicator /* aspect of sentinel that is unhealthy */ string, instance string, status string) {
	r.sentinelCheck.WithLabelValues(namespace, resource, indicator, instance, status).Add(1)
	r.metricsTracker.UpdateResourceMetric(namespace, "redisfailover", resource)
}

func (r *recorder) RecordK8sOperation(namespace string, kind string, name string, operation string, status string, err string) {
	r.k8sServiceOperations.WithLabelValues(namespace, kind, name, operation, status, err).Add(1)
	r.metricsTracker.UpdateResourceMetric(namespace, kind, name)
}

func (r *recorder) RecordRedisOperation(kind /*redis/sentinel? */ string, IP string, operation string, status string, err string) {
	r.redisOperations.WithLabelValues(kind, IP, operation, status, err).Add(1)
	r.metricsTracker.UpdateInstanceMetric(IP)
}

// MetricsTracker methods
func (mt *MetricsTracker) UpdateResourceMetric(namespace, kind, name string) {
	key := fmt.Sprintf("%v/%v/%v", namespace, kind, name)
	mt.mu.Lock()
	mt.resourceMetrics[key] = time.Now()
	mt.mu.Unlock()
}

func (mt *MetricsTracker) UpdateInstanceMetric(IP string) {
	mt.mu.Lock()
	mt.instanceMetrics[IP] = time.Now()
	mt.mu.Unlock()
}

func (mt *MetricsTracker) GetStaleMetrics(gcInterval time.Duration) ([]prometheus.Labels, []prometheus.Labels, []prometheus.Labels) {
	var kubernetesResourceBasedLabels, customResourceBasedLabels, ipBasedLabels []prometheus.Labels
	staleTime := time.Now().Add(-gcInterval)

	mt.mu.Lock()
	defer mt.mu.Unlock()

	for key, lastUpdate := range mt.resourceMetrics {
		if lastUpdate.Before(staleTime) {
			ids := strings.Split(key, "/")
			namespace := ids[0]
			kind := ids[1]
			resource := ids[2]
			kubernetesResourceBasedLabels = append(kubernetesResourceBasedLabels,
				prometheus.Labels{
					"namespace": namespace,
					"name":      resource,
					"kind":      kind,
				},
			)
			customResourceBasedLabels = append(customResourceBasedLabels,
				prometheus.Labels{
					"namespace": namespace,
					"resource":  resource,
				},
			)
			delete(mt.resourceMetrics, key)
		}
	}

	for IP, lastUpdate := range mt.instanceMetrics {
		if lastUpdate.Before(staleTime) {
			ipBasedLabels = append(ipBasedLabels,
				prometheus.Labels{
					"IP": IP,
				},
			)
			delete(mt.instanceMetrics, IP)
		}
	}

	return kubernetesResourceBasedLabels, customResourceBasedLabels, ipBasedLabels
}

// Garbage collection routine
func (r *recorder) removeStaleMetrics() {
	for {
		k, c, i := r.metricsTracker.GetStaleMetrics(metricsGCIntervalMinutes * time.Minute)
		metricsDeletedCount := 0

		for _, label := range k {
			metricsDeletedCount += r.ensureResource.DeletePartialMatch(label)
			metricsDeletedCount += r.k8sServiceOperations.DeletePartialMatch(label)
		}
		for _, label := range c {
			metricsDeletedCount += r.redisCheck.DeletePartialMatch(label)
			metricsDeletedCount += r.sentinelCheck.DeletePartialMatch(label)
			labelWithName := prometheus.Labels{
				"namespace": label["namespace"],
				"name":      label["resource"],
			}
			metricsDeletedCount += r.clusterOK.DeletePartialMatch(labelWithName)
		}
		for _, label := range i {
			metricsDeletedCount += r.redisOperations.DeletePartialMatch(label)
		}

		log.Debugf("deleted %v stale metrics", metricsDeletedCount)
		time.Sleep(metricsGCIntervalMinutes * time.Minute)
	}
}