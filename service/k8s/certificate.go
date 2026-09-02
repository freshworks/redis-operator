package k8s

import (
	"context"

	cmapi "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	cmclientset "github.com/cert-manager/cert-manager/pkg/client/clientset/versioned"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/freshworks/redis-operator/log"
	"github.com/freshworks/redis-operator/metrics"
)

// Certificate manages cert-manager Certificate resources.
//
// The interface mirrors the other service types in this package: a
// GET, a CREATE, an UPDATE and the convenience CreateOrUpdate that
// reads the live object first to forward its ResourceVersion.
type Certificate interface {
	GetCertificate(namespace string, name string) (*cmapi.Certificate, error)
	CreateCertificate(namespace string, cert *cmapi.Certificate) error
	UpdateCertificate(namespace string, cert *cmapi.Certificate) error
	CreateOrUpdateCertificate(namespace string, cert *cmapi.Certificate) error
}

// CertificateService talks to the cert-manager API.
type CertificateService struct {
	cmClient        cmclientset.Interface
	logger          log.Logger
	metricsRecorder metrics.Recorder
}

// NewCertificateService returns a Certificate KubeService.
func NewCertificateService(cmClient cmclientset.Interface, logger log.Logger, metricsRecorder metrics.Recorder) *CertificateService {
	logger = logger.With("service", "k8s.certificate")
	return &CertificateService{
		cmClient:        cmClient,
		logger:          logger,
		metricsRecorder: metricsRecorder,
	}
}

func (c *CertificateService) GetCertificate(namespace string, name string) (*cmapi.Certificate, error) {
	cert, err := c.cmClient.CertmanagerV1().Certificates(namespace).Get(context.TODO(), name, metav1.GetOptions{})
	recordMetrics(namespace, "Certificate", name, "GET", err, c.metricsRecorder)
	if err != nil {
		return nil, err
	}
	return cert, nil
}

func (c *CertificateService) CreateCertificate(namespace string, cert *cmapi.Certificate) error {
	_, err := c.cmClient.CertmanagerV1().Certificates(namespace).Create(context.TODO(), cert, metav1.CreateOptions{})
	recordMetrics(namespace, "Certificate", cert.GetName(), "CREATE", err, c.metricsRecorder)
	if err != nil {
		return err
	}
	c.logger.WithField("namespace", namespace).WithField("certificate", cert.Name).Debugf("certificate created")
	return nil
}

func (c *CertificateService) UpdateCertificate(namespace string, cert *cmapi.Certificate) error {
	_, err := c.cmClient.CertmanagerV1().Certificates(namespace).Update(context.TODO(), cert, metav1.UpdateOptions{})
	recordMetrics(namespace, "Certificate", cert.GetName(), "UPDATE", err, c.metricsRecorder)
	if err != nil {
		return err
	}
	c.logger.WithField("namespace", namespace).WithField("certificate", cert.Name).Debugf("certificate updated")
	return nil
}

func (c *CertificateService) CreateOrUpdateCertificate(namespace string, cert *cmapi.Certificate) error {
	stored, err := c.GetCertificate(namespace, cert.Name)
	if err != nil {
		if errors.IsNotFound(err) {
			return c.CreateCertificate(namespace, cert)
		}
		return err
	}
	cert.ResourceVersion = stored.ResourceVersion
	return c.UpdateCertificate(namespace, cert)
}
