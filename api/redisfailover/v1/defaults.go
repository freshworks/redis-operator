package v1

const (
	defaultRedisNumber              = 3
	defaultSentinelNumber           = 3
	defaultSentinelExporterImage    = "quay.io/oliver006/redis_exporter:v1.43.0"
	defaultExporterImage            = "quay.io/oliver006/redis_exporter:v1.43.0"
	defaultImage                    = "redis:7.2.4-alpine"
	defaultValkeyImage              = "valkey/valkey:7.2.11-alpine"
	defaultRedisPort                = 6379
	defaultReservedPodMemoryPercent = 10

	defaultTLSAuthClients   = TLSAuthClientsNo
	defaultCertManagerGroup = "cert-manager.io"
	defaultCertManagerKind  = "Issuer"
)

var (
	defaultSentinelCustomConfig = []string{
		"down-after-milliseconds 5000",
		"failover-timeout 10000",
	}
	defaultRedisCustomConfig = []string{
		"replica-priority 100",
	}
	bootstrappingRedisCustomConfig = []string{
		"replica-priority 0",
	}
)
