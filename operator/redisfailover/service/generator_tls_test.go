package service_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	cmapi "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	cmmeta "github.com/cert-manager/cert-manager/pkg/apis/meta/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	redisfailoverv1 "github.com/freshworks/redis-operator/api/redisfailover/v1"
	"github.com/freshworks/redis-operator/log"
	"github.com/freshworks/redis-operator/metrics"
	mK8SService "github.com/freshworks/redis-operator/mocks/service/k8s"
	rfservice "github.com/freshworks/redis-operator/operator/redisfailover/service"
)

// generateTLSRF returns a RedisFailover with TLS enabled via cert-manager.
func generateTLSRF() *redisfailoverv1.RedisFailover {
	rf := generateRF()
	rf.Spec.Redis.Image = "redis:7.2"
	rf.Spec.Sentinel.Image = "redis:7.2"
	rf.Spec.Redis.Port = 6379
	rf.Spec.Redis.Exporter.Enabled = true
	rf.Spec.Sentinel.Exporter.Enabled = true
	rf.Spec.TLS = &redisfailoverv1.TLSSettings{
		Enabled:     true,
		AuthClients: redisfailoverv1.TLSAuthClientsNo,
		CertManager: &redisfailoverv1.CertManagerSettings{
			IssuerRef: cmmeta.ObjectReference{
				Name:  "test-ca",
				Kind:  "Issuer",
				Group: "cert-manager.io",
			},
		},
	}
	return rf
}

func ensureSucceeded(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("ensure call failed: %v", err)
	}
}

func TestRedisConfigMapHasTLSDirectives(t *testing.T) {
	a := assert.New(t)
	rf := generateTLSRF()

	var got *corev1.ConfigMap
	ms := &mK8SService.Services{}
	ms.On("CreateOrUpdateConfigMap", rf.Namespace, mock.MatchedBy(func(cm *corev1.ConfigMap) bool {
		if cm.Name == rfservice.GetRedisName(rf) {
			got = cm
			return true
		}
		return false
	})).Return(nil)

	gen := rfservice.NewRedisFailoverKubeClient(ms, log.DummyLogger{}, metrics.Dummy, "cluster.local")
	ensureSucceeded(t, gen.EnsureRedisConfigMap(rf, nil, nil))

	if !a.NotNil(got) {
		return
	}
	conf := got.Data["redis.conf"]
	a.Contains(conf, "tls-port 6379")
	a.Contains(conf, "port 0")
	a.Contains(conf, "tls-cert-file /tls/tls.crt")
	a.Contains(conf, "tls-key-file /tls/tls.key")
	a.Contains(conf, "tls-ca-cert-file /tls/ca.crt")
	a.Contains(conf, "tls-auth-clients no")
	a.Contains(conf, "tls-replication yes")
	// Plain "port 6379" must not appear when TLS is on.
	a.NotContains(conf, "\nport 6379")
}

func TestSentinelConfigMapHasTLSDirectives(t *testing.T) {
	a := assert.New(t)
	rf := generateTLSRF()

	var got *corev1.ConfigMap
	ms := &mK8SService.Services{}
	ms.On("CreateOrUpdateConfigMap", rf.Namespace, mock.MatchedBy(func(cm *corev1.ConfigMap) bool {
		if cm.Name == rfservice.GetSentinelName(rf) {
			got = cm
			return true
		}
		return false
	})).Return(nil)

	gen := rfservice.NewRedisFailoverKubeClient(ms, log.DummyLogger{}, metrics.Dummy, "cluster.local")
	ensureSucceeded(t, gen.EnsureSentinelConfigMap(rf, nil, nil))

	if !a.NotNil(got) {
		return
	}
	conf := got.Data["sentinel.conf"]
	a.Contains(conf, "tls-port 26379")
	a.Contains(conf, "port 0")
	a.Contains(conf, "tls-cert-file /tls/tls.crt")
	a.Contains(conf, "tls-auth-clients no")
	a.Contains(conf, "tls-replication yes")
}

func TestRedisStatefulSetHasTLSVolume(t *testing.T) {
	a := assert.New(t)
	rf := generateTLSRF()

	got := redisStatefulSetFor(t, rf, "")
	if !a.NotNil(got) {
		return
	}
	expectedSecret := rfservice.GetTLSSecretName(rf)

	// Volume present and points at the right secret.
	tlsVol := findVolume(got.Spec.Template.Spec.Volumes, "redis-tls")
	if a.NotNil(tlsVol, "redis-tls volume must be present") {
		a.NotNil(tlsVol.Secret)
		if tlsVol.Secret != nil {
			a.Equal(expectedSecret, tlsVol.Secret.SecretName)
			if a.NotNil(tlsVol.Secret.DefaultMode, "TLS secret must not fall back to the world-readable default mode") {
				a.Equal(int32(0440), *tlsVol.Secret.DefaultMode,
					"TLS secret files must be readable only by the owner and the pod fsGroup")
			}
		}
	}

	// Mount present on the redis container.
	redisContainer := got.Spec.Template.Spec.Containers[0]
	a.True(hasTLSMount(redisContainer.VolumeMounts),
		"redis container must mount the TLS secret read-only at /tls")
}

func TestSentinelDeploymentHasTLSVolume(t *testing.T) {
	a := assert.New(t)
	rf := generateTLSRF()

	got := sentinelDeploymentFor(t, rf, "")
	if !a.NotNil(got) {
		return
	}

	tlsVol := findVolume(got.Spec.Template.Spec.Volumes, "redis-tls")
	if a.NotNil(tlsVol, "redis-tls volume must be present on the sentinel pod") {
		a.NotNil(tlsVol.Secret)
		if tlsVol.Secret != nil {
			a.Equal(rfservice.GetTLSSecretName(rf), tlsVol.Secret.SecretName)
			if a.NotNil(tlsVol.Secret.DefaultMode, "TLS secret must not fall back to the world-readable default mode") {
				a.Equal(int32(0440), *tlsVol.Secret.DefaultMode,
					"TLS secret files must be readable only by the owner and the pod fsGroup")
			}
		}
	}

	sentinelContainer := got.Spec.Template.Spec.Containers[0]
	a.True(hasTLSMount(sentinelContainer.VolumeMounts),
		"sentinel container must mount the TLS secret read-only at /tls")
}

// A pod securityContext supplied by the user replaces the operator's default
// wholesale, so it may carry no fsGroup. Secret files are then owned by
// root:root and 0440 would be unreadable by a container running as non-root,
// so the mode must stay at the kubelet default in that case.
func TestTLSVolumeKeepsDefaultModeWithoutFSGroup(t *testing.T) {
	a := assert.New(t)
	runAsUser := int64(1000)
	noFSGroup := &corev1.PodSecurityContext{RunAsUser: &runAsUser}

	rf := generateTLSRF()
	rf.Spec.Redis.SecurityContext = noFSGroup
	rf.Spec.Sentinel.SecurityContext = noFSGroup

	sts := redisStatefulSetFor(t, rf, "")
	if a.NotNil(sts) {
		tlsVol := findVolume(sts.Spec.Template.Spec.Volumes, "redis-tls")
		if a.NotNil(tlsVol) && a.NotNil(tlsVol.Secret) {
			a.Nil(tlsVol.Secret.DefaultMode,
				"without an fsGroup the redis TLS volume must keep the kubelet default mode")
		}
	}

	deploy := sentinelDeploymentFor(t, rf, "")
	if a.NotNil(deploy) {
		tlsVol := findVolume(deploy.Spec.Template.Spec.Volumes, "redis-tls")
		if a.NotNil(tlsVol) && a.NotNil(tlsVol.Secret) {
			a.Nil(tlsVol.Secret.DefaultMode,
				"without an fsGroup the sentinel TLS volume must keep the kubelet default mode")
		}
	}
}

func findVolume(volumes []corev1.Volume, name string) *corev1.Volume {
	for i := range volumes {
		if volumes[i].Name == name {
			return &volumes[i]
		}
	}
	return nil
}

func hasTLSMount(mounts []corev1.VolumeMount) bool {
	for _, m := range mounts {
		if m.Name == "redis-tls" && m.MountPath == "/tls" && m.ReadOnly {
			return true
		}
	}
	return false
}

func redisStatefulSetFor(t *testing.T, rf *redisfailoverv1.RedisFailover, tlsHash string) *appsv1.StatefulSet {
	t.Helper()

	var got *appsv1.StatefulSet
	ms := &mK8SService.Services{}
	ms.On("CreateOrUpdateStatefulSet", rf.Namespace, mock.MatchedBy(func(ss *appsv1.StatefulSet) bool {
		got = ss
		return true
	})).Return(nil)
	ms.On("CreateOrUpdatePodDisruptionBudget", rf.Namespace, mock.Anything).Return(nil).Maybe()
	ms.On("GetStatefulSet", rf.Namespace, mock.Anything).Return(nil, apierrors.NewNotFound(schema.GroupResource{}, "")).Maybe()

	gen := rfservice.NewRedisFailoverKubeClient(ms, log.DummyLogger{}, metrics.Dummy, "cluster.local")
	ensureSucceeded(t, gen.EnsureRedisStatefulset(rf, nil, nil, tlsHash))
	return got
}

func sentinelDeploymentFor(t *testing.T, rf *redisfailoverv1.RedisFailover, tlsHash string) *appsv1.Deployment {
	t.Helper()

	var got *appsv1.Deployment
	ms := &mK8SService.Services{}
	ms.On("CreateOrUpdateDeployment", rf.Namespace, mock.MatchedBy(func(d *appsv1.Deployment) bool {
		got = d
		return true
	})).Return(nil)
	ms.On("CreateOrUpdatePodDisruptionBudget", rf.Namespace, mock.Anything).Return(nil).Maybe()

	gen := rfservice.NewRedisFailoverKubeClient(ms, log.DummyLogger{}, metrics.Dummy, "cluster.local")
	ensureSucceeded(t, gen.EnsureSentinelDeployment(rf, nil, nil, tlsHash))
	return got
}

func TestRedisExporterHasTLSEnv(t *testing.T) {
	a := assert.New(t)
	rf := generateTLSRF()

	var got *appsv1.StatefulSet
	ms := &mK8SService.Services{}
	ms.On("CreateOrUpdateStatefulSet", rf.Namespace, mock.MatchedBy(func(ss *appsv1.StatefulSet) bool {
		got = ss
		return true
	})).Return(nil)
	ms.On("CreateOrUpdatePodDisruptionBudget", rf.Namespace, mock.Anything).Return(nil).Maybe()
	ms.On("GetStatefulSet", rf.Namespace, mock.Anything).Return(nil, apierrors.NewNotFound(schema.GroupResource{}, "")).Maybe()

	gen := rfservice.NewRedisFailoverKubeClient(ms, log.DummyLogger{}, metrics.Dummy, "cluster.local")
	ensureSucceeded(t, gen.EnsureRedisStatefulset(rf, nil, nil, ""))

	if !a.NotNil(got) {
		return
	}
	var exporter *corev1.Container
	for i := range got.Spec.Template.Spec.Containers {
		c := &got.Spec.Template.Spec.Containers[i]
		if c.Name == "redis-exporter" {
			exporter = c
			break
		}
	}
	if !a.NotNil(exporter, "redis exporter sidecar must be present") {
		return
	}
	env := envToMap(exporter.Env)
	a.Equal("/tls/tls.crt", env["REDIS_EXPORTER_TLS_CLIENT_CERT_FILE"])
	a.Equal("/tls/tls.key", env["REDIS_EXPORTER_TLS_CLIENT_KEY_FILE"])
	a.Equal("/tls/ca.crt", env["REDIS_EXPORTER_TLS_CA_CERT_FILE"])
	a.Contains(env["REDIS_ADDR"], "rediss://")
}

func TestSentinelExporterTLSAddr(t *testing.T) {
	a := assert.New(t)
	rf := generateTLSRF()

	var got *appsv1.Deployment
	ms := &mK8SService.Services{}
	ms.On("CreateOrUpdateDeployment", rf.Namespace, mock.MatchedBy(func(d *appsv1.Deployment) bool {
		got = d
		return true
	})).Return(nil)
	ms.On("CreateOrUpdatePodDisruptionBudget", rf.Namespace, mock.Anything).Return(nil).Maybe()
	ms.On("GetStatefulSet", rf.Namespace, mock.Anything).Return(nil, apierrors.NewNotFound(schema.GroupResource{}, "")).Maybe()

	gen := rfservice.NewRedisFailoverKubeClient(ms, log.DummyLogger{}, metrics.Dummy, "cluster.local")
	ensureSucceeded(t, gen.EnsureSentinelDeployment(rf, nil, nil, ""))

	if !a.NotNil(got) {
		return
	}
	var exporter *corev1.Container
	for i := range got.Spec.Template.Spec.Containers {
		c := &got.Spec.Template.Spec.Containers[i]
		if c.Name == "sentinel-exporter" {
			exporter = c
			break
		}
	}
	if !a.NotNil(exporter, "sentinel exporter sidecar must be present") {
		return
	}
	env := envToMap(exporter.Env)
	a.Equal("rediss://127.0.0.1:26379", env["REDIS_ADDR"])
	a.Equal("/tls/tls.crt", env["REDIS_EXPORTER_TLS_CLIENT_CERT_FILE"])
}

func TestRedisShutdownScriptUsesTLS(t *testing.T) {
	a := assert.New(t)
	rf := generateTLSRF()

	var got *corev1.ConfigMap
	ms := &mK8SService.Services{}
	ms.On("CreateOrUpdateConfigMap", rf.Namespace, mock.MatchedBy(func(cm *corev1.ConfigMap) bool {
		if cm.Name == rfservice.GetRedisShutdownConfigMapName(rf) {
			got = cm
			return true
		}
		return false
	})).Return(nil)

	gen := rfservice.NewRedisFailoverKubeClient(ms, log.DummyLogger{}, metrics.Dummy, "cluster.local")
	ensureSucceeded(t, gen.EnsureRedisShutdownConfigMap(rf, nil, nil))

	if !a.NotNil(got) {
		return
	}
	script := got.Data["shutdown.sh"]
	a.Contains(script, "--tls")
	a.Contains(script, "--cacert /tls/ca.crt")
	a.Contains(script, "--cert /tls/tls.crt")
	a.Contains(script, "--key /tls/tls.key")
}

func TestRedisReadinessScriptUsesTLS(t *testing.T) {
	a := assert.New(t)
	rf := generateTLSRF()

	var got *corev1.ConfigMap
	ms := &mK8SService.Services{}
	ms.On("CreateOrUpdateConfigMap", rf.Namespace, mock.MatchedBy(func(cm *corev1.ConfigMap) bool {
		if cm.Name == rfservice.GetRedisReadinessName(rf) {
			got = cm
			return true
		}
		return false
	})).Return(nil)

	gen := rfservice.NewRedisFailoverKubeClient(ms, log.DummyLogger{}, metrics.Dummy, "cluster.local")
	ensureSucceeded(t, gen.EnsureRedisReadinessConfigMap(rf, nil, nil))

	if !a.NotNil(got) {
		return
	}
	script := got.Data["ready.sh"]
	a.Contains(script, "--tls")
	a.Contains(script, "--cacert /tls/ca.crt")
}

func TestRedisLivenessProbeUsesTLS(t *testing.T) {
	a := assert.New(t)
	rf := generateTLSRF()

	var got *appsv1.StatefulSet
	ms := &mK8SService.Services{}
	ms.On("CreateOrUpdateStatefulSet", rf.Namespace, mock.MatchedBy(func(ss *appsv1.StatefulSet) bool {
		got = ss
		return true
	})).Return(nil)
	ms.On("CreateOrUpdatePodDisruptionBudget", rf.Namespace, mock.Anything).Return(nil).Maybe()
	ms.On("GetStatefulSet", rf.Namespace, mock.Anything).Return(nil, apierrors.NewNotFound(schema.GroupResource{}, "")).Maybe()

	gen := rfservice.NewRedisFailoverKubeClient(ms, log.DummyLogger{}, metrics.Dummy, "cluster.local")
	ensureSucceeded(t, gen.EnsureRedisStatefulset(rf, nil, nil, ""))

	if !a.NotNil(got) {
		return
	}
	probe := got.Spec.Template.Spec.Containers[0].LivenessProbe
	if !a.NotNil(probe) || !a.NotNil(probe.Exec) {
		return
	}
	cmd := strings.Join(probe.Exec.Command, " ")
	a.Contains(cmd, "--tls")
	a.Contains(cmd, "--cacert /tls/ca.crt")
	a.Contains(cmd, "-h localhost",
		"probe must dial -h localhost so the host matches a SAN entry of the in-pod TLS cert")
	a.NotContains(cmd, "$(hostname)",
		"$(hostname) returns the bare pod name and is not covered by the cert SAN list")
}

func TestSentinelProbeUsesTLS(t *testing.T) {
	a := assert.New(t)
	rf := generateTLSRF()

	var got *appsv1.Deployment
	ms := &mK8SService.Services{}
	ms.On("CreateOrUpdateDeployment", rf.Namespace, mock.MatchedBy(func(d *appsv1.Deployment) bool {
		got = d
		return true
	})).Return(nil)
	ms.On("CreateOrUpdatePodDisruptionBudget", rf.Namespace, mock.Anything).Return(nil).Maybe()
	ms.On("GetStatefulSet", rf.Namespace, mock.Anything).Return(nil, apierrors.NewNotFound(schema.GroupResource{}, "")).Maybe()

	gen := rfservice.NewRedisFailoverKubeClient(ms, log.DummyLogger{}, metrics.Dummy, "cluster.local")
	ensureSucceeded(t, gen.EnsureSentinelDeployment(rf, nil, nil, ""))

	if !a.NotNil(got) {
		return
	}
	probe := got.Spec.Template.Spec.Containers[0].LivenessProbe
	if !a.NotNil(probe) || !a.NotNil(probe.Exec) {
		return
	}
	cmd := strings.Join(probe.Exec.Command, " ")
	a.Contains(cmd, "--tls")
	a.Contains(cmd, "-h localhost",
		"sentinel probe must dial -h localhost so the host matches a SAN entry of the in-pod TLS cert")
	a.NotContains(cmd, "$(hostname)",
		"$(hostname) returns the bare pod name and is not covered by the cert SAN list")
}

func TestTLSSecretNamePrecedence(t *testing.T) {
	a := assert.New(t)

	rfCM := generateTLSRF()
	a.Equal("rftls-"+rfCM.Name, rfservice.GetTLSSecretName(rfCM),
		"cert-manager managed mode should use generated default")

	rfCMOverride := generateTLSRF()
	rfCMOverride.Spec.TLS.CertManager.SecretName = "custom-secret"
	a.Equal("custom-secret", rfservice.GetTLSSecretName(rfCMOverride),
		"certManager.secretName should override the default")

	rfBYO := generateTLSRF()
	rfBYO.Spec.TLS.CertManager = nil
	rfBYO.Spec.TLS.CertificateSecret = &redisfailoverv1.LocalSecretReference{SecretName: "byo-tls"}
	a.Equal("byo-tls", rfservice.GetTLSSecretName(rfBYO),
		"certificateSecret.secretName should take precedence")

	rfOff := generateRF()
	a.Equal("", rfservice.GetTLSSecretName(rfOff),
		"with TLS disabled the helper must return an empty string")
}

func TestTLSCACertSecretName(t *testing.T) {
	a := assert.New(t)

	rfCM := generateTLSRF()
	a.Equal("rftls-"+rfCM.Name+"-ca", rfservice.GetTLSCACertSecretName(rfCM),
		"default CA secret name derives from the TLS secret name with a -ca suffix")

	rfOverride := generateTLSRF()
	rfOverride.Spec.TLS.CACertSecretName = "custom-ca"
	a.Equal("custom-ca", rfservice.GetTLSCACertSecretName(rfOverride),
		"caCertSecretName must override the derived default")

	rfBYO := generateTLSRF()
	rfBYO.Spec.TLS.CertManager = nil
	rfBYO.Spec.TLS.CertificateSecret = &redisfailoverv1.LocalSecretReference{SecretName: "byo-tls"}
	a.Equal("byo-tls-ca", rfservice.GetTLSCACertSecretName(rfBYO),
		"default CA secret name tracks the bring-your-own TLS secret name")

	rfOff := generateRF()
	a.Equal("", rfservice.GetTLSCACertSecretName(rfOff),
		"with TLS disabled the helper must return an empty string")
}

func envToMap(env []corev1.EnvVar) map[string]string {
	out := make(map[string]string, len(env))
	for _, e := range env {
		out[e.Name] = e.Value
	}
	return out
}

func TestEnsureRedisCertificateCreatesCertManagerCert(t *testing.T) {
	a := assert.New(t)
	rf := generateTLSRF()
	rf.Spec.TLS.CertManager.Duration = &metav1.Duration{Duration: 30 * 24 * time.Hour}
	rf.Spec.TLS.CertManager.RenewBefore = &metav1.Duration{Duration: 10 * 24 * time.Hour}

	var got *cmapi.Certificate
	ms := &mK8SService.Services{}
	ms.On("CreateOrUpdateCertificate", rf.Namespace, mock.MatchedBy(func(c *cmapi.Certificate) bool {
		got = c
		return true
	})).Return(nil)

	gen := rfservice.NewRedisFailoverKubeClient(ms, log.DummyLogger{}, metrics.Dummy, "cluster.local")
	ensureSucceeded(t, gen.EnsureRedisCertificate(rf, nil, nil))

	if !a.NotNil(got) {
		return
	}
	a.Equal(rfservice.GetTLSCertificateName(rf), got.Name)
	a.Equal(rf.Namespace, got.Namespace)
	a.Equal(rfservice.GetTLSSecretName(rf), got.Spec.SecretName)
	a.Equal("test-ca", got.Spec.IssuerRef.Name)
	a.Equal("Issuer", got.Spec.IssuerRef.Kind)
	a.Equal("cert-manager.io", got.Spec.IssuerRef.Group)
	a.Equal(rf.Spec.TLS.CertManager.Duration, got.Spec.Duration)
	a.Equal(rf.Spec.TLS.CertManager.RenewBefore, got.Spec.RenewBefore)

	// DNS SAN coverage: each Service must appear in short and FQDN form, plus the
	// per-pod wildcard for the headless service.
	dnsSet := make(map[string]struct{}, len(got.Spec.DNSNames))
	for _, d := range got.Spec.DNSNames {
		dnsSet[d] = struct{}{}
	}
	for _, want := range []string{
		rfservice.GetRedisName(rf),
		rfservice.GetRedisMasterName(rf) + "." + rf.Namespace + ".svc.cluster.local",
		rfservice.GetSentinelName(rf) + "." + rf.Namespace + ".svc",
		"*." + rfservice.GetRedisName(rf) + "." + rf.Namespace + ".svc.cluster.local",
	} {
		_, ok := dnsSet[want]
		a.Truef(ok, "expected DNS SAN %q to be present (got %v)", want, got.Spec.DNSNames)
	}

	// Usages must include both server and client auth so the cert works for
	// the cluster pods AND the operator client.
	usages := make(map[cmapi.KeyUsage]struct{}, len(got.Spec.Usages))
	for _, u := range got.Spec.Usages {
		usages[u] = struct{}{}
	}
	_, hasServer := usages[cmapi.UsageServerAuth]
	_, hasClient := usages[cmapi.UsageClientAuth]
	a.True(hasServer, "Certificate must declare server auth usage")
	a.True(hasClient, "Certificate must declare client auth usage")
}

func TestEnsureRedisCertificateAppendsExtraSANs(t *testing.T) {
	a := assert.New(t)
	rf := generateTLSRF()
	rf.Spec.TLS.CertManager.ExtraSANs = []string{
		"redis.example.com",
		"10.0.0.50",
		"redis.internal.corp",
		"2001:db8::1",
	}

	var got *cmapi.Certificate
	ms := &mK8SService.Services{}
	ms.On("CreateOrUpdateCertificate", rf.Namespace, mock.MatchedBy(func(c *cmapi.Certificate) bool {
		got = c
		return true
	})).Return(nil)

	gen := rfservice.NewRedisFailoverKubeClient(ms, log.DummyLogger{}, metrics.Dummy, "cluster.local")
	ensureSucceeded(t, gen.EnsureRedisCertificate(rf, nil, nil))

	if !a.NotNil(got) {
		return
	}

	dnsSet := make(map[string]struct{}, len(got.Spec.DNSNames))
	for _, d := range got.Spec.DNSNames {
		dnsSet[d] = struct{}{}
	}
	ipSet := make(map[string]struct{}, len(got.Spec.IPAddresses))
	for _, ip := range got.Spec.IPAddresses {
		ipSet[ip] = struct{}{}
	}

	// Extra DNS SANs are added to DNSNames.
	for _, want := range []string{"redis.example.com", "redis.internal.corp"} {
		_, ok := dnsSet[want]
		a.Truef(ok, "expected extra DNS SAN %q to be present (got %v)", want, got.Spec.DNSNames)
	}

	// Extra IP SANs are auto-classified into IPAddresses (IPv4 + IPv6).
	for _, want := range []string{"10.0.0.50", "2001:db8::1"} {
		_, ok := ipSet[want]
		a.Truef(ok, "expected extra IP SAN %q to be present (got %v)", want, got.Spec.IPAddresses)
	}

	// IP-looking entries must not leak into DNSNames.
	for _, ip := range []string{"10.0.0.50", "2001:db8::1"} {
		_, leaked := dnsSet[ip]
		a.Falsef(leaked, "IP %q must not appear in DNSNames", ip)
	}

	// Computed service SANs must still be present — extras are additive.
	for _, want := range []string{
		rfservice.GetRedisName(rf),
		rfservice.GetRedisMasterName(rf) + "." + rf.Namespace + ".svc.cluster.local",
		"*." + rfservice.GetRedisName(rf) + "." + rf.Namespace + ".svc.cluster.local",
	} {
		_, ok := dnsSet[want]
		a.Truef(ok, "computed DNS SAN %q must remain after extras merge (got %v)", want, got.Spec.DNSNames)
	}
}

func TestEnsureRedisCertificateCoversInPodDials(t *testing.T) {
	a := assert.New(t)
	rf := generateTLSRF()

	var got *cmapi.Certificate
	ms := &mK8SService.Services{}
	ms.On("CreateOrUpdateCertificate", rf.Namespace, mock.MatchedBy(func(c *cmapi.Certificate) bool {
		got = c
		return true
	})).Return(nil)

	gen := rfservice.NewRedisFailoverKubeClient(ms, log.DummyLogger{}, metrics.Dummy, "cluster.local")
	ensureSucceeded(t, gen.EnsureRedisCertificate(rf, nil, nil))

	if !a.NotNil(got) {
		return
	}

	dnsSet := make(map[string]struct{}, len(got.Spec.DNSNames))
	for _, d := range got.Spec.DNSNames {
		dnsSet[d] = struct{}{}
	}
	ipSet := make(map[string]struct{}, len(got.Spec.IPAddresses))
	for _, ip := range got.Spec.IPAddresses {
		ipSet[ip] = struct{}{}
	}

	// localhost must be a DNS SAN so the liveness probe (-h localhost)
	// verifies against the same cert.
	_, hasLocalhost := dnsSet["localhost"]
	a.True(hasLocalhost, "localhost DNS SAN must be present (got %v)", got.Spec.DNSNames)

	// 127.0.0.1 and ::1 must be IP SANs so the redis_exporter sidecar
	// (REDIS_ADDR=rediss://127.0.0.1:...) and the sentinel monitor
	// target (sentinel monitor mymaster 127.0.0.1 ...) verify against
	// the same cert.
	for _, ip := range []string{"127.0.0.1", "::1"} {
		_, ok := ipSet[ip]
		a.Truef(ok, "expected loopback IP SAN %q to be present (got %v)", ip, got.Spec.IPAddresses)
	}
}

func TestEnsureRedisCertificateUsesConfiguredClusterDomain(t *testing.T) {
	a := assert.New(t)
	rf := generateTLSRF()

	var got *cmapi.Certificate
	ms := &mK8SService.Services{}
	ms.On("CreateOrUpdateCertificate", rf.Namespace, mock.MatchedBy(func(c *cmapi.Certificate) bool {
		got = c
		return true
	})).Return(nil)

	// cozystack's default cluster domain
	gen := rfservice.NewRedisFailoverKubeClient(ms, log.DummyLogger{}, metrics.Dummy, "cozy.local")
	ensureSucceeded(t, gen.EnsureRedisCertificate(rf, nil, nil))

	if !a.NotNil(got) {
		return
	}

	dnsSet := make(map[string]struct{}, len(got.Spec.DNSNames))
	for _, d := range got.Spec.DNSNames {
		dnsSet[d] = struct{}{}
	}

	// FQDN SANs must use the configured cluster domain.
	for _, want := range []string{
		rfservice.GetRedisName(rf) + "." + rf.Namespace + ".svc.cozy.local",
		rfservice.GetRedisMasterName(rf) + "." + rf.Namespace + ".svc.cozy.local",
		rfservice.GetSentinelName(rf) + "." + rf.Namespace + ".svc.cozy.local",
		"*." + rfservice.GetRedisName(rf) + "." + rf.Namespace + ".svc.cozy.local",
	} {
		_, ok := dnsSet[want]
		a.Truef(ok, "expected DNS SAN %q for custom cluster domain (got %v)", want, got.Spec.DNSNames)
	}

	// The default cluster.local FQDN must NOT appear when a custom
	// domain is configured — otherwise the cert would silently mask
	// SAN-mismatch bugs on non-default clusters.
	for _, unwanted := range []string{
		rfservice.GetRedisName(rf) + "." + rf.Namespace + ".svc.cluster.local",
		"*." + rfservice.GetRedisName(rf) + "." + rf.Namespace + ".svc.cluster.local",
	} {
		_, ok := dnsSet[unwanted]
		a.Falsef(ok, "cluster.local SAN %q must not be present when --cluster-domain=cozy.local (got %v)", unwanted, got.Spec.DNSNames)
	}
}

func TestEnsureRedisCertificateLoopbackSANsArePresentWithoutExtras(t *testing.T) {
	a := assert.New(t)
	rf := generateTLSRF()

	var got *cmapi.Certificate
	ms := &mK8SService.Services{}
	ms.On("CreateOrUpdateCertificate", rf.Namespace, mock.MatchedBy(func(c *cmapi.Certificate) bool {
		got = c
		return true
	})).Return(nil)

	gen := rfservice.NewRedisFailoverKubeClient(ms, log.DummyLogger{}, metrics.Dummy, "cluster.local")
	ensureSucceeded(t, gen.EnsureRedisCertificate(rf, nil, nil))

	if !a.NotNil(got) {
		return
	}
	// Loopback IPs are part of the default SAN set; this asserts the
	// default does not silently regress to an empty IPAddresses slice
	// (which would break the in-pod TLS self-dials).
	a.ElementsMatch([]string{"127.0.0.1", "::1"}, got.Spec.IPAddresses,
		"default IPAddresses must include only the loopback IPs when no extras are set")
}

func TestEnsureRedisCertificateSkipsForBYOSecret(t *testing.T) {
	a := assert.New(t)
	rf := generateTLSRF()
	rf.Spec.TLS.CertManager = nil
	rf.Spec.TLS.CertificateSecret = &redisfailoverv1.LocalSecretReference{SecretName: "byo-tls"}

	ms := &mK8SService.Services{}
	gen := rfservice.NewRedisFailoverKubeClient(ms, log.DummyLogger{}, metrics.Dummy, "cluster.local")

	a.NoError(gen.EnsureRedisCertificate(rf, nil, nil))
	ms.AssertNotCalled(t, "CreateOrUpdateCertificate", mock.Anything, mock.Anything)
}

func TestEnsureRedisCertificateSkipsWhenDisabled(t *testing.T) {
	a := assert.New(t)
	rf := generateRF() // TLS = nil

	ms := &mK8SService.Services{}
	gen := rfservice.NewRedisFailoverKubeClient(ms, log.DummyLogger{}, metrics.Dummy, "cluster.local")

	a.NoError(gen.EnsureRedisCertificate(rf, nil, nil))
	ms.AssertNotCalled(t, "CreateOrUpdateCertificate", mock.Anything, mock.Anything)
}

func TestEnsureRedisCACertSecretPublishesCAOnly(t *testing.T) {
	a := assert.New(t)
	rf := generateTLSRF()
	srcName := rfservice.GetTLSSecretName(rf)

	leaf := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: srcName, Namespace: rf.Namespace},
		Type:       corev1.SecretTypeTLS,
		Data: map[string][]byte{
			"tls.crt": []byte("LEAF-CERT"),
			"tls.key": []byte("LEAF-KEY"),
			"ca.crt":  []byte("CA-PEM"),
		},
	}

	var got *corev1.Secret
	ms := &mK8SService.Services{}
	ms.On("GetSecret", rf.Namespace, srcName).Return(leaf, nil)
	ms.On("CreateOrUpdateSecret", rf.Namespace, mock.MatchedBy(func(s *corev1.Secret) bool {
		got = s
		return true
	})).Return(nil)

	gen := rfservice.NewRedisFailoverKubeClient(ms, log.DummyLogger{}, metrics.Dummy, "cluster.local")
	_, err := gen.EnsureRedisCACertSecret(rf, nil, nil)
	ensureSucceeded(t, err)
	ms.AssertExpectations(t)

	if !a.NotNil(got) {
		return
	}
	a.Equal(rfservice.GetTLSCACertSecretName(rf), got.Name)
	a.Equal(rf.Namespace, got.Namespace)
	a.Equal(corev1.SecretTypeOpaque, got.Type)
	a.Equal([]byte("CA-PEM"), got.Data["ca.crt"])
	// The whole point of the feature: the published Secret must carry the CA
	// certificate and nothing else — no private key, no leaf cert.
	a.Len(got.Data, 1, "CA-only Secret must hold exactly one key (ca.crt)")
	_, hasKey := got.Data["tls.key"]
	a.False(hasKey, "CA-only Secret must not contain tls.key")
	_, hasCert := got.Data["tls.crt"]
	a.False(hasCert, "CA-only Secret must not contain tls.crt")
}

func TestEnsureRedisCACertSecretDefersWhenTLSSecretMissing(t *testing.T) {
	a := assert.New(t)
	rf := generateTLSRF()
	srcName := rfservice.GetTLSSecretName(rf)

	ms := &mK8SService.Services{}
	ms.On("GetSecret", rf.Namespace, srcName).
		Return(nil, apierrors.NewNotFound(corev1.Resource("secrets"), srcName))

	gen := rfservice.NewRedisFailoverKubeClient(ms, log.DummyLogger{}, metrics.Dummy, "cluster.local")
	// Not-yet-populated TLS secret (cert-manager is asynchronous) must be a
	// soft skip, not an error that blocks the rest of the reconcile.
	_, err := gen.EnsureRedisCACertSecret(rf, nil, nil)
	a.NoError(err)
	ms.AssertNotCalled(t, "CreateOrUpdateSecret", mock.Anything, mock.Anything)
	ms.AssertExpectations(t)
}

func TestEnsureRedisCACertSecretReturnsErrorOnGetFailure(t *testing.T) {
	a := assert.New(t)
	rf := generateTLSRF()
	srcName := rfservice.GetTLSSecretName(rf)

	ms := &mK8SService.Services{}
	ms.On("GetSecret", rf.Namespace, srcName).Return(nil, errors.New("api server unavailable"))

	gen := rfservice.NewRedisFailoverKubeClient(ms, log.DummyLogger{}, metrics.Dummy, "cluster.local")
	// A non-NotFound error is a real failure and must propagate.
	_, err := gen.EnsureRedisCACertSecret(rf, nil, nil)
	a.Error(err)
	ms.AssertNotCalled(t, "CreateOrUpdateSecret", mock.Anything, mock.Anything)
	ms.AssertExpectations(t)
}

func TestEnsureRedisCACertSecretDefersWhenCACrtMissing(t *testing.T) {
	a := assert.New(t)
	rf := generateTLSRF()
	srcName := rfservice.GetTLSSecretName(rf)

	leaf := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: srcName, Namespace: rf.Namespace},
		Type:       corev1.SecretTypeTLS,
		Data: map[string][]byte{
			"tls.crt": []byte("LEAF-CERT"),
			"tls.key": []byte("LEAF-KEY"),
		},
	}

	ms := &mK8SService.Services{}
	ms.On("GetSecret", rf.Namespace, srcName).Return(leaf, nil)

	gen := rfservice.NewRedisFailoverKubeClient(ms, log.DummyLogger{}, metrics.Dummy, "cluster.local")
	_, err := gen.EnsureRedisCACertSecret(rf, nil, nil)
	a.NoError(err)
	ms.AssertNotCalled(t, "CreateOrUpdateSecret", mock.Anything, mock.Anything)
	ms.AssertExpectations(t)
}

func TestEnsureRedisCACertSecretSkipsWhenDisabled(t *testing.T) {
	a := assert.New(t)
	rf := generateRF() // TLS == nil

	ms := &mK8SService.Services{}
	gen := rfservice.NewRedisFailoverKubeClient(ms, log.DummyLogger{}, metrics.Dummy, "cluster.local")

	_, err := gen.EnsureRedisCACertSecret(rf, nil, nil)
	a.NoError(err)
	ms.AssertNotCalled(t, "GetSecret", mock.Anything, mock.Anything)
	ms.AssertNotCalled(t, "CreateOrUpdateSecret", mock.Anything, mock.Anything)
}

func TestEnsureRedisCACertSecretBYOWithOverrideName(t *testing.T) {
	a := assert.New(t)
	rf := generateTLSRF()
	rf.Spec.TLS.CertManager = nil
	rf.Spec.TLS.CertificateSecret = &redisfailoverv1.LocalSecretReference{SecretName: "byo-tls"}
	rf.Spec.TLS.CACertSecretName = "my-ca-bundle"

	leaf := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "byo-tls", Namespace: rf.Namespace},
		Type:       corev1.SecretTypeTLS,
		Data:       map[string][]byte{"ca.crt": []byte("BYO-CA")},
	}

	var got *corev1.Secret
	ms := &mK8SService.Services{}
	ms.On("GetSecret", rf.Namespace, "byo-tls").Return(leaf, nil)
	ms.On("CreateOrUpdateSecret", rf.Namespace, mock.MatchedBy(func(s *corev1.Secret) bool {
		got = s
		return true
	})).Return(nil)

	gen := rfservice.NewRedisFailoverKubeClient(ms, log.DummyLogger{}, metrics.Dummy, "cluster.local")
	_, err := gen.EnsureRedisCACertSecret(rf, nil, nil)
	ensureSucceeded(t, err)
	ms.AssertExpectations(t)

	if !a.NotNil(got) {
		return
	}
	a.Equal("my-ca-bundle", got.Name, "explicit caCertSecretName override must be honored in BYO mode")
	a.Equal([]byte("BYO-CA"), got.Data["ca.crt"])
}

// tlsHashAnnotation pins the published annotation name; downstream tooling and
// users read it, so a rename is a breaking change and must fail here.
const tlsHashAnnotation = "redis-failover.freshworks.com/tls-secret-hash"

// tlsHashFor drives the CA-publish path against a TLS secret with the given
// contents and returns the hash it hands back for the pod templates.
func tlsHashFor(t *testing.T, rf *redisfailoverv1.RedisFailover, data map[string][]byte) string {
	t.Helper()

	srcName := rfservice.GetTLSSecretName(rf)
	ms := &mK8SService.Services{}
	ms.On("GetSecret", rf.Namespace, srcName).Return(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: srcName, Namespace: rf.Namespace},
		Data:       data,
	}, nil)
	ms.On("CreateOrUpdateSecret", rf.Namespace, mock.Anything).Return(nil).Maybe()

	gen := rfservice.NewRedisFailoverKubeClient(ms, log.DummyLogger{}, metrics.Dummy, "cluster.local")
	hash, err := gen.EnsureRedisCACertSecret(rf, nil, nil)
	ensureSucceeded(t, err)
	return hash
}

func TestTLSSecretHashStampedOnPodTemplates(t *testing.T) {
	a := assert.New(t)
	rf := generateTLSRF()
	rf.Spec.Redis.PodAnnotations = map[string]string{"team": "db"}
	rf.Spec.Sentinel.PodAnnotations = map[string]string{"team": "db"}

	hash := tlsHashFor(t, rf, map[string][]byte{"tls.crt": []byte("LEAF"), "ca.crt": []byte("CA")})
	a.NotEmpty(hash, "a populated TLS secret must yield a hash to pin the pods to")

	sts := redisStatefulSetFor(t, rf, hash)
	if a.NotNil(sts) {
		a.Equal(hash, sts.Spec.Template.Annotations[tlsHashAnnotation],
			"redis pod template must carry the TLS content hash")
		a.Equal("db", sts.Spec.Template.Annotations["team"],
			"stamping the hash must not drop the user's pod annotations")
	}

	deploy := sentinelDeploymentFor(t, rf, hash)
	if a.NotNil(deploy) {
		a.Equal(hash, deploy.Spec.Template.Annotations[tlsHashAnnotation],
			"sentinel pod template must carry the TLS content hash")
		a.Equal("db", deploy.Spec.Template.Annotations["team"],
			"stamping the hash must not drop the user's pod annotations")
	}

	// The spec maps are the live objects from the informer cache; stamping
	// must not write through to them.
	a.NotContains(rf.Spec.Redis.PodAnnotations, tlsHashAnnotation)
	a.NotContains(rf.Spec.Sentinel.PodAnnotations, tlsHashAnnotation)
}

func TestTLSSecretHashTracksSecretContent(t *testing.T) {
	a := assert.New(t)
	rf := generateTLSRF()

	first := tlsHashFor(t, rf, map[string][]byte{"tls.crt": []byte("LEAF-1"), "ca.crt": []byte("CA-1")})
	again := tlsHashFor(t, rf, map[string][]byte{"tls.crt": []byte("LEAF-1"), "ca.crt": []byte("CA-1")})
	renewed := tlsHashFor(t, rf, map[string][]byte{"tls.crt": []byte("LEAF-2"), "ca.crt": []byte("CA-1")})
	rotatedCA := tlsHashFor(t, rf, map[string][]byte{"tls.crt": []byte("LEAF-1"), "ca.crt": []byte("CA-2")})

	a.Equal(first, again, "unchanged TLS material must not roll the pods")
	a.NotEqual(first, renewed, "a renewed certificate must change the hash")
	a.NotEqual(first, rotatedCA,
		"a rotated CA must change the hash: redis loads ca.crt once at startup")

	empty := tlsHashFor(t, rf, map[string][]byte{"ca.crt": []byte("CA-1")})
	a.Empty(empty, "with no tls.crt there is nothing to pin the pods to yet")
}

// TestTLSSecretHashTracksAuthClients pins the roll behaviour of
// spec.tls.authClients. The directive reaches a running server only through
// the generated config file, which Redis reads once at startup, and the Redis
// StatefulSet rolls OnDelete. Left out of the pod template, a change to it is
// admitted and then lands pod by pod at whatever unrelated restart comes next
// — a node drain, an OOM, or the renewal roll months later.
func TestTLSSecretHashTracksAuthClients(t *testing.T) {
	a := assert.New(t)
	data := map[string][]byte{"tls.crt": []byte("LEAF"), "ca.crt": []byte("CA")}

	hashFor := func(authClients string) string {
		rf := generateTLSRF()
		rf.Spec.TLS.AuthClients = authClients
		return tlsHashFor(t, rf, data)
	}

	no := hashFor(redisfailoverv1.TLSAuthClientsNo)
	optional := hashFor(redisfailoverv1.TLSAuthClientsOptional)
	yes := hashFor(redisfailoverv1.TLSAuthClientsYes)

	a.Equal(no, hashFor(redisfailoverv1.TLSAuthClientsNo),
		"an unchanged directive must not roll the pods")
	a.Equal(no, hashFor(""),
		"an unset directive defaults to no, so both spellings must hash alike")
	a.NotEqual(no, yes,
		"demanding client certificates must roll the pods that enforce it")
	a.NotEqual(no, optional,
		"accepting client certificates must roll the pods that enforce it")
	a.NotEqual(optional, yes,
		"optional and yes are different servers and must not hash alike")
}

func TestTLSSecretHashAbsentWhenTLSDisabled(t *testing.T) {
	a := assert.New(t)
	rf := generateRF()
	rf.Spec.Redis.PodAnnotations = map[string]string{"team": "db"}
	rf.Spec.Sentinel.PodAnnotations = map[string]string{"team": "db"}

	sts := redisStatefulSetFor(t, rf, "")
	if a.NotNil(sts) {
		a.NotContains(sts.Spec.Template.Annotations, tlsHashAnnotation,
			"no TLS means no hash annotation on the redis pod template")
		a.Equal("db", sts.Spec.Template.Annotations["team"])
	}

	deploy := sentinelDeploymentFor(t, rf, "")
	if a.NotNil(deploy) {
		a.NotContains(deploy.Spec.Template.Annotations, tlsHashAnnotation,
			"no TLS means no hash annotation on the sentinel pod template")
		a.Equal("db", deploy.Spec.Template.Annotations["team"])
	}
}

// TestPlaintextProbesKeepUpstreamCommands pins the probe commands rendered for
// a failover without TLS to the exact strings v3.3.5 emits. Those strings are
// part of the pod template, so any drift makes every running Redis StatefulSet
// and Sentinel Deployment differ from its live spec and roll once on the first
// reconcile after an operator upgrade — for users who never asked for TLS.
func TestPlaintextProbesKeepUpstreamCommands(t *testing.T) {
	tests := []struct {
		name              string
		mutate            func(*redisfailoverv1.RedisFailover)
		redisLiveness     string
		sentinelLiveness  string
		sentinelReadiness string
	}{
		{
			name:              "defaults",
			mutate:            func(*redisfailoverv1.RedisFailover) {},
			redisLiveness:     "redis-cli -h $(hostname) -p 6379 --user pinger --pass pingpass --no-auth-warning ping | grep PONG",
			sentinelLiveness:  "redis-cli -h $(hostname) -p 26379 ping",
			sentinelReadiness: "redis-cli -h $(hostname) -p 26379 sentinel get-master-addr-by-name mymaster | head -n 1 | grep -vq '127.0.0.1'",
		},
		{
			name:              "custom redis port",
			mutate:            func(rf *redisfailoverv1.RedisFailover) { rf.Spec.Redis.Port = 6380 },
			redisLiveness:     "redis-cli -h $(hostname) -p 6380 --user pinger --pass pingpass --no-auth-warning ping | grep PONG",
			sentinelLiveness:  "redis-cli -h $(hostname) -p 26379 ping",
			sentinelReadiness: "redis-cli -h $(hostname) -p 26379 sentinel get-master-addr-by-name mymaster | head -n 1 | grep -vq '127.0.0.1'",
		},
		{
			name:              "master name from the failover",
			mutate:            func(rf *redisfailoverv1.RedisFailover) { rf.Spec.Sentinel.DisableMyMaster = true },
			redisLiveness:     "redis-cli -h $(hostname) -p 6379 --user pinger --pass pingpass --no-auth-warning ping | grep PONG",
			sentinelLiveness:  "redis-cli -h $(hostname) -p 26379 ping",
			sentinelReadiness: "redis-cli -h $(hostname) -p 26379 sentinel get-master-addr-by-name test | head -n 1 | grep -vq '127.0.0.1'",
		},
		{
			name:              "valkey engine",
			mutate:            func(rf *redisfailoverv1.RedisFailover) { rf.Spec.Engine = redisfailoverv1.ValkeyEngine },
			redisLiveness:     "valkey-cli -h $(hostname) -p 6379 --user pinger --pass pingpass --no-auth-warning ping | grep PONG",
			sentinelLiveness:  "valkey-cli -h $(hostname) -p 26379 ping",
			sentinelReadiness: "valkey-cli -h $(hostname) -p 26379 sentinel get-master-addr-by-name mymaster | head -n 1 | grep -vq '127.0.0.1'",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			a := assert.New(t)
			rf := generateRF()
			rf.Spec.Redis.Port = 6379
			test.mutate(rf)
			a.Nil(rf.Spec.TLS, "this test covers the failover that never opted into TLS")

			ss := redisStatefulSetFor(t, rf, "")
			sd := sentinelDeploymentFor(t, rf, "")
			if !a.NotNil(ss) || !a.NotNil(sd) {
				return
			}

			got := map[string]string{
				"redis liveness":     execCommand(t, ss.Spec.Template.Spec.Containers[0].LivenessProbe),
				"sentinel liveness":  execCommand(t, sd.Spec.Template.Spec.Containers[0].LivenessProbe),
				"sentinel readiness": execCommand(t, sd.Spec.Template.Spec.Containers[0].ReadinessProbe),
			}
			a.Equal(test.redisLiveness, got["redis liveness"])
			a.Equal(test.sentinelLiveness, got["sentinel liveness"])
			a.Equal(test.sentinelReadiness, got["sentinel readiness"])

			for probe, cmd := range got {
				a.NotContains(cmd, "localhost",
					"%s dials $(hostname) without TLS; localhost is only needed to match a certificate SAN", probe)
				a.NotContains(cmd, "--tls", "%s must not carry TLS flags when TLS is off", probe)
			}
		})
	}
}

// execCommand returns the shell script of a probe, after checking it is still
// wrapped in the "sh -c" form the expectations are written against.
func execCommand(t *testing.T, probe *corev1.Probe) string {
	t.Helper()
	if probe == nil || probe.Exec == nil {
		t.Fatal("probe has no exec command")
	}
	cmd := probe.Exec.Command
	if len(cmd) != 3 || cmd[0] != "sh" || cmd[1] != "-c" {
		t.Fatalf("probe command is not an sh -c script: %q", cmd)
	}
	return cmd[2]
}
