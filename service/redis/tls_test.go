package redis

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
)

func TestBuildTLSConfigNilOnEmptyCA(t *testing.T) {
	cfg, err := BuildTLSConfig(nil, nil, nil, "redis.example.com")
	assert.NoError(t, err)
	assert.Nil(t, cfg)

	cfg, err = BuildTLSConfig([]byte{}, nil, nil, "redis.example.com")
	assert.NoError(t, err)
	assert.Nil(t, cfg)
}

func TestBuildTLSConfigRejectsEmptyServerName(t *testing.T) {
	ca := selfSignedCAPEM(t)
	_, err := BuildTLSConfig(ca, nil, nil, "")
	if assert.Error(t, err) {
		assert.Contains(t, err.Error(), "ServerName must not be empty")
	}
}

func TestBuildTLSConfigRejectsInvalidCA(t *testing.T) {
	_, err := BuildTLSConfig([]byte("not a pem"), nil, nil, "redis.example.com")
	if assert.Error(t, err) {
		assert.Contains(t, err.Error(), "no usable certificates")
	}
}

func TestBuildTLSConfigSucceeds(t *testing.T) {
	ca := selfSignedCAPEM(t)
	cfg, err := BuildTLSConfig(ca, nil, nil, "redis.example.com")
	if !assert.NoError(t, err) {
		return
	}
	if !assert.NotNil(t, cfg) {
		return
	}
	assert.Equal(t, "redis.example.com", cfg.ServerName)
	assert.NotNil(t, cfg.RootCAs)
	assert.Equal(t, uint16(0x0303), cfg.MinVersion, "MinVersion must be TLS 1.2 (0x0303)")
	assert.Empty(t, cfg.Certificates, "no client cert expected when certPEM/keyPEM are nil")
}

func TestBuildTLSConfigLoadsClientCertificate(t *testing.T) {
	ca := selfSignedCAPEM(t)
	clientCert, clientKey := selfSignedClientPEM(t)

	cfg, err := BuildTLSConfig(ca, clientCert, clientKey, "redis.example.com")
	if !assert.NoError(t, err) {
		return
	}
	if !assert.NotNil(t, cfg) {
		return
	}
	if !assert.Len(t, cfg.Certificates, 1, "client certificate must be loaded into tls.Config") {
		return
	}
	assert.NotNil(t, cfg.Certificates[0].PrivateKey)
	assert.NotEmpty(t, cfg.Certificates[0].Certificate)
}

func TestBuildTLSConfigRejectsCertWithoutKey(t *testing.T) {
	ca := selfSignedCAPEM(t)
	clientCert, _ := selfSignedClientPEM(t)

	_, err := BuildTLSConfig(ca, clientCert, nil, "redis.example.com")
	if assert.Error(t, err) {
		assert.Contains(t, err.Error(), "must be supplied together")
	}
}

func TestBuildTLSConfigRejectsKeyWithoutCert(t *testing.T) {
	ca := selfSignedCAPEM(t)
	_, clientKey := selfSignedClientPEM(t)

	_, err := BuildTLSConfig(ca, nil, clientKey, "redis.example.com")
	if assert.Error(t, err) {
		assert.Contains(t, err.Error(), "must be supplied together")
	}
}

func TestBuildTLSConfigRejectsMalformedClientKeyPair(t *testing.T) {
	ca := selfSignedCAPEM(t)

	_, err := BuildTLSConfig(ca, []byte("not a cert"), []byte("not a key"), "redis.example.com")
	if assert.Error(t, err) {
		assert.Contains(t, err.Error(), "loading client certificate")
	}
}

func TestBuildTLSConfigRejectsClientMaterialWithoutCA(t *testing.T) {
	clientCert, clientKey := selfSignedClientPEM(t)

	_, err := BuildTLSConfig(nil, clientCert, clientKey, "redis.example.com")
	if assert.Error(t, err) {
		assert.Contains(t, err.Error(), "caPEM must be supplied")
	}

	// Cert-only must also error, not silently fall back to plaintext.
	_, err = BuildTLSConfig(nil, clientCert, nil, "redis.example.com")
	if assert.Error(t, err) {
		assert.Contains(t, err.Error(), "caPEM must be supplied")
	}
}

// selfSignedCAPEM produces a freshly-minted self-signed CA certificate
// in PEM form for use in BuildTLSConfig tests.
func selfSignedCAPEM(t *testing.T) []byte {
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

// selfSignedClientPEM produces a freshly-minted self-signed leaf
// certificate plus its PKCS#1 private key, both PEM-encoded, for use as
// a client certificate in BuildTLSConfig tests.
func selfSignedClientPEM(t *testing.T) (certPEM, keyPEM []byte) {
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
