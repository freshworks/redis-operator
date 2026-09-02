package redis

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
)

// BuildTLSConfig assembles the *tls.Config the operator uses to dial
// Redis and Sentinel pods over TLS.
//
// The CA pool is loaded from the supplied caPEM, which the operator
// reads from the same Secret that cert-manager (or the user) populated
// for the cluster pods. The ServerName drives certificate validation:
// the operator dials pods by their (unstable) IP but presents a stable
// DNS name during the TLS handshake, which the certificate's DNS SANs
// cover.
//
// certPEM and keyPEM are optional. When both are non-empty, the config
// also presents them as a client certificate during the TLS handshake,
// which is required when the Redis and Sentinel pods enforce mutual
// TLS (spec.tls.authClients: yes). Supplying only one of the two is an
// error.
//
// Returns (nil, nil) when caPEM is empty: callers treat a nil config as
// "no TLS" and dial in plaintext.
func BuildTLSConfig(caPEM, certPEM, keyPEM []byte, serverName string) (*tls.Config, error) {
	if len(caPEM) == 0 {
		if len(certPEM) > 0 || len(keyPEM) > 0 {
			return nil, errors.New("tls: caPEM must be supplied when client certPEM or keyPEM are provided")
		}
		return nil, nil
	}
	if serverName == "" {
		return nil, errors.New("tls: ServerName must not be empty when caPEM is provided")
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("tls: failed to parse CA certificate (no usable certificates found)")
	}
	cfg := &tls.Config{
		RootCAs:    pool,
		ServerName: serverName,
		MinVersion: tls.VersionTLS12,
	}
	hasCert := len(certPEM) > 0
	hasKey := len(keyPEM) > 0
	if hasCert != hasKey {
		return nil, errors.New("tls: client certPEM and keyPEM must be supplied together")
	}
	if hasCert {
		clientCert, err := tls.X509KeyPair(certPEM, keyPEM)
		if err != nil {
			return nil, fmt.Errorf("tls: loading client certificate: %w", err)
		}
		cfg.Certificates = []tls.Certificate{clientCert}
	}
	return cfg, nil
}
