package service

// variables refering to the redis exporter port
const (
	exporterPort                  = 9121
	sentinelExporterPort          = 9355
	exporterPortName              = "http-metrics"
	exporterContainerName         = "redis-exporter"
	sentinelExporterContainerName = "sentinel-exporter"
	exporterDefaultRequestCPU     = "10m"
	exporterDefaultLimitCPU       = "1000m"
	exporterDefaultRequestMemory  = "50Mi"
	exporterDefaultLimitMemory    = "100Mi"
)

const (
	baseName               = "rf"
	sentinelName           = "s"
	sentinelRoleName       = "sentinel"
	sentinelConfigFileName = "sentinel.conf"
	redisConfigFileName    = "redis.conf"
	redisName              = "r"
	redisMasterName        = "rm"
	redisSlaveName         = "rs"
	redisShutdownName      = "r-s"
	redisReadinessName     = "r-readiness"
	redisRoleName          = "redis"
	appLabel               = "redis-failover"
	hostnameTopologyKey    = "kubernetes.io/hostname"
)

const (
	redisRoleLabelKey    = "redisfailovers-role"
	redisRoleLabelMaster = "master"
	redisRoleLabelSlave  = "slave"

	clusterAutoscalerSafeToEvictAnnotationKey    = "cluster-autoscaler.kubernetes.io/safe-to-evict"
	clusterAutoscalerSafeToEvictAnnotationMaster = "false"
	clusterAutoscalerSafeToEvictAnnotationSlave  = "true"

	// redisFailoverNameLabelKey identifies which RedisFailover a pod belongs to (set by the
	// handler on every pod it creates). Used to exclude an endpoint's own slaves from the
	// eviction candidate pool when freeing headroom for that same endpoint's master resize.
	redisFailoverNameLabelKey = "redisfailovers.databases.spotahome.com/name"

	// resizeStartedAtAnnotationKey records when an in-place resize attempt began, so it can be
	// timed out and fall back to the delete-based rollout. There is no requeue/backoff or
	// Status subresource in this operator, so this state must be tracked this way across the
	// periodic reconcile resync rather than in memory.
	resizeStartedAtAnnotationKey = "redis-failover.freshworks.com/resize-started-at"

	// resizeTargetRevisionAnnotationKey records which StatefulSet revision a tracked resize
	// attempt was for. Without this, a leftover resize-started-at annotation from an
	// already-resolved attempt (e.g. ClearResizeState failed after a successful resize) would
	// be indistinguishable from a still-in-progress attempt for a brand-new target, and its
	// stale timestamp could send an unrelated later resize straight to a timeout.
	resizeTargetRevisionAnnotationKey = "redis-failover.freshworks.com/resize-target-revision"
)
