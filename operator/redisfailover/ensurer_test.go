package redisfailover_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	redisfailoverv1 "github.com/freshworks/redis-operator/api/redisfailover/v1"
	"github.com/freshworks/redis-operator/log"
	"github.com/freshworks/redis-operator/metrics"
	mRFService "github.com/freshworks/redis-operator/mocks/operator/redisfailover/service"
	mK8SService "github.com/freshworks/redis-operator/mocks/service/k8s"
	rfOperator "github.com/freshworks/redis-operator/operator/redisfailover"
)

const (
	name      = "test"
	namespace = "testns"
)

func generateConfig() rfOperator.Config {
	return rfOperator.Config{
		ListenAddress: "1234",
		MetricsPath:   "/awesome",
	}
}

func generateRF(enableExporter bool, bootstrapping bool, disableMyMaster bool) *redisfailoverv1.RedisFailover {
	return &redisfailoverv1.RedisFailover{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: redisfailoverv1.RedisFailoverSpec{
			Redis: redisfailoverv1.RedisSettings{
				Replicas: int32(3),
				Exporter: redisfailoverv1.Exporter{
					Enabled: enableExporter,
				},
			},
			Sentinel: redisfailoverv1.SentinelSettings{
				Replicas:        int32(3),
				DisableMyMaster: disableMyMaster,
			},
			BootstrapNode: generateRFBootstrappingNode(bootstrapping),
		},
	}
}

func generateRFBootstrappingNode(bootstrapping bool) *redisfailoverv1.BootstrapSettings {
	if bootstrapping {
		return &redisfailoverv1.BootstrapSettings{
			Host: "127.0.0.1",
			Port: "6379",
		}
	}
	return nil
}

func TestEnsure(t *testing.T) {
	tests := []struct {
		name                        string
		exporter                    bool
		bootstrapping               bool
		bootstrappingAllowSentinels bool
	}{
		{
			name:                        "Call everything, use exporter",
			exporter:                    true,
			bootstrapping:               false,
			bootstrappingAllowSentinels: false,
		},
		{
			name:                        "Call everything, don't use exporter",
			exporter:                    false,
			bootstrapping:               false,
			bootstrappingAllowSentinels: false,
		},
		{
			name:                        "Only ensure Redis when bootstrapping",
			exporter:                    false,
			bootstrapping:               true,
			bootstrappingAllowSentinels: false,
		},
		{
			name:                        "call everything when bootstrapping allows sentinels",
			exporter:                    false,
			bootstrapping:               true,
			bootstrappingAllowSentinels: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)

			rf := generateRF(test.exporter, test.bootstrapping, false)
			if test.bootstrapping {
				rf.Spec.BootstrapNode.AllowSentinels = test.bootstrappingAllowSentinels
			}

			config := generateConfig()
			mk := &mK8SService.Services{}
			mrfc := &mRFService.RedisFailoverCheck{}
			mrfh := &mRFService.RedisFailoverHeal{}
			mrfs := &mRFService.RedisFailoverClient{}
			if test.exporter {
				mrfs.On("EnsureRedisService", rf, mock.Anything, mock.Anything).Once().Return(nil)
			} else {
				mrfs.On("EnsureNotPresentRedisService", rf).Once().Return(nil)
			}

			if !test.bootstrapping || test.bootstrappingAllowSentinels {
				mrfs.On("EnsureSentinelService", rf, mock.Anything, mock.Anything).Once().Return(nil)
				mrfs.On("EnsureSentinelConfigMap", rf, mock.Anything, mock.Anything).Once().Return(nil)
				mrfs.On("EnsureSentinelDeployment", rf, mock.Anything, mock.Anything, mock.Anything).Once().Return(nil)
			}

			mrfs.On("EnsureRedisMasterService", rf, mock.Anything, mock.Anything).Once().Return(nil)
			mrfs.On("EnsureRedisSlaveService", rf, mock.Anything, mock.Anything).Once().Return(nil)
			mrfs.On("EnsureRedisCertificate", rf, mock.Anything, mock.Anything).Once().Return(nil)
			mrfs.On("EnsureRedisCACertSecret", rf, mock.Anything, mock.Anything).Once().Return("", nil)
			mrfs.On("EnsureRedisConfigMap", rf, mock.Anything, mock.Anything).Once().Return(nil)
			mrfs.On("EnsureRedisShutdownConfigMap", rf, mock.Anything, mock.Anything).Once().Return(nil)
			mrfs.On("EnsureRedisReadinessConfigMap", rf, mock.Anything, mock.Anything).Once().Return(nil)
			mrfs.On("EnsureRedisStatefulset", rf, mock.Anything, mock.Anything, mock.Anything).Once().Return(nil)

			// Create the Kops client and call the valid logic.
			handler := rfOperator.NewRedisFailoverHandler(config, mrfs, mrfc, mrfh, mk, metrics.Dummy, log.Dummy)
			err := handler.Ensure(rf, map[string]string{}, []metav1.OwnerReference{}, metrics.Dummy)

			assert.NoError(err)
			mrfs.AssertExpectations(t)
		})
	}
}

// The TLS Secret is read once per reconcile, by the CA-publish step. The hash
// it returns has to reach both pod templates, or a renewed certificate never
// rolls the pods.
func TestEnsurePropagatesTLSHashToPodTemplates(t *testing.T) {
	assert := assert.New(t)

	const tlsHash = "0d9f1c2b3a"

	rf := generateRF(false, false, false)
	config := generateConfig()
	mk := &mK8SService.Services{}
	mrfc := &mRFService.RedisFailoverCheck{}
	mrfh := &mRFService.RedisFailoverHeal{}
	mrfs := &mRFService.RedisFailoverClient{}

	mrfs.On("EnsureNotPresentRedisService", rf).Once().Return(nil)
	mrfs.On("EnsureSentinelService", rf, mock.Anything, mock.Anything).Once().Return(nil)
	mrfs.On("EnsureSentinelConfigMap", rf, mock.Anything, mock.Anything).Once().Return(nil)
	mrfs.On("EnsureRedisMasterService", rf, mock.Anything, mock.Anything).Once().Return(nil)
	mrfs.On("EnsureRedisSlaveService", rf, mock.Anything, mock.Anything).Once().Return(nil)
	mrfs.On("EnsureRedisCertificate", rf, mock.Anything, mock.Anything).Once().Return(nil)
	mrfs.On("EnsureRedisConfigMap", rf, mock.Anything, mock.Anything).Once().Return(nil)
	mrfs.On("EnsureRedisShutdownConfigMap", rf, mock.Anything, mock.Anything).Once().Return(nil)
	mrfs.On("EnsureRedisReadinessConfigMap", rf, mock.Anything, mock.Anything).Once().Return(nil)

	mrfs.On("EnsureRedisCACertSecret", rf, mock.Anything, mock.Anything).Once().Return(tlsHash, nil)
	mrfs.On("EnsureRedisStatefulset", rf, mock.Anything, mock.Anything, tlsHash).Once().Return(nil)
	mrfs.On("EnsureSentinelDeployment", rf, mock.Anything, mock.Anything, tlsHash).Once().Return(nil)

	handler := rfOperator.NewRedisFailoverHandler(config, mrfs, mrfc, mrfh, mk, metrics.Dummy, log.Dummy)
	err := handler.Ensure(rf, map[string]string{}, []metav1.OwnerReference{}, metrics.Dummy)

	assert.NoError(err)
	mrfs.AssertExpectations(t)
}

// On a first install with TLS on, cert-manager writes the Secret after the
// RedisFailover is created. Writing the pod-facing objects before it exists
// leaves the pods on a missing volume and, once the Secret appears and its
// hash lands on the templates, rolls them a second time. They wait instead.
func TestEnsureWaitsForTLSMaterialBeforeWritingPodFacingObjects(t *testing.T) {
	assert := assert.New(t)

	rf := generateRF(false, false, false)
	rf.Spec.TLS = &redisfailoverv1.TLSSettings{
		Enabled:           true,
		CertificateSecret: &redisfailoverv1.LocalSecretReference{SecretName: "test-tls"},
	}
	config := generateConfig()
	mk := &mK8SService.Services{}
	mrfc := &mRFService.RedisFailoverCheck{}
	mrfh := &mRFService.RedisFailoverHeal{}
	mrfs := &mRFService.RedisFailoverClient{}

	mrfs.On("EnsureNotPresentRedisService", rf).Once().Return(nil)
	mrfs.On("EnsureSentinelService", rf, mock.Anything, mock.Anything).Once().Return(nil)
	mrfs.On("EnsureRedisMasterService", rf, mock.Anything, mock.Anything).Once().Return(nil)
	mrfs.On("EnsureRedisSlaveService", rf, mock.Anything, mock.Anything).Once().Return(nil)
	mrfs.On("EnsureRedisCertificate", rf, mock.Anything, mock.Anything).Once().Return(nil)
	mrfs.On("EnsureRedisCACertSecret", rf, mock.Anything, mock.Anything).Once().Return("", nil)
	podFacing := []string{"EnsureSentinelConfigMap", "EnsureRedisShutdownConfigMap", "EnsureRedisReadinessConfigMap", "EnsureRedisConfigMap"}
	for _, method := range podFacing {
		mrfs.On(method, rf, mock.Anything, mock.Anything).Return(nil).Maybe()
	}
	mrfs.On("EnsureRedisStatefulset", rf, mock.Anything, mock.Anything, mock.Anything).Return(nil).Maybe()
	mrfs.On("EnsureSentinelDeployment", rf, mock.Anything, mock.Anything, mock.Anything).Return(nil).Maybe()

	handler := rfOperator.NewRedisFailoverHandler(config, mrfs, mrfc, mrfh, mk, metrics.Dummy, log.Dummy)
	err := handler.Ensure(rf, map[string]string{}, []metav1.OwnerReference{}, metrics.Dummy)

	assert.NoError(err)
	mrfs.AssertExpectations(t)
	for _, method := range podFacing {
		mrfs.AssertNotCalled(t, method, rf, mock.Anything, mock.Anything)
	}
	mrfs.AssertNotCalled(t, "EnsureRedisStatefulset", rf, mock.Anything, mock.Anything, mock.Anything)
	mrfs.AssertNotCalled(t, "EnsureSentinelDeployment", rf, mock.Anything, mock.Anything, mock.Anything)
}
