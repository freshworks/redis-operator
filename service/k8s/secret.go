package k8s

import (
	"context"
	"reflect"

	"github.com/freshworks/redis-operator/log"
	"github.com/freshworks/redis-operator/metrics"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Secret interacts with k8s to get secrets
type Secret interface {
	GetSecret(namespace, name string) (*corev1.Secret, error)
	CreateSecret(namespace string, secret *corev1.Secret) error
	UpdateSecret(namespace string, secret *corev1.Secret) error
	CreateOrUpdateSecret(namespace string, secret *corev1.Secret) error
}

// SecretService is the secret service implementation using API calls to kubernetes.
type SecretService struct {
	kubeClient      kubernetes.Interface
	logger          log.Logger
	metricsRecorder metrics.Recorder
}

func NewSecretService(kubeClient kubernetes.Interface, logger log.Logger, metricsRecorder metrics.Recorder) *SecretService {

	logger = logger.With("service", "k8s.secret")
	return &SecretService{
		kubeClient:      kubeClient,
		logger:          logger,
		metricsRecorder: metricsRecorder,
	}
}

func (s *SecretService) GetSecret(namespace, name string) (*corev1.Secret, error) {

	secret, err := s.kubeClient.CoreV1().Secrets(namespace).Get(context.TODO(), name, metav1.GetOptions{})
	recordMetrics(namespace, "Secret", name, "GET", err, s.metricsRecorder)
	if err != nil {
		return nil, err
	}

	return secret, err
}

func (s *SecretService) CreateSecret(namespace string, secret *corev1.Secret) error {
	_, err := s.kubeClient.CoreV1().Secrets(namespace).Create(context.TODO(), secret, metav1.CreateOptions{})
	recordMetrics(namespace, "Secret", secret.GetName(), "CREATE", err, s.metricsRecorder)
	if err != nil {
		return err
	}
	s.logger.WithField("namespace", namespace).WithField("secret", secret.Name).Debugf("secret created")
	return nil
}

func (s *SecretService) UpdateSecret(namespace string, secret *corev1.Secret) error {
	_, err := s.kubeClient.CoreV1().Secrets(namespace).Update(context.TODO(), secret, metav1.UpdateOptions{})
	recordMetrics(namespace, "Secret", secret.GetName(), "UPDATE", err, s.metricsRecorder)
	if err != nil {
		return err
	}
	s.logger.WithField("namespace", namespace).WithField("secret", secret.Name).Debugf("secret updated")
	return nil
}

func (s *SecretService) CreateOrUpdateSecret(namespace string, secret *corev1.Secret) error {
	storedSecret, err := s.GetSecret(namespace, secret.Name)
	if err != nil {
		// If no resource we need to create.
		if errors.IsNotFound(err) {
			return s.CreateSecret(namespace, secret)
		}
		return err
	}

	// Skip the write when the desired content already matches what is stored.
	// The operator does not watch Secrets, so an unconditional update would not
	// loop, but it would still issue a redundant API write (bumping
	// resourceVersion) on every reconcile of the owning resource. Comparing the
	// managed content keeps the published Secret quiet between actual changes
	// (e.g. CA rotation).
	if storedSecret.Type == secret.Type && reflect.DeepEqual(storedSecret.Data, secret.Data) {
		return nil
	}

	// Already exists, need to Update.
	// Set the correct resource version to ensure we are on the latest version. This way the only valid
	// namespace is our spec(https://github.com/kubernetes/community/blob/master/contributors/devel/api-conventions.md#concurrency-control-and-consistency),
	// we will replace the current namespace state.
	secret.ResourceVersion = storedSecret.ResourceVersion
	return s.UpdateSecret(namespace, secret)
}
