package service

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	redisfailoverv1 "github.com/freshworks/redis-operator/api/redisfailover/v1"
	mK8SService "github.com/freshworks/redis-operator/mocks/service/k8s"
)

// tlsConfigRF returns a TLS-enabled RedisFailover with the given
// tls-auth-clients value, bring-your-own-secret mode so the secret name is
// predictable.
func tlsConfigRF(authClients string) *redisfailoverv1.RedisFailover {
	return &redisfailoverv1.RedisFailover{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "testns"},
		Spec: redisfailoverv1.RedisFailoverSpec{
			TLS: &redisfailoverv1.TLSSettings{
				Enabled:     true,
				AuthClients: authClients,
			},
		},
	}
}

// tlsSecretService returns a k8s service mock that hands back the cluster's
// TLS secret with the given contents.
func tlsSecretService(rf *redisfailoverv1.RedisFailover, data map[string][]byte) *mK8SService.Services {
	name := GetTLSSecretName(rf)
	ms := &mK8SService.Services{}
	ms.On("GetSecret", rf.Namespace, name).Return(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: rf.Namespace},
		Data:       data,
	}, nil)
	return ms
}

// TestTLSConfigForPresentsClientCertificate pins the operator's client
// credentials to what the TLS secret carries rather than to
// spec.tls.authClients. The directive is mutable, and the running pods keep
// enforcing the value they booted with, so gating the certificate on the spec
// leaves the operator unable to reach its own instance for as long as a flip
// to "no" is pending on pods that still demand one.
func TestTLSConfigForPresentsClientCertificate(t *testing.T) {
	t.Parallel()
	ca := tlsTestCA(t)
	cert, key := tlsTestClientPair(t)

	cases := []struct {
		name        string
		authClients string
		data        map[string][]byte
		wantCerts   int
	}{
		{
			name:        "authClients no presents the pair the secret carries",
			authClients: redisfailoverv1.TLSAuthClientsNo,
			data:        map[string][]byte{"ca.crt": ca, "tls.crt": cert, "tls.key": key},
			wantCerts:   1,
		},
		{
			name:        "authClients optional presents the pair",
			authClients: redisfailoverv1.TLSAuthClientsOptional,
			data:        map[string][]byte{"ca.crt": ca, "tls.crt": cert, "tls.key": key},
			wantCerts:   1,
		},
		{
			name:        "authClients yes presents the pair",
			authClients: redisfailoverv1.TLSAuthClientsYes,
			data:        map[string][]byte{"ca.crt": ca, "tls.crt": cert, "tls.key": key},
			wantCerts:   1,
		},
		{
			name:        "an unset authClients presents the pair",
			authClients: "",
			data:        map[string][]byte{"ca.crt": ca, "tls.crt": cert, "tls.key": key},
			wantCerts:   1,
		},
		{
			name:        "a CA-only secret yields no client certificate",
			authClients: redisfailoverv1.TLSAuthClientsNo,
			data:        map[string][]byte{"ca.crt": ca},
			wantCerts:   0,
		},
		{
			name:        "a certificate without its key yields no client certificate",
			authClients: redisfailoverv1.TLSAuthClientsYes,
			data:        map[string][]byte{"ca.crt": ca, "tls.crt": cert},
			wantCerts:   0,
		},
		{
			name:        "a key without its certificate yields no client certificate",
			authClients: redisfailoverv1.TLSAuthClientsYes,
			data:        map[string][]byte{"ca.crt": ca, "tls.key": key},
			wantCerts:   0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a := assert.New(t)
			rf := tlsConfigRF(tc.authClients)

			cfg, err := tlsConfigFor(tlsSecretService(rf, tc.data), rf)
			if !a.NoError(err) {
				return
			}
			if !a.NotNil(cfg) {
				return
			}
			a.Len(cfg.Certificates, tc.wantCerts)
			a.NotNil(cfg.RootCAs, "the CA bundle must always be loaded")
			a.Equal(GetRedisName(rf), cfg.ServerName)
		})
	}
}

func TestTLSConfigForDisabledDialsPlaintext(t *testing.T) {
	t.Parallel()
	a := assert.New(t)
	rf := tlsConfigRF(redisfailoverv1.TLSAuthClientsYes)
	rf.Spec.TLS.Enabled = false

	cfg, err := tlsConfigFor(&mK8SService.Services{}, rf)
	a.NoError(err)
	a.Nil(cfg, "TLS off must yield the nil config that makes callers dial plaintext")
}

func TestTLSConfigForRejectsSecretWithoutCA(t *testing.T) {
	t.Parallel()
	cert, key := tlsTestClientPair(t)

	cases := []struct {
		name string
		data map[string][]byte
	}{
		{"no ca.crt at all", map[string][]byte{"tls.crt": cert, "tls.key": key}},
		{"an empty ca.crt", map[string][]byte{"ca.crt": {}, "tls.crt": cert, "tls.key": key}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a := assert.New(t)
			rf := tlsConfigRF(redisfailoverv1.TLSAuthClientsNo)

			_, err := tlsConfigFor(tlsSecretService(rf, tc.data), rf)
			if a.Error(err) {
				a.Contains(err.Error(), "ca.crt")
			}
		})
	}
}

// tlsTestCA mints a self-signed CA certificate in PEM form.
func tlsTestCA(t *testing.T) []byte {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("x509.CreateCertificate: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// tlsTestClientPair mints a self-signed leaf certificate and its key, both in
// PEM form, standing in for the tls.crt/tls.key a cert-manager secret carries.
func tlsTestClientPair(t *testing.T) (certPEM, keyPEM []byte) {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "test-client"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("x509.CreateCertificate: %v", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(priv)})
	return certPEM, keyPEM
}
