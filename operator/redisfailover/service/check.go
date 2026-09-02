package service

import (
	"crypto/tls"
	"errors"
	"fmt"
	"strconv"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"

	redisfailoverv1 "github.com/freshworks/redis-operator/api/redisfailover/v1"
	"github.com/freshworks/redis-operator/log"
	"github.com/freshworks/redis-operator/metrics"
	"github.com/freshworks/redis-operator/operator/redisfailover/util"
	"github.com/freshworks/redis-operator/service/k8s"
	"github.com/freshworks/redis-operator/service/redis"
)

// tlsConfigFor reads the cluster's TLS secret and builds the tls.Config
// the operator uses to dial Redis and Sentinel pods. Returns (nil, nil)
// when spec.tls.enabled is false, which is the signal callers feed to
// the redis client to dial plaintext. ServerName is set to the redis
// headless service name so the certificate's SAN list matches. The
// operator's own client certificate is presented whenever the Secret
// carries both tls.crt and tls.key, independently of spec.tls.authClients.
func tlsConfigFor(s k8s.Services, rf *redisfailoverv1.RedisFailover) (*tls.Config, error) {
	if !TLSEnabled(rf) {
		return nil, nil
	}
	secretName := GetTLSSecretName(rf)
	secret, err := s.GetSecret(rf.Namespace, secretName)
	if err != nil {
		return nil, fmt.Errorf("tls: reading secret %s/%s: %w", rf.Namespace, secretName, err)
	}
	caPEM, ok := secret.Data["ca.crt"]
	if !ok {
		return nil, fmt.Errorf("tls: secret %s/%s does not contain ca.crt", rf.Namespace, secretName)
	}
	if len(caPEM) == 0 {
		return nil, fmt.Errorf("tls: secret %s/%s contains an empty ca.crt", rf.Namespace, secretName)
	}
	// Redis and Sentinel reject a client that presents no certificate
	// while tls-auth-clients is yes. That directive is mutable and the
	// pods keep enforcing the value they booted with until they roll, so
	// a spec currently reading no is no evidence that the server will
	// not ask. Present the pair whenever the Secret carries one:
	// offering a certificate to a server that does not want it costs
	// nothing, withholding it from a server that does locks the operator
	// out of its own instance. A Secret without both halves yields no
	// client certificate.
	certPEM := secret.Data["tls.crt"]
	keyPEM := secret.Data["tls.key"]
	if len(certPEM) == 0 || len(keyPEM) == 0 {
		certPEM, keyPEM = nil, nil
	}
	return redis.BuildTLSConfig(caPEM, certPEM, keyPEM, GetRedisName(rf))
}

// RedisFailoverCheck defines the interface able to check the correct status of a redis failover
type RedisFailoverCheck interface {
	CheckRedisNumber(rFailover *redisfailoverv1.RedisFailover) error
	CheckSentinelNumber(rFailover *redisfailoverv1.RedisFailover) error
	CheckAllSlavesFromMaster(master string, rFailover *redisfailoverv1.RedisFailover) error
	CheckSentinelNumberInMemory(sentinel string, rFailover *redisfailoverv1.RedisFailover) error
	CheckSentinelSlavesNumberInMemory(sentinel string, rFailover *redisfailoverv1.RedisFailover) error
	CheckSentinelQuorum(rFailover *redisfailoverv1.RedisFailover) (int, error)
	CheckIfMasterLocalhost(rFailover *redisfailoverv1.RedisFailover) (bool, error)
	CheckSentinelMonitor(rFailover *redisfailoverv1.RedisFailover, sentinel string, monitor ...string) error
	GetMasterIP(rFailover *redisfailoverv1.RedisFailover) (string, error)
	GetNumberMasters(rFailover *redisfailoverv1.RedisFailover) (int, error)
	GetRedisesIPs(rFailover *redisfailoverv1.RedisFailover) ([]string, error)
	GetSentinelsIPs(rFailover *redisfailoverv1.RedisFailover) ([]string, error)
	GetMaxRedisPodTime(rFailover *redisfailoverv1.RedisFailover) (time.Duration, error)
	GetRedisesSlavesPods(rFailover *redisfailoverv1.RedisFailover) ([]string, error)
	GetRedisesMasterPod(rFailover *redisfailoverv1.RedisFailover) (string, error)
	GetStatefulSetUpdateRevision(rFailover *redisfailoverv1.RedisFailover) (string, error)
	GetRedisRevisionHash(podName string, rFailover *redisfailoverv1.RedisFailover) (string, error)
	CheckRedisSlavesReady(slaveIP string, rFailover *redisfailoverv1.RedisFailover) (bool, error)
	IsRedisRunning(rFailover *redisfailoverv1.RedisFailover) bool
	IsSentinelRunning(rFailover *redisfailoverv1.RedisFailover) bool
	IsClusterRunning(rFailover *redisfailoverv1.RedisFailover) bool
}

// RedisFailoverChecker is our implementation of RedisFailoverCheck interface
type RedisFailoverChecker struct {
	k8sService    k8s.Services
	redisClient   redis.Client
	logger        log.Logger
	metricsClient metrics.Recorder
}

// NewRedisFailoverChecker creates an object of the RedisFailoverChecker struct
func NewRedisFailoverChecker(k8sService k8s.Services, redisClient redis.Client, logger log.Logger, metricsClient metrics.Recorder) *RedisFailoverChecker {
	return &RedisFailoverChecker{
		k8sService:    k8sService,
		redisClient:   redisClient,
		logger:        logger,
		metricsClient: metricsClient,
	}
}

// CheckRedisNumber controlls that the number of deployed redis is the same than the requested on the spec
func (r *RedisFailoverChecker) CheckRedisNumber(rf *redisfailoverv1.RedisFailover) error {
	ss, err := r.k8sService.GetStatefulSet(rf.Namespace, GetRedisName(rf))
	if err != nil {
		return err
	}
	if rf.Spec.Redis.Replicas != *ss.Spec.Replicas {
		return errors.New("number of redis pods differ from specification")
	}
	return nil
}

// CheckSentinelNumber controlls that the number of deployed sentinel is the same than the requested on the spec
func (r *RedisFailoverChecker) CheckSentinelNumber(rf *redisfailoverv1.RedisFailover) error {
	d, err := r.k8sService.GetDeployment(rf.Namespace, GetSentinelName(rf))
	if err != nil {
		return err
	}
	if rf.Spec.Sentinel.Replicas != *d.Spec.Replicas {
		return errors.New("number of sentinel pods differ from specification")
	}
	return nil
}

func (r *RedisFailoverChecker) setMasterLabelIfNecessary(namespace string, pod corev1.Pod) error {
	for labelKey, labelValue := range pod.Labels {
		if labelKey == redisRoleLabelKey && labelValue == redisRoleLabelMaster {
			return nil
		}
	}
	return r.k8sService.UpdatePodLabels(namespace, pod.Name, generateRedisMasterRoleLabel())
}

func (r *RedisFailoverChecker) setSlaveLabelIfNecessary(namespace string, pod corev1.Pod) error {
	for labelKey, labelValue := range pod.Labels {
		if labelKey == redisRoleLabelKey && labelValue == redisRoleLabelSlave {
			return nil
		}
	}
	return r.k8sService.UpdatePodLabels(namespace, pod.Name, generateRedisSlaveRoleLabel())
}

func (r *RedisFailoverChecker) setMasterAnnotationIfNecessary(namespace string, pod corev1.Pod, rf *redisfailoverv1.RedisFailover) error {
	currentValue, exists := pod.ObjectMeta.Annotations[clusterAutoscalerSafeToEvictAnnotationKey]

	if !rf.Spec.Redis.PreventMasterEviction {
		// Remove annotation when preventMasterEviction is disabled
		if exists {
			return r.k8sService.RemovePodAnnotation(namespace, pod.ObjectMeta.Name, clusterAutoscalerSafeToEvictAnnotationKey)
		}
		return nil
	}

	// Add annotation when preventMasterEviction is enabled
	expectedValue := clusterAutoscalerSafeToEvictAnnotationMaster
	if !exists || currentValue != expectedValue {
		return r.k8sService.UpdatePodAnnotations(namespace, pod.ObjectMeta.Name, generateRedisMasterAnnotations())
	}

	return nil
}

func (r *RedisFailoverChecker) setSlaveAnnotationIfNecessary(namespace string, pod corev1.Pod, rf *redisfailoverv1.RedisFailover) error {
	currentValue, exists := pod.ObjectMeta.Annotations[clusterAutoscalerSafeToEvictAnnotationKey]

	if !rf.Spec.Redis.PreventMasterEviction {
		// Remove annotation when preventMasterEviction is disabled
		if exists {
			return r.k8sService.RemovePodAnnotation(namespace, pod.ObjectMeta.Name, clusterAutoscalerSafeToEvictAnnotationKey)
		}
		return nil
	}

	// Add annotation when preventMasterEviction is enabled
	expectedValue := clusterAutoscalerSafeToEvictAnnotationSlave
	if !exists || currentValue != expectedValue {
		return r.k8sService.UpdatePodAnnotations(namespace, pod.ObjectMeta.Name, generateRedisSlaveAnnotations())
	}

	return nil
}

// CheckAllSlavesFromMaster controlls that all slaves have the same master (the real one)
func (r *RedisFailoverChecker) CheckAllSlavesFromMaster(master string, rf *redisfailoverv1.RedisFailover) error {
	rps, err := r.k8sService.GetStatefulSetPods(rf.Namespace, GetRedisName(rf))
	if err != nil {
		return err
	}

	password, err := k8s.GetRedisPassword(r.k8sService, rf)
	if err != nil {
		return err
	}

	tlsConfig, err := tlsConfigFor(r.k8sService, rf)
	if err != nil {
		return err
	}

	rport := getRedisPort(rf.Spec.Redis.Port)
	for _, rp := range rps.Items {
		if rp.Status.PodIP == master {
			err = r.setMasterLabelIfNecessary(rf.Namespace, rp)
			if err != nil {
				return err
			}
			err = r.setMasterAnnotationIfNecessary(rf.Namespace, rp, rf)
			if err != nil {
				return err
			}
		} else {
			err = r.setSlaveLabelIfNecessary(rf.Namespace, rp)
			if err != nil {
				return err
			}
			err = r.setSlaveAnnotationIfNecessary(rf.Namespace, rp, rf)
			if err != nil {
				return err
			}
		}

		slave, err := r.redisClient.GetSlaveOf(rp.Status.PodIP, rport, password, tlsConfig)
		if err != nil {
			r.logger.Errorf("Get slave of master failed, maybe this node is not ready, pod ip: %s", rp.Status.PodIP)
			return err
		}
		if slave != "" && slave != master {
			return fmt.Errorf("slave %s don't have the master %s, has %s", rp.Status.PodIP, master, slave)
		}
	}
	return nil
}

// CheckSentinelNumberInMemory controls that the provided sentinel has only the living sentinels on its memory.
func (r *RedisFailoverChecker) CheckSentinelNumberInMemory(sentinel string, rf *redisfailoverv1.RedisFailover) error {
	tlsConfig, err := tlsConfigFor(r.k8sService, rf)
	if err != nil {
		return err
	}
	nSentinels, err := r.redisClient.GetNumberSentinelsInMemory(sentinel, tlsConfig)
	if err != nil {
		return err
	} else if nSentinels != rf.Spec.Sentinel.Replicas {
		return errors.New("sentinels in memory mismatch")
	}
	return nil
}

// This function will check if the local host ip is set as the master for all currently available pods
// This  can be used to detect the fresh boot of all the redis pods
// This function returns true if it all available pods have local host ip as master,
// false if atleast one of the ip is not local hostip
// false and error if any function fails
func (r *RedisFailoverChecker) CheckIfMasterLocalhost(rFailover *redisfailoverv1.RedisFailover) (bool, error) {

	var lhmaster = 0
	redisIps, err := r.GetRedisesIPs(rFailover)
	if len(redisIps) == 0 || err != nil {
		r.logger.Warningf("CheckIfMasterLocalhost GetRedisesIPs Failed- unable to fetch any redis Ips Currently")
		return false, errors.New("unable to fetch any redis Ips Currently")
	}
	password, err := k8s.GetRedisPassword(r.k8sService, rFailover)
	if err != nil {
		r.logger.Errorf("CheckIfMasterLocalhost -- GetRedisPassword Failed")
		return false, err
	}
	tlsConfig, err := tlsConfigFor(r.k8sService, rFailover)
	if err != nil {
		return false, err
	}
	rport := getRedisPort(rFailover.Spec.Redis.Port)
	for _, sip := range redisIps {
		master, err := r.redisClient.GetSlaveOf(sip, rport, password, tlsConfig)
		if err != nil {
			r.logger.Warningf("CheckIfMasterLocalhost -- GetSlaveOf Failed")
			return false, err
		} else if master == "" {
			r.logger.Warningf("CheckIfMasterLocalhost -- Master already available ?? check manually")
			return false, errors.New("unexpected master state, fix manually")
		} else {
			if master == "127.0.0.1" {
				lhmaster++
			}
		}
	}
	if lhmaster == len(redisIps) {
		r.logger.Infof("all available redis configured localhost as master , operator must heal")
		return true, nil
	}
	r.logger.Infof("atleast one pod does not have localhost as master , operator should not heal")
	return false, nil
}

// This function will call the sentinel client apis to check with sentinel if the sentinel is in a state
// to heal the redis system
func (r *RedisFailoverChecker) CheckSentinelQuorum(rFailover *redisfailoverv1.RedisFailover) (int, error) {

	var unhealthyCnt = -1

	sentinels, err := r.GetSentinelsIPs(rFailover)
	if err != nil {
		r.logger.Warningf("CheckSentinelQuorum Error in getting sentinel Ip's")
		return unhealthyCnt, err
	}
	if len(sentinels) < int(getQuorum(rFailover)) {
		unhealthyCnt = int(getQuorum(rFailover)) - len(sentinels)
		r.logger.Warningf("insufficnet sentinel to reach Quorum - Unhealthy count: %d", unhealthyCnt)
		return unhealthyCnt, errors.New("insufficnet sentinel to reach Quorum")
	}

	tlsConfig, err := tlsConfigFor(r.k8sService, rFailover)
	if err != nil {
		return unhealthyCnt, err
	}
	unhealthyCnt = 0
	for _, sip := range sentinels {
		err = r.redisClient.SentinelCheckQuorum(sip, rFailover.MasterName(), tlsConfig)
		if err != nil {
			unhealthyCnt += 1
		} else {
			continue
		}
	}
	if unhealthyCnt < int(getQuorum(rFailover)) {
		return unhealthyCnt, nil
	} else {
		r.logger.Errorf("insufficnet sentinel to reach Quorum - Unhealthy count: %d", unhealthyCnt)
		return unhealthyCnt, errors.New("insufficnet sentinel to reach Quorum")
	}
}

// CheckSentinelSlavesNumberInMemory controls that the provided sentinel has only the expected slaves number.
func (r *RedisFailoverChecker) CheckSentinelSlavesNumberInMemory(sentinel string, rf *redisfailoverv1.RedisFailover) error {
	tlsConfig, err := tlsConfigFor(r.k8sService, rf)
	if err != nil {
		return err
	}
	nSlaves, err := r.redisClient.GetNumberSentinelSlavesInMemory(sentinel, tlsConfig)
	if err != nil {
		return err
	} else {
		if rf.Bootstrapping() {
			if nSlaves != rf.Spec.Redis.Replicas {
				return errors.New("redis slaves in sentinel memory mismatch")
			}
		} else {
			if nSlaves != rf.Spec.Redis.Replicas-1 {
				return errors.New("redis slaves in sentinel memory mismatch")
			}
		}
	}
	return nil

}

// CheckSentinelMonitor controls if the sentinels are monitoring the expected master
func (r *RedisFailoverChecker) CheckSentinelMonitor(rf *redisfailoverv1.RedisFailover, sentinel string, monitor ...string) error {
	monitorIP := monitor[0]
	monitorPort := ""
	if len(monitor) > 1 {
		monitorPort = monitor[1]
	}
	tlsConfig, err := tlsConfigFor(r.k8sService, rf)
	if err != nil {
		return err
	}
	actualMonitorIP, actualMonitorPort, err := r.redisClient.GetSentinelMonitor(sentinel, rf.MasterName(), tlsConfig)
	if err != nil {
		return err
	}
	if actualMonitorIP != monitorIP || (monitorPort != "" && monitorPort != actualMonitorPort) {
		return fmt.Errorf("sentinel monitoring %s:%s instead %s:%s", actualMonitorIP, actualMonitorPort, monitorIP, monitorPort)
	}
	return nil
}

// GetMasterIP connects to all redis and returns the master of the redis failover
func (r *RedisFailoverChecker) GetMasterIP(rf *redisfailoverv1.RedisFailover) (string, error) {
	rips, err := r.GetRedisesIPs(rf)
	if err != nil {
		return "", err
	}

	password, err := k8s.GetRedisPassword(r.k8sService, rf)
	if err != nil {
		return "", err
	}

	tlsConfig, err := tlsConfigFor(r.k8sService, rf)
	if err != nil {
		return "", err
	}

	masters := []string{}
	rport := getRedisPort(rf.Spec.Redis.Port)
	for _, rip := range rips {
		master, err := r.redisClient.IsMaster(rip, rport, password, tlsConfig)
		if err != nil {
			r.logger.Errorf("Get redis info failed, maybe this node is not ready, pod ip: %s", rip)
			continue
		}
		if master {
			masters = append(masters, rip)
		}
	}

	if len(masters) != 1 {
		return "", errors.New("number of redis nodes known as master is different than 1")
	}
	return masters[0], nil
}

// GetNumberMasters returns the number of redis nodes that are working as a master
func (r *RedisFailoverChecker) GetNumberMasters(rf *redisfailoverv1.RedisFailover) (int, error) {
	nMasters := 0
	rips, err := r.GetRedisesIPs(rf)
	if err != nil {
		r.logger.Errorf("%v", err)
		return nMasters, err
	}

	password, err := k8s.GetRedisPassword(r.k8sService, rf)
	if err != nil {
		r.logger.Errorf("Error getting password: %s", err.Error())
		return nMasters, err
	}

	tlsConfig, err := tlsConfigFor(r.k8sService, rf)
	if err != nil {
		return nMasters, err
	}

	rport := getRedisPort(rf.Spec.Redis.Port)
	for _, rip := range rips {
		master, err := r.redisClient.IsMaster(rip, rport, password, tlsConfig)
		if err != nil {
			r.logger.Errorf("Get redis info failed, maybe this node is not ready, pod ip: %s", rip)
			continue
		}
		if master {
			nMasters++
		}
	}
	return nMasters, nil
}

// GetRedisesIPs returns the IPs of the Redis nodes
func (r *RedisFailoverChecker) GetRedisesIPs(rf *redisfailoverv1.RedisFailover) ([]string, error) {
	redises := []string{}
	rps, err := r.k8sService.GetStatefulSetPods(rf.Namespace, GetRedisName(rf))
	if err != nil {
		return nil, err
	}
	for _, rp := range rps.Items {
		if rp.Status.Phase == corev1.PodRunning && rp.DeletionTimestamp == nil { // Only work with running pods
			redises = append(redises, rp.Status.PodIP)
		}
	}
	return redises, nil
}

// GetSentinelsIPs returns the IPs of the Sentinel nodes
func (r *RedisFailoverChecker) GetSentinelsIPs(rf *redisfailoverv1.RedisFailover) ([]string, error) {
	sentinels := []string{}
	rps, err := r.k8sService.GetDeploymentPods(rf.Namespace, GetSentinelName(rf))
	if err != nil {
		return nil, err
	}
	for _, sp := range rps.Items {
		if sp.Status.Phase == corev1.PodRunning && sp.DeletionTimestamp == nil { // Only work with running pods
			sentinels = append(sentinels, sp.Status.PodIP)
		}
	}
	return sentinels, nil
}

// GetMaxRedisPodTime returns the MAX uptime among the active Pods
func (r *RedisFailoverChecker) GetMaxRedisPodTime(rf *redisfailoverv1.RedisFailover) (time.Duration, error) {
	maxTime := 0 * time.Hour
	rps, err := r.k8sService.GetStatefulSetPods(rf.Namespace, GetRedisName(rf))
	if err != nil {
		return maxTime, err
	}
	for _, redisNode := range rps.Items {
		if redisNode.Status.StartTime == nil {
			continue
		}
		start := redisNode.Status.StartTime.Round(time.Second)
		alive := time.Since(start)
		r.logger.Debugf("Pod %s has been alive for %.f seconds", redisNode.Status.PodIP, alive.Seconds())
		if alive > maxTime {
			maxTime = alive
		}
	}
	return maxTime, nil
}

// GetRedisesSlavesPods returns pods names of the Redis slave nodes
func (r *RedisFailoverChecker) GetRedisesSlavesPods(rf *redisfailoverv1.RedisFailover) ([]string, error) {
	redises := []string{}
	rps, err := r.k8sService.GetStatefulSetPods(rf.Namespace, GetRedisName(rf))
	if err != nil {
		return nil, err
	}

	password, err := k8s.GetRedisPassword(r.k8sService, rf)
	if err != nil {
		return redises, err
	}

	tlsConfig, err := tlsConfigFor(r.k8sService, rf)
	if err != nil {
		return redises, err
	}

	rport := getRedisPort(rf.Spec.Redis.Port)
	for _, rp := range rps.Items {
		if rp.Status.Phase == corev1.PodRunning && rp.DeletionTimestamp == nil { // Only work with running
			master, err := r.redisClient.IsMaster(rp.Status.PodIP, rport, password, tlsConfig)
			if err != nil {
				return []string{}, err
			}
			if !master {
				redises = append(redises, rp.ObjectMeta.Name)
			}
		}
	}
	return redises, nil
}

// GetRedisesMasterPod returns pods names of the Redis slave nodes
func (r *RedisFailoverChecker) GetRedisesMasterPod(rFailover *redisfailoverv1.RedisFailover) (string, error) {
	rps, err := r.k8sService.GetStatefulSetPods(rFailover.Namespace, GetRedisName(rFailover))
	if err != nil {
		return "", err
	}

	password, err := k8s.GetRedisPassword(r.k8sService, rFailover)
	if err != nil {
		return "", err
	}

	tlsConfig, err := tlsConfigFor(r.k8sService, rFailover)
	if err != nil {
		return "", err
	}

	rport := getRedisPort(rFailover.Spec.Redis.Port)
	for _, rp := range rps.Items {
		if rp.Status.Phase == corev1.PodRunning && rp.DeletionTimestamp == nil { // Only work with running
			master, err := r.redisClient.IsMaster(rp.Status.PodIP, rport, password, tlsConfig)
			if err != nil {
				return "", err
			}
			if master {
				return rp.ObjectMeta.Name, nil
			}
		}
	}
	return "", errors.New("redis nodes known as master not found")
}

// GetStatefulSetUpdateRevision returns current version for the statefulSet
// If the label don't exists, we return an empty value and no error, so previous versions don't break
func (r *RedisFailoverChecker) GetStatefulSetUpdateRevision(rFailover *redisfailoverv1.RedisFailover) (string, error) {
	ss, err := r.k8sService.GetStatefulSet(rFailover.Namespace, GetRedisName(rFailover))
	if err != nil {
		return "", err
	}

	if ss == nil {
		return "", errors.New("statefulSet not found")
	}

	return ss.Status.UpdateRevision, nil
}

// GetRedisRevisionHash returns the statefulset uid for the pod
func (r *RedisFailoverChecker) GetRedisRevisionHash(podName string, rFailover *redisfailoverv1.RedisFailover) (string, error) {
	pod, err := r.k8sService.GetPod(rFailover.Namespace, podName)
	if err != nil {
		return "", err
	}

	if pod == nil {
		return "", errors.New("pod not found")
	}

	if pod.ObjectMeta.Labels == nil {
		return "", errors.New("labels not found")
	}

	val := pod.ObjectMeta.Labels[appsv1.ControllerRevisionHashLabelKey]

	return val, nil
}

// CheckRedisSlavesReady returns true if the slave is ready (sync, connected, etc)
func (r *RedisFailoverChecker) CheckRedisSlavesReady(ip string, rFailover *redisfailoverv1.RedisFailover) (bool, error) {
	password, err := k8s.GetRedisPassword(r.k8sService, rFailover)
	if err != nil {
		return false, err
	}

	tlsConfig, err := tlsConfigFor(r.k8sService, rFailover)
	if err != nil {
		return false, err
	}

	port := getRedisPort(rFailover.Spec.Redis.Port)
	return r.redisClient.SlaveIsReady(ip, port, password, tlsConfig)
}

// IsRedisRunning returns true if all the pods are Running
func (r *RedisFailoverChecker) IsRedisRunning(rFailover *redisfailoverv1.RedisFailover) bool {
	dp, err := r.k8sService.GetStatefulSetPods(rFailover.Namespace, GetRedisName(rFailover))
	return err == nil && len(dp.Items) > int(rFailover.Spec.Redis.Replicas-1) && AreAllRunning(dp, int(rFailover.Spec.Redis.Replicas))
}

// IsSentinelRunning returns true if all the pods are Running
func (r *RedisFailoverChecker) IsSentinelRunning(rFailover *redisfailoverv1.RedisFailover) bool {
	dp, err := r.k8sService.GetDeploymentPods(rFailover.Namespace, GetSentinelName(rFailover))
	return err == nil && len(dp.Items) > int(rFailover.Spec.Sentinel.Replicas-1) && AreAllRunning(dp, int(rFailover.Spec.Sentinel.Replicas))
}

// IsClusterRunning returns true if all the pods in the given redisfailover are Running
func (r *RedisFailoverChecker) IsClusterRunning(rFailover *redisfailoverv1.RedisFailover) bool {
	return r.IsSentinelRunning(rFailover) && r.IsRedisRunning(rFailover)
}

func getRedisPort(p int32) string {
	return strconv.Itoa(int(p))
}

func AreAllRunning(pods *corev1.PodList, expectedRunningPods int) bool {
	var runningPods int
	for _, pod := range pods.Items {
		if util.PodIsScheduling(&pod) {
			return false
		}
		if util.PodIsTerminal(&pod) {
			continue
		}
		runningPods++
	}
	return runningPods >= expectedRunningPods
}
