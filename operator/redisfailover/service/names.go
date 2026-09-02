package service

import (
	"fmt"

	redisfailoverv1 "github.com/freshworks/redis-operator/api/redisfailover/v1"
)

// GetRedisShutdownConfigMapName returns the name for redis configmap
func GetRedisShutdownConfigMapName(rf *redisfailoverv1.RedisFailover) string {
	if rf.Spec.Redis.ShutdownConfigMap != "" {
		return rf.Spec.Redis.ShutdownConfigMap
	}
	return GetRedisShutdownName(rf)
}

// GetRedisName returns the name for redis resources
func GetRedisName(rf *redisfailoverv1.RedisFailover) string {
	return generateName(redisName, rf.Name)
}

// GetRedisShutdownName returns the name for redis resources
func GetRedisShutdownName(rf *redisfailoverv1.RedisFailover) string {
	return generateName(redisShutdownName, rf.Name)
}

// GetRedisReadinessName returns the name for redis resources
func GetRedisReadinessName(rf *redisfailoverv1.RedisFailover) string {
	return generateName(redisReadinessName, rf.Name)
}

// GetSentinelName returns the name for sentinel resources
func GetSentinelName(rf *redisfailoverv1.RedisFailover) string {
	return generateName(sentinelName, rf.Name)
}

func GetRedisMasterName(rf *redisfailoverv1.RedisFailover) string {
	return generateName(redisMasterName, rf.Name)
}

func GetRedisSlaveName(rf *redisfailoverv1.RedisFailover) string {
	return generateName(redisSlaveName, rf.Name)
}

// GetTLSCertificateName returns the name for the cert-manager Certificate
// the operator creates when spec.tls.certManager is configured.
func GetTLSCertificateName(rf *redisfailoverv1.RedisFailover) string {
	return generateName(tlsCertificateName, rf.Name)
}

// GetTLSSecretName returns the Secret name that holds the TLS material for
// the cluster. The derivation lives on the API type because validation needs
// it too; see RedisFailover.TLSSecretName for the precedence rules.
func GetTLSSecretName(rf *redisfailoverv1.RedisFailover) string {
	return rf.TLSSecretName()
}

// GetTLSCACertSecretName returns the name of the Opaque Secret the operator
// publishes with only ca.crt (no private key). It honors an explicit override
// via spec.tls.caCertSecretName; otherwise it derives from the TLS secret name
// as "<tls-secret-name>-ca". Returns the empty string when TLS is disabled.
func GetTLSCACertSecretName(rf *redisfailoverv1.RedisFailover) string {
	if !TLSEnabled(rf) {
		return ""
	}
	if name := rf.Spec.TLS.CACertSecretName; name != "" {
		return name
	}
	return GetTLSSecretName(rf) + "-ca"
}

// TLSEnabled is a small helper used pervasively in the generator.
func TLSEnabled(rf *redisfailoverv1.RedisFailover) bool {
	return rf.Spec.TLS != nil && rf.Spec.TLS.Enabled
}

// tlsAuthClients returns the effective tls-auth-clients value: the spec field,
// or the "no" the API defaulting fills in for an empty one. The two spellings
// describe the same server, so anything derived from this value must not tell
// them apart.
func tlsAuthClients(rf *redisfailoverv1.RedisFailover) string {
	if !TLSEnabled(rf) || rf.Spec.TLS.AuthClients == "" {
		return redisfailoverv1.TLSAuthClientsNo
	}
	return rf.Spec.TLS.AuthClients
}

func generateName(typeName, metaName string) string {
	return fmt.Sprintf("%s%s-%s", baseName, typeName, metaName)
}
