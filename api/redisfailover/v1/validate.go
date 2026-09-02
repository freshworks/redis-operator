package v1

import (
	"errors"
	"fmt"
	"strconv"
)

const (
	maxNameLength = 48
)

// Validate set the values by default if not defined and checks if the values given are valid
func (r *RedisFailover) Validate() error {
	if len(r.Name) > maxNameLength {
		return fmt.Errorf("name length can't be higher than %d", maxNameLength)
	}

	switch r.Spec.Engine {
	case "", RedisEngine, ValkeyEngine:
	default:
		return fmt.Errorf("invalid engine %q, must be Redis, Valkey, or omitted", r.Spec.Engine)
	}

	defaultImageForEngine := defaultImage
	if r.Spec.Engine == ValkeyEngine {
		defaultImageForEngine = defaultValkeyImage
	}

	if r.Bootstrapping() {
		if r.Spec.BootstrapNode.Host == "" {
			return errors.New("BootstrapNode must include a host when provided")
		}

		if r.Spec.BootstrapNode.Port == "" {
			r.Spec.BootstrapNode.Port = strconv.Itoa(defaultRedisPort)
		}
		r.Spec.Redis.CustomConfig = deduplicateStr(append(bootstrappingRedisCustomConfig, r.Spec.Redis.CustomConfig...))
	} else {
		r.Spec.Redis.CustomConfig = deduplicateStr(append(defaultRedisCustomConfig, r.Spec.Redis.CustomConfig...))
	}

	if r.Spec.Redis.Image == "" {
		r.Spec.Redis.Image = defaultImageForEngine
	}

	if r.Spec.Sentinel.Image == "" {
		r.Spec.Sentinel.Image = defaultImageForEngine
	}

	if r.Spec.Redis.Replicas <= 0 {
		r.Spec.Redis.Replicas = defaultRedisNumber
	}

	if r.Spec.Redis.Port <= 0 {
		r.Spec.Redis.Port = defaultRedisPort
	}

	if r.Spec.Redis.ReservedPodMemoryPercent <= 0 {
		r.Spec.Redis.ReservedPodMemoryPercent = defaultReservedPodMemoryPercent
	}

	if r.Spec.Sentinel.Replicas <= 0 {
		r.Spec.Sentinel.Replicas = defaultSentinelNumber
	}

	if r.Spec.Redis.Exporter.Image == "" {
		r.Spec.Redis.Exporter.Image = defaultExporterImage
	}

	if r.Spec.Sentinel.Exporter.Image == "" {
		r.Spec.Sentinel.Exporter.Image = defaultSentinelExporterImage
	}

	if len(r.Spec.Sentinel.CustomConfig) == 0 {
		r.Spec.Sentinel.CustomConfig = defaultSentinelCustomConfig
	}

	if err := r.validateTLS(); err != nil {
		return err
	}

	return nil
}

// validateTLS enforces the TLSSettings invariants and applies defaults.
// Returns nil when TLS is disabled or unset.
func (r *RedisFailover) validateTLS() error {
	tls := r.Spec.TLS
	if tls == nil || !tls.Enabled {
		return nil
	}

	switch tls.AuthClients {
	case "":
		tls.AuthClients = defaultTLSAuthClients
	case TLSAuthClientsNo, TLSAuthClientsOptional, TLSAuthClientsYes:
		// ok
	default:
		return fmt.Errorf("tls.authClients must be one of %q, %q, %q (got %q)",
			TLSAuthClientsNo, TLSAuthClientsOptional, TLSAuthClientsYes, tls.AuthClients)
	}

	cmSet := tls.CertManager != nil
	secretSet := tls.CertificateSecret != nil

	switch {
	case cmSet && secretSet:
		return errors.New("tls.certManager and tls.certificateSecret are mutually exclusive")
	case !cmSet && !secretSet:
		return errors.New("tls.enabled is true but neither tls.certManager nor tls.certificateSecret is set")
	}

	if cmSet {
		if tls.CertManager.IssuerRef.Name == "" {
			return errors.New("tls.certManager.issuerRef.name is required")
		}
		if tls.CertManager.IssuerRef.Group == "" {
			tls.CertManager.IssuerRef.Group = defaultCertManagerGroup
		}
		if tls.CertManager.IssuerRef.Kind == "" {
			tls.CertManager.IssuerRef.Kind = defaultCertManagerKind
		}
	}

	if secretSet && tls.CertificateSecret.SecretName == "" {
		return errors.New("tls.certificateSecret.secretName is required")
	}

	// The operator writes the CA-only Secret unconditionally. Pointing it at
	// the Secret that carries the certificate and key would replace that
	// material with a Secret holding nothing but ca.crt, leaving the cluster
	// without a serving certificate.
	if tls.CACertSecretName != "" && tls.CACertSecretName == r.TLSSecretName() {
		return fmt.Errorf("tls.caCertSecretName %q must differ from the Secret holding the certificate and key, "+
			"otherwise the CA-only Secret the operator publishes overwrites it", tls.CACertSecretName)
	}

	return nil
}

func deduplicateStr(strSlice []string) []string {
	allKeys := make(map[string]bool)
	list := []string{}
	for _, item := range strSlice {
		if _, value := allKeys[item]; !value {
			allKeys[item] = true
			list = append(list, item)
		}
	}
	return list
}
