package v1

import "fmt"

// generatedTLSSecretNameFormat is the Secret name the operator generates for
// cert-manager-managed TLS material when the spec pins no name of its own.
// It must stay in sync with the operator's name helpers.
const generatedTLSSecretNameFormat = "rftls-%s"

// TLSSecretName returns the name of the Secret that holds the cluster's TLS
// material (tls.crt, tls.key, ca.crt). It honors an explicit override when
// set; otherwise it derives from the failover name.
//
// Precedence:
//  1. spec.tls.certificateSecret.secretName (bring-your-own-secret mode)
//  2. spec.tls.certManager.secretName       (cert-manager override)
//  3. <generated default>                   (cert-manager managed)
//
// Returns the empty string when TLS is disabled or unconfigured.
func (r *RedisFailover) TLSSecretName() string {
	tls := r.Spec.TLS
	if tls == nil || !tls.Enabled {
		return ""
	}
	if tls.CertificateSecret != nil && tls.CertificateSecret.SecretName != "" {
		return tls.CertificateSecret.SecretName
	}
	if tls.CertManager != nil && tls.CertManager.SecretName != "" {
		return tls.CertManager.SecretName
	}
	return fmt.Sprintf(generatedTLSSecretNameFormat, r.Name)
}
