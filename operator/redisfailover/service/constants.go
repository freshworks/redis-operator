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
	tlsCertificateName     = "tls"
	appLabel               = "redis-failover"
	hostnameTopologyKey    = "kubernetes.io/hostname"
)

// TLS volume / file layout.
//
// cert-manager produces a Secret with three well-known keys (tls.crt,
// tls.key, ca.crt). The operator mounts that Secret read-only into every
// Redis and Sentinel pod at tlsMountPath. The probe scripts, redis.conf
// and sentinel.conf reference the same paths.
const (
	tlsVolumeName  = "redis-tls"
	tlsMountPath   = "/tls"
	tlsCertFile    = tlsMountPath + "/tls.crt"
	tlsKeyFile     = tlsMountPath + "/tls.key"
	tlsCAFile      = tlsMountPath + "/ca.crt"
	tlsSecretKey   = "tls.crt"
	tlsSecretCAKey = "ca.crt"

	// tlsSecretHashAnnotation carries a content hash of the mounted TLS
	// Secret and of the tls-auth-clients directive on the Redis and
	// Sentinel pod templates. Redis loads the certificate files and its
	// config once at startup and never re-reads them, so a renewed Secret
	// or a changed directive reaches the running server only if its pod
	// restarts.
	// Changing this annotation changes the pod template, which is what
	// makes the StatefulSet publish a new updateRevision and the Sentinel
	// Deployment roll.
	tlsSecretHashAnnotation = "redis-failover.freshworks.com/tls-secret-hash"

	// tlsVolumeMode keeps the private key off the world-readable bits the
	// kubelet would otherwise apply. It is only safe when the pod declares
	// an fsGroup; see tlsVolume.
	tlsVolumeMode = int32(0440)
)

const (
	redisRoleLabelKey    = "redisfailovers-role"
	redisRoleLabelMaster = "master"
	redisRoleLabelSlave  = "slave"

	clusterAutoscalerSafeToEvictAnnotationKey    = "cluster-autoscaler.kubernetes.io/safe-to-evict"
	clusterAutoscalerSafeToEvictAnnotationMaster = "false"
	clusterAutoscalerSafeToEvictAnnotationSlave  = "true"
)
