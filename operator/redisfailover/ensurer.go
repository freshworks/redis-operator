package redisfailover

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	redisfailoverv1 "github.com/freshworks/redis-operator/api/redisfailover/v1"
	"github.com/freshworks/redis-operator/metrics"
	rfservice "github.com/freshworks/redis-operator/operator/redisfailover/service"
)

// Ensure is called to ensure all of the resources associated with a RedisFailover are created
func (w *RedisFailoverHandler) Ensure(rf *redisfailoverv1.RedisFailover, labels map[string]string, or []metav1.OwnerReference, metricsClient metrics.Recorder) error {
	if rf.Spec.Redis.Exporter.Enabled {
		if err := w.rfService.EnsureRedisService(rf, labels, or); err != nil {
			return err
		}
	} else {
		if err := w.rfService.EnsureNotPresentRedisService(rf); err != nil {
			return err
		}
	}

	sentinelsAllowed := rf.SentinelsAllowed()
	if sentinelsAllowed {
		if err := w.rfService.EnsureSentinelService(rf, labels, or); err != nil {
			return err
		}
	}

	if err := w.rfService.EnsureRedisMasterService(rf, labels, or); err != nil {
		return err
	}

	if err := w.rfService.EnsureRedisSlaveService(rf, labels, or); err != nil {
		return err
	}

	// TLS Certificate must exist before the pods are created so the Secret
	// the pods mount is populated. EnsureRedisCertificate is a no-op when
	// TLS is disabled or the user supplied their own Secret.
	if err := w.rfService.EnsureRedisCertificate(rf, labels, or); err != nil {
		return err
	}

	// Publish a CA-only Secret (ca.crt without any private key) derived from
	// the TLS Secret so clients can verify the server under tightly scoped
	// RBAC. No-op when TLS is disabled; defers gracefully until the TLS
	// Secret's ca.crt is available.
	// The hash it returns pins the pod templates to the TLS material that
	// was read here, so a renewed certificate rolls Redis and Sentinel.
	tlsHash, err := w.rfService.EnsureRedisCACertSecret(rf, labels, or)
	if err != nil {
		return err
	}

	// Everything from here on shapes the pods: the ConfigMaps they read at
	// startup and the templates they are created from. With TLS on and no
	// certificate in the Secret yet there is nothing to pin them to, the
	// pods could not start against the missing volume anyway, and writing
	// them now would roll them again as soon as the hash appears on the
	// next pass. Wait for the material; the objects written above do not
	// depend on it.
	if rfservice.TLSEnabled(rf) && tlsHash == "" {
		w.logger.WithField("redisfailover", rf.ObjectMeta.Name).WithField("namespace", rf.ObjectMeta.Namespace).Infof("TLS secret %s has no certificate yet, waiting before writing the redis and sentinel config and workloads", rfservice.GetTLSSecretName(rf))
		return nil
	}

	if sentinelsAllowed {
		if err := w.rfService.EnsureSentinelConfigMap(rf, labels, or); err != nil {
			return err
		}
	}
	if err := w.rfService.EnsureRedisShutdownConfigMap(rf, labels, or); err != nil {
		return err
	}
	if err := w.rfService.EnsureRedisReadinessConfigMap(rf, labels, or); err != nil {
		return err
	}
	if err := w.rfService.EnsureRedisConfigMap(rf, labels, or); err != nil {
		return err
	}
	if err := w.rfService.EnsureRedisStatefulset(rf, labels, or, tlsHash); err != nil {
		return err
	}

	if sentinelsAllowed {
		if err := w.rfService.EnsureSentinelDeployment(rf, labels, or, tlsHash); err != nil {
			return err
		}
	}

	return nil
}
