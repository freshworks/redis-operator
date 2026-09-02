package service

import (
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	redisfailoverv1 "github.com/freshworks/redis-operator/api/redisfailover/v1"
	"github.com/freshworks/redis-operator/log"
	"github.com/freshworks/redis-operator/metrics"
	"github.com/freshworks/redis-operator/operator/redisfailover/util"
	"github.com/freshworks/redis-operator/service/k8s"
)

// RedisFailoverClient has the minimumm methods that a Redis failover controller needs to satisfy
// in order to talk with K8s
type RedisFailoverClient interface {
	EnsureSentinelService(rFailover *redisfailoverv1.RedisFailover, labels map[string]string, ownerRefs []metav1.OwnerReference) error
	EnsureSentinelConfigMap(rFailover *redisfailoverv1.RedisFailover, labels map[string]string, ownerRefs []metav1.OwnerReference) error
	EnsureSentinelDeployment(rFailover *redisfailoverv1.RedisFailover, labels map[string]string, ownerRefs []metav1.OwnerReference, tlsHash string) error
	EnsureRedisStatefulset(rFailover *redisfailoverv1.RedisFailover, labels map[string]string, ownerRefs []metav1.OwnerReference, tlsHash string) error
	EnsureRedisService(rFailover *redisfailoverv1.RedisFailover, labels map[string]string, ownerRefs []metav1.OwnerReference) error
	EnsureRedisMasterService(rFailover *redisfailoverv1.RedisFailover, labels map[string]string, ownerRefs []metav1.OwnerReference) error
	EnsureRedisSlaveService(rFailover *redisfailoverv1.RedisFailover, labels map[string]string, ownerRefs []metav1.OwnerReference) error
	EnsureRedisShutdownConfigMap(rFailover *redisfailoverv1.RedisFailover, labels map[string]string, ownerRefs []metav1.OwnerReference) error
	EnsureRedisReadinessConfigMap(rFailover *redisfailoverv1.RedisFailover, labels map[string]string, ownerRefs []metav1.OwnerReference) error
	EnsureRedisConfigMap(rFailover *redisfailoverv1.RedisFailover, labels map[string]string, ownerRefs []metav1.OwnerReference) error
	EnsureNotPresentRedisService(rFailover *redisfailoverv1.RedisFailover) error
	EnsureRedisCertificate(rFailover *redisfailoverv1.RedisFailover, labels map[string]string, ownerRefs []metav1.OwnerReference) error
	EnsureRedisCACertSecret(rFailover *redisfailoverv1.RedisFailover, labels map[string]string, ownerRefs []metav1.OwnerReference) (string, error)
}

// RedisFailoverKubeClient implements the required methods to talk with kubernetes
type RedisFailoverKubeClient struct {
	K8SService    k8s.Services
	logger        log.Logger
	metricsClient metrics.Recorder
	// clusterDomain is the cluster's DNS suffix (e.g. "cluster.local",
	// "cozy.local") used to template the FQDN SANs on the cert-manager
	// Certificate. Empty falls back to "cluster.local" so existing
	// deployments that never set --cluster-domain keep working.
	clusterDomain string
}

// NewRedisFailoverKubeClient creates a new RedisFailoverKubeClient.
// clusterDomain controls the *.svc.<domain> SAN entries on the
// generated cert-manager Certificate; pass an empty string to fall
// back to the upstream Kubernetes default ("cluster.local").
func NewRedisFailoverKubeClient(k8sService k8s.Services, logger log.Logger, metricsClient metrics.Recorder, clusterDomain string) *RedisFailoverKubeClient {
	if clusterDomain == "" {
		clusterDomain = "cluster.local"
	}
	return &RedisFailoverKubeClient{
		K8SService:    k8sService,
		logger:        logger,
		metricsClient: metricsClient,
		clusterDomain: clusterDomain,
	}
}

func generateSelectorLabels(component, name string) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":      name,
		"app.kubernetes.io/component": component,
		"app.kubernetes.io/part-of":   appLabel,
	}
}

func generateRedisDefaultRoleLabel() map[string]string {
	return generateRedisSlaveRoleLabel()
}

func generateRedisMasterRoleLabel() map[string]string {
	return map[string]string{
		redisRoleLabelKey: redisRoleLabelMaster,
	}
}

func generateRedisSlaveRoleLabel() map[string]string {
	return map[string]string{
		redisRoleLabelKey: redisRoleLabelSlave,
	}
}

func generateRedisMasterAnnotations() map[string]string {
	return map[string]string{
		clusterAutoscalerSafeToEvictAnnotationKey: clusterAutoscalerSafeToEvictAnnotationMaster,
	}
}

func generateRedisSlaveAnnotations() map[string]string {
	return map[string]string{
		clusterAutoscalerSafeToEvictAnnotationKey: clusterAutoscalerSafeToEvictAnnotationSlave,
	}
}

// EnsureSentinelService makes sure the sentinel service exists
func (r *RedisFailoverKubeClient) EnsureSentinelService(rf *redisfailoverv1.RedisFailover, labels map[string]string, ownerRefs []metav1.OwnerReference) error {
	svc := generateSentinelService(rf, labels, ownerRefs)
	err := r.K8SService.CreateOrUpdateService(rf.Namespace, svc)
	r.setEnsureOperationMetrics(svc.Namespace, svc.Name, "Service", rf.Name, err)
	return err
}

// EnsureSentinelConfigMap makes sure the sentinel configmap exists
func (r *RedisFailoverKubeClient) EnsureSentinelConfigMap(rf *redisfailoverv1.RedisFailover, labels map[string]string, ownerRefs []metav1.OwnerReference) error {
	cm := generateSentinelConfigMap(rf, labels, ownerRefs)
	err := r.K8SService.CreateOrUpdateConfigMap(rf.Namespace, cm)
	r.setEnsureOperationMetrics(cm.Namespace, cm.Name, "ConfigMap", rf.Name, err)
	return err
}

// EnsureSentinelDeployment makes sure the sentinel deployment exists in the desired state.
// tlsHash is stamped on the pod template so a renewed TLS Secret rolls the pods.
func (r *RedisFailoverKubeClient) EnsureSentinelDeployment(rf *redisfailoverv1.RedisFailover, labels map[string]string, ownerRefs []metav1.OwnerReference, tlsHash string) error {
	if !rf.Spec.Sentinel.DisablePodDisruptionBudget {
		if err := r.ensurePodDisruptionBudget(rf, sentinelName, sentinelRoleName, labels, ownerRefs); err != nil {
			return err
		}
	}
	d := generateSentinelDeployment(rf, labels, ownerRefs, tlsHash)
	err := r.K8SService.CreateOrUpdateDeployment(rf.Namespace, d)

	r.setEnsureOperationMetrics(d.Namespace, d.Name, "Deployment", rf.Name, err)
	return err
}

// EnsureRedisStatefulset makes sure the redis statefulset exists in the desired state.
// tlsHash is stamped on the pod template so a renewed TLS Secret rolls the pods.
func (r *RedisFailoverKubeClient) EnsureRedisStatefulset(rf *redisfailoverv1.RedisFailover, labels map[string]string, ownerRefs []metav1.OwnerReference, tlsHash string) error {
	if !rf.Spec.Redis.DisablePodDisruptionBudget {
		if err := r.ensurePodDisruptionBudget(rf, redisName, redisRoleName, labels, ownerRefs); err != nil {
			return err
		}
	}

	// Check and validate StatefulSet before creation/update
	if err := r.checkAndValidateStatefulSet(rf); err != nil {
		return err
	}

	// Generate and create/update StatefulSet
	ss := generateRedisStatefulSet(rf, labels, ownerRefs, tlsHash)
	err := r.K8SService.CreateOrUpdateStatefulSet(rf.Namespace, ss)

	r.setEnsureOperationMetrics(ss.Namespace, ss.Name, "StatefulSet", rf.Name, err)
	return err
}

// checkAndValidateStatefulSet checks if StatefulSet exists and validates maxmemory configuration
// Returns error if validation fails for new StatefulSet creation, logs warning for existing StatefulSets
func (r *RedisFailoverKubeClient) checkAndValidateStatefulSet(rf *redisfailoverv1.RedisFailover) error {
	// Check if StatefulSet already exists
	existingStatefulSet, err := r.K8SService.GetStatefulSet(rf.Namespace, GetRedisName(rf))
	statefulSetExists := err == nil && existingStatefulSet != nil

	// Run all validation checks
	// Add more validation functions here as needed
	isValidConfig := true
	var validationErrors []string

	// Validation 1: Validate maxmemory configuration
	if !r.validateMaxMemoryConfig(rf) {
		isValidConfig = false
		validationErrors = append(validationErrors, "maxmemory configuration exceeds allowed memory limits")
	}

	// Validation 2: Add more validations here in the future
	// Example:
	// if !r.validateSomeOtherConfig(rf) {
	//     isValidConfig = false
	//     validationErrors = append(validationErrors, "some other validation failed")
	// }

	// Handle validation failures
	if !isValidConfig {
		validationMsg := strings.Join(validationErrors, "; ")

		if statefulSetExists {
			// StatefulSet already exists - log warning and continue
			// Invalid configs will be filtered out when applying configs to running pods
			r.logger.WithField("redisfailover", rf.Name).WithField("namespace", rf.Namespace).Warningf("Configuration validation failed: %s. Invalid configs will be skipped when applying to running pods", validationMsg)

			// Record metric for validation warning on existing StatefulSet
			validationErr := fmt.Errorf("configuration validation warning: %s", validationMsg)
			r.setEnsureOperationMetrics(rf.Namespace, GetRedisName(rf), "StatefulSet", rf.Name, validationErr)

			return nil
		} else {
			// StatefulSet doesn't exist yet - block creation
			err := fmt.Errorf("configuration validation failed for RedisFailover %s: %s. Cannot create StatefulSet with invalid configuration", rf.Name, validationMsg)
			r.setEnsureOperationMetrics(rf.Namespace, GetRedisName(rf), "StatefulSet", rf.Name, err)
			return err
		}
	}

	return nil
}

// validateMaxMemoryConfig validates maxmemory configuration against CRD spec memory
func (r *RedisFailoverKubeClient) validateMaxMemoryConfig(rf *redisfailoverv1.RedisFailover) bool {
	// Get memory from CRD spec (prioritize Requests over Limits)
	var crdMemory int64

	// First priority: Check Requests
	if rf.Spec.Redis.Resources.Requests != nil {
		if memRequest := rf.Spec.Redis.Resources.Requests.Memory(); memRequest != nil {
			crdMemory = memRequest.Value()
		}
	}

	// Second priority: If Requests is 0, check Limits
	if crdMemory == 0 && rf.Spec.Redis.Resources.Limits != nil {
		if memLimit := rf.Spec.Redis.Resources.Limits.Memory(); memLimit != nil {
			crdMemory = memLimit.Value()
		}
	}

	// If no memory limits/requests specified, allow creation
	if crdMemory == 0 {
		return true
	}

	// Get the memory overhead percentage (default is 10%)
	reservedPodMemoryPercent := rf.Spec.Redis.ReservedPodMemoryPercent
	if reservedPodMemoryPercent <= 0 {
		reservedPodMemoryPercent = 10 // Default overhead
	}

	// Check each custom config line for maxmemory
	for _, configLine := range rf.Spec.Redis.CustomConfig {
		if strings.HasPrefix(configLine, "maxmemory ") {
			// Parse maxmemory value
			parts := strings.Fields(configLine)
			if len(parts) >= 2 {
				maxMemoryStr := parts[1]
				maxMemoryBytes, err := ParseMemorySize(maxMemoryStr)
				if err != nil {
					// Invalid memory format, reject
					return false
				}

				// Calculate allowed memory: CRD memory * (100 - overhead) / 100
				allowedMemory := crdMemory * int64(100-reservedPodMemoryPercent) / 100
				if maxMemoryBytes > allowedMemory {
					// maxmemory exceeds overhead limit, reject
					return false
				}
			}
		}
	}

	return true // Valid configuration
}

// EnsureRedisConfigMap makes sure the Redis ConfigMap exists
func (r *RedisFailoverKubeClient) EnsureRedisConfigMap(rf *redisfailoverv1.RedisFailover, labels map[string]string, ownerRefs []metav1.OwnerReference) error {

	password, err := k8s.GetRedisPassword(r.K8SService, rf)
	if err != nil {
		return err
	}

	cm := generateRedisConfigMap(rf, labels, ownerRefs, password)
	err = r.K8SService.CreateOrUpdateConfigMap(rf.Namespace, cm)

	r.setEnsureOperationMetrics(cm.Namespace, cm.Name, "ConfigMap", rf.Name, err)
	return err
}

// EnsureRedisShutdownConfigMap makes sure the redis configmap with shutdown script exists
func (r *RedisFailoverKubeClient) EnsureRedisShutdownConfigMap(rf *redisfailoverv1.RedisFailover, labels map[string]string, ownerRefs []metav1.OwnerReference) error {
	if rf.Spec.Redis.ShutdownConfigMap != "" {
		if _, err := r.K8SService.GetConfigMap(rf.Namespace, rf.Spec.Redis.ShutdownConfigMap); err != nil {
			return err
		}
	} else {
		cm := generateRedisShutdownConfigMap(rf, labels, ownerRefs)
		err := r.K8SService.CreateOrUpdateConfigMap(rf.Namespace, cm)
		r.setEnsureOperationMetrics(cm.Namespace, cm.Name, "ConfigMap", rf.Name, err)
		return err
	}
	return nil
}

// EnsureRedisReadinessConfigMap makes sure the redis configmap with shutdown script exists
func (r *RedisFailoverKubeClient) EnsureRedisReadinessConfigMap(rf *redisfailoverv1.RedisFailover, labels map[string]string, ownerRefs []metav1.OwnerReference) error {
	cm := generateRedisReadinessConfigMap(rf, labels, ownerRefs)
	err := r.K8SService.CreateOrUpdateConfigMap(rf.Namespace, cm)
	r.setEnsureOperationMetrics(cm.Namespace, cm.Name, "ConfigMap", rf.Name, err)
	return err
}

// EnsureRedisCertificate ensures the cert-manager Certificate resource
// exists when spec.tls.certManager is configured. It is a no-op for any
// other TLS mode (disabled, or bring-your-own-secret).
func (r *RedisFailoverKubeClient) EnsureRedisCertificate(rf *redisfailoverv1.RedisFailover, labels map[string]string, ownerRefs []metav1.OwnerReference) error {
	if !TLSEnabled(rf) || rf.Spec.TLS.CertManager == nil {
		return nil
	}
	cert := generateRedisCertificate(rf, labels, ownerRefs, r.clusterDomain)
	err := r.K8SService.CreateOrUpdateCertificate(rf.Namespace, cert)
	r.setEnsureOperationMetrics(cert.Namespace, cert.Name, "Certificate", rf.Name, err)
	return err
}

// EnsureRedisCACertSecret publishes an Opaque Secret containing only ca.crt,
// extracted from the cluster's TLS secret. The CA-only Secret never holds a
// private key, so RBAC can be scoped to it to let clients verify the Redis
// server without exposing tls.key. It is a no-op when TLS is disabled and
// applies to both cert-manager and bring-your-own-secret modes.
//
// The source TLS secret is populated asynchronously by cert-manager, or
// managed externally in bring-your-own mode, so a missing secret or a
// missing/empty ca.crt is treated as "not ready yet": the method logs and
// returns nil so the rest of the reconcile proceeds and a later pass
// republishes the CA certificate once it becomes available.
//
// It also returns a content hash of the TLS secret it read, folded together
// with the effective tls-auth-clients value, for the caller to stamp on the
// Redis and Sentinel pod templates. This is the only read of
// that secret in the Ensure phase, so the hash is derived here rather than
// from a second GET there; the check-and-heal phase reads it again for every
// Redis or Sentinel client it builds, see tlsConfigFor. The hash is empty
// whenever there is nothing to pin yet: TLS disabled, secret absent, or no
// tls.crt in it.
func (r *RedisFailoverKubeClient) EnsureRedisCACertSecret(rf *redisfailoverv1.RedisFailover, labels map[string]string, ownerRefs []metav1.OwnerReference) (string, error) {
	if !TLSEnabled(rf) {
		return "", nil
	}
	srcName := GetTLSSecretName(rf)
	src, err := r.K8SService.GetSecret(rf.Namespace, srcName)
	if err != nil {
		if apierrors.IsNotFound(err) {
			r.logger.WithField("namespace", rf.Namespace).WithField("secret", srcName).
				Debugf("TLS secret not present yet; deferring CA cert secret to a later reconcile")
			return "", nil
		}
		return "", err
	}
	// Taken before the ca.crt check below, so that a secret carrying a
	// serving certificate but no CA yet still pins the pods to it.
	tlsHash := tlsSecretContentHash(rf, src)
	caPEM := src.Data[tlsSecretCAKey]
	if len(caPEM) == 0 {
		r.logger.WithField("namespace", rf.Namespace).WithField("secret", srcName).
			Debugf("TLS secret has no %q yet; deferring CA cert secret to a later reconcile", tlsSecretCAKey)
		return tlsHash, nil
	}
	secret := generateRedisCACertSecret(rf, labels, ownerRefs, caPEM)
	err = r.K8SService.CreateOrUpdateSecret(rf.Namespace, secret)
	r.setEnsureOperationMetrics(secret.Namespace, secret.Name, "Secret", rf.Name, err)
	return tlsHash, err
}

// EnsureRedisService makes sure the redis statefulset exists
func (r *RedisFailoverKubeClient) EnsureRedisService(rf *redisfailoverv1.RedisFailover, labels map[string]string, ownerRefs []metav1.OwnerReference) error {
	svc := generateRedisService(rf, labels, ownerRefs)
	err := r.K8SService.CreateOrUpdateService(rf.Namespace, svc)

	r.setEnsureOperationMetrics(svc.Namespace, svc.Name, "Service", rf.Name, err)
	return err
}

// EnsureNotPresentRedisService makes sure the redis service is not present
func (r *RedisFailoverKubeClient) EnsureNotPresentRedisService(rf *redisfailoverv1.RedisFailover) error {
	name := GetRedisName(rf)
	namespace := rf.Namespace
	// If the service exists (no get error), delete it
	if _, err := r.K8SService.GetService(namespace, name); err == nil {
		return r.K8SService.DeleteService(namespace, name)
	}
	return nil
}

// EnsureRedisMasterService makes sure the redis master service exists
func (r *RedisFailoverKubeClient) EnsureRedisMasterService(rf *redisfailoverv1.RedisFailover, labels map[string]string, ownerRefs []metav1.OwnerReference) error {
	svc := generateRedisMasterService(rf, labels, ownerRefs)
	err := r.K8SService.CreateOrUpdateService(rf.Namespace, svc)

	r.setEnsureOperationMetrics(svc.Namespace, svc.Name, "Service", rf.Name, err)
	return err
}

// EnsureRedisSlaveService makes sure the redis slave service exists
func (r *RedisFailoverKubeClient) EnsureRedisSlaveService(rf *redisfailoverv1.RedisFailover, labels map[string]string, ownerRefs []metav1.OwnerReference) error {
	svc := generateRedisSlaveService(rf, labels, ownerRefs)
	err := r.K8SService.CreateOrUpdateService(rf.Namespace, svc)

	r.setEnsureOperationMetrics(svc.Namespace, svc.Name, "Service", rf.Name, err)
	return err
}

// EnsureRedisStatefulset makes sure the pdb exists in the desired state
func (r *RedisFailoverKubeClient) ensurePodDisruptionBudget(rf *redisfailoverv1.RedisFailover, name string, component string, labels map[string]string, ownerRefs []metav1.OwnerReference) error {
	name = generateName(name, rf.Name)
	namespace := rf.Namespace

	minAvailable := intstr.FromInt(2)
	if rf.Spec.Redis.Replicas <= 2 {
		minAvailable = intstr.FromInt(1)
	}

	labels = util.MergeLabels(labels, generateSelectorLabels(component, rf.Name))

	pdb := generatePodDisruptionBudget(name, namespace, labels, ownerRefs, minAvailable)
	err := r.K8SService.CreateOrUpdatePodDisruptionBudget(namespace, pdb)
	r.setEnsureOperationMetrics(pdb.Namespace, pdb.Name, "PodDisruptionBudget" /* pdb.TypeMeta.Kind isnt working;  pdb.Kind isnt working either */, rf.Name, err)
	return err
}

func (r *RedisFailoverKubeClient) setEnsureOperationMetrics(objectNamespace string, objectName string, objectKind string, ownerName string, err error) {
	if nil != err {
		r.metricsClient.RecordEnsureOperation(objectNamespace, objectName, objectKind, ownerName, metrics.FAIL)
	}
	r.metricsClient.RecordEnsureOperation(objectNamespace, objectName, objectKind, ownerName, metrics.SUCCESS)
}
