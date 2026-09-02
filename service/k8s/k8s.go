package k8s

import (
	cmclientset "github.com/cert-manager/cert-manager/pkg/client/clientset/versioned"
	apiextensionscli "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	"k8s.io/client-go/kubernetes"

	redisfailoverclientset "github.com/freshworks/redis-operator/client/k8s/clientset/versioned"
	"github.com/freshworks/redis-operator/log"
	"github.com/freshworks/redis-operator/metrics"
)

// Service is the K8s service entrypoint.
type Services interface {
	ConfigMap
	Secret
	Pod
	PodDisruptionBudget
	RedisFailover
	Service
	RBAC
	Deployment
	StatefulSet
	Certificate
}

type services struct {
	ConfigMap
	Secret
	Pod
	PodDisruptionBudget
	RedisFailover
	Service
	RBAC
	Deployment
	StatefulSet
	Certificate
}

// New returns a new Kubernetes service.
func New(kubecli kubernetes.Interface, crdcli redisfailoverclientset.Interface, apiextcli apiextensionscli.Interface, cmcli cmclientset.Interface, logger log.Logger, metricsRecorder metrics.Recorder) Services {
	return &services{
		ConfigMap:           NewConfigMapService(kubecli, logger, metricsRecorder),
		Secret:              NewSecretService(kubecli, logger, metricsRecorder),
		Pod:                 NewPodService(kubecli, logger, metricsRecorder),
		PodDisruptionBudget: NewPodDisruptionBudgetService(kubecli, logger, metricsRecorder),
		RedisFailover:       NewRedisFailoverService(crdcli, logger, metricsRecorder),
		Service:             NewServiceService(kubecli, logger, metricsRecorder),
		RBAC:                NewRBACService(kubecli, logger, metricsRecorder),
		Deployment:          NewDeploymentService(kubecli, logger, metricsRecorder),
		StatefulSet:         NewStatefulSetService(kubecli, logger, metricsRecorder),
		Certificate:         NewCertificateService(cmcli, logger, metricsRecorder),
	}
}
