package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"

	redisfailoverv1 "github.com/freshworks/redis-operator/api/redisfailover/v1"
	"github.com/freshworks/redis-operator/log"
	"github.com/freshworks/redis-operator/metrics"
	"github.com/freshworks/redis-operator/operator/redisfailover/util"
	"github.com/freshworks/redis-operator/service/k8s"
	"github.com/freshworks/redis-operator/service/redis"
)

// RedisFailoverCheck defines the interface able to check the correct status of a redis failover
type RedisFailoverCheck interface {
	CheckRedisNumber(rFailover *redisfailoverv1.RedisFailover) error
	CheckSentinelNumber(rFailover *redisfailoverv1.RedisFailover) error
	CheckAllSlavesFromMaster(master string, rFailover *redisfailoverv1.RedisFailover) error
	CheckSentinelNumberInMemory(sentinel string, rFailover *redisfailoverv1.RedisFailover) error
	CheckSentinelSlavesNumberInMemory(sentinel string, rFailover *redisfailoverv1.RedisFailover) error
	CheckSentinelQuorum(rFailover *redisfailoverv1.RedisFailover) (int, error)
	CheckIfMasterLocalhost(rFailover *redisfailoverv1.RedisFailover) (bool, error)
	CheckSentinelMonitor(sentinel, masterName string, monitor ...string) error
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
	// IsPodResourceOnlyChange reports whether podRevision - the revision a pod is currently on,
	// per its controller-revision-hash label - differs from the StatefulSet's current template
	// only in "redis" container resources, by diffing the ControllerRevision snapshot for that
	// revision against the current template. Computed fresh per pod (not cached at the
	// StatefulSet level) because pods are caught up one at a time across reconciles: once the
	// StatefulSet stops changing, a stale cached signal would trivially read as resource-only
	// for every remaining pod, regardless of what that pod's own pending diff actually contains.
	IsPodResourceOnlyChange(podRevision string, rFailover *redisfailoverv1.RedisFailover) (bool, error)
	// GetPodResizeCondition reports whether podName currently has a PodResizePending or
	// PodResizeInProgress condition and, if so, which type and reason (e.g.
	// corev1.PodReasonDeferred, corev1.PodReasonInfeasible, or corev1.PodReasonError).
	GetPodResizeCondition(podName string, rFailover *redisfailoverv1.RedisFailover) (found bool, condType corev1.PodConditionType, reason string, err error)
	// PodResourcesMatchDesired reports whether podName's redis container currently has the
	// same resources as rFailover.Spec.Redis.Resources. The absence of a PodResizePending
	// condition alone doesn't prove a resize actually applied - it's also true when the
	// resize call never reached the pod at all (e.g. an RBAC rejection) - so callers must
	// check this before treating a resize as successful.
	PodResourcesMatchDesired(podName string, rFailover *redisfailoverv1.RedisFailover) (bool, error)
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

		slave, err := r.redisClient.GetSlaveOf(rp.Status.PodIP, rport, password)
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
	nSentinels, err := r.redisClient.GetNumberSentinelsInMemory(sentinel)
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
	rport := getRedisPort(rFailover.Spec.Redis.Port)
	for _, sip := range redisIps {
		master, err := r.redisClient.GetSlaveOf(sip, rport, password)
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

	unhealthyCnt = 0
	for _, sip := range sentinels {
		err = r.redisClient.SentinelCheckQuorum(sip, rFailover.MasterName())
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
	nSlaves, err := r.redisClient.GetNumberSentinelSlavesInMemory(sentinel)
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
func (r *RedisFailoverChecker) CheckSentinelMonitor(sentinel, masterName string, monitor ...string) error {
	monitorIP := monitor[0]
	monitorPort := ""
	if len(monitor) > 1 {
		monitorPort = monitor[1]
	}
	actualMonitorIP, actualMonitorPort, err := r.redisClient.GetSentinelMonitor(sentinel, masterName)
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

	masters := []string{}
	rport := getRedisPort(rf.Spec.Redis.Port)
	for _, rip := range rips {
		master, err := r.redisClient.IsMaster(rip, rport, password)
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

	rport := getRedisPort(rf.Spec.Redis.Port)
	for _, rip := range rips {
		master, err := r.redisClient.IsMaster(rip, rport, password)
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

	rport := getRedisPort(rf.Spec.Redis.Port)
	for _, rp := range rps.Items {
		if rp.Status.Phase == corev1.PodRunning && rp.DeletionTimestamp == nil { // Only work with running
			master, err := r.redisClient.IsMaster(rp.Status.PodIP, rport, password)
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

	rport := getRedisPort(rFailover.Spec.Redis.Port)
	for _, rp := range rps.Items {
		if rp.Status.Phase == corev1.PodRunning && rp.DeletionTimestamp == nil { // Only work with running
			master, err := r.redisClient.IsMaster(rp.Status.PodIP, rport, password)
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

// IsPodResourceOnlyChange reports whether podRevision - the revision a pod is currently on, per
// its controller-revision-hash label (see GetRedisRevisionHash) - differs from the StatefulSet's
// current template only in "redis" container resources.
//
// Deliberately does not compare the live pod's own spec: a running pod is not a pure
// instantiation of its template - the scheduler, the ServiceAccount admission controller,
// DefaultTolerationSeconds, and IRSA-style webhooks (e.g. EKS's pod identity webhook) all add or
// set fields on every pod that never exist on any template (NodeName, extra Volumes/Env/
// Tolerations, EnableServiceLinks, Priority, ...). Comparing that against a template would see
// those as differences on every single pod, forever, regardless of what actually changed.
//
// Instead, this fetches the ControllerRevision named podRevision - the exact historical template
// (metadata + PodSpec) that revision's pods were created from, kept by the StatefulSet
// controller for exactly this kind of lookup (it's how `kubectl rollout history` works) - and
// diffs that against the StatefulSet's current template. Both sides are then the same kind of
// object (a pure template, never touched by pod-creation-time admission), so no noise tolerance
// of any kind is needed - a real StatefulSet-level diff, not a pod-vs-template approximation.
//
// Distinguishes two different kinds of "can't tell": a revision that's been pruned beyond
// RevisionHistoryLimit, or a ControllerRevision whose Data doesn't decode into a usable
// template, is *permanently* unknowable - retrying changes nothing, so these fall back to
// (false, nil), routing the caller to the pre-existing, dependency-free DeletePod path exactly
// as if this feature didn't exist. A transient failure on either Get call (a timeout, rate
// limiting, a dropped connection) is a different thing entirely: it's very likely to resolve on
// its own, and the pod itself did nothing wrong - forcing a disruptive delete (a Sentinel
// failover, if this is the master) over a blip that had nothing to do with the pod's actual
// state would be a bad trade for something that would probably have succeeded on the very next
// reconcile. So a transient error is returned as a real error instead, and it's on the caller
// (UpdateRedisesPods) to skip this pod for the current reconcile - no delete, no resize - and
// let the normal resync loop try again shortly.
func (r *RedisFailoverChecker) IsPodResourceOnlyChange(podRevision string, rFailover *redisfailoverv1.RedisFailover) (bool, error) {
	revision, err := r.k8sService.GetControllerRevision(rFailover.Namespace, podRevision)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			// Already pruned - there will never be anything to compare against, no matter how
			// many times this is retried.
			return false, nil
		}
		return false, fmt.Errorf("fetching controllerrevision %s: %w", podRevision, err)
	}
	oldTemplate, err := decodeStatefulSetRevisionTemplate(revision)
	if err != nil {
		// Malformed data on an immutable object - not transient, retrying returns the exact
		// same bytes every time.
		r.logger.WithField("redisfailover", rFailover.ObjectMeta.Name).WithField("revision", podRevision).Warningf("could not decode controllerrevision, falling back to delete: %v", err)
		return false, nil
	}
	if len(oldTemplate.Spec.Containers) == 0 {
		// Decoding produced an empty template - the ControllerRevision's Data didn't have the
		// expected shape. Don't guess either way.
		return false, nil
	}

	ss, err := r.k8sService.GetStatefulSet(rFailover.Namespace, GetRedisName(rFailover))
	if err != nil {
		return false, fmt.Errorf("fetching statefulset %s: %w", GetRedisName(rFailover), err)
	}
	if ss == nil {
		return false, nil
	}

	return k8s.IsResourceOnlyChange(&oldTemplate.Spec, &ss.Spec.Template.Spec, "redis"), nil
}

// statefulSetRevisionData mirrors the shape the StatefulSet controller encodes into a
// ControllerRevision's Data field: a strategic-merge "replace" patch carrying the full template
// verbatim (not a diff against a prior revision). The "$patch" key is ignored on decode - Go's
// json.Unmarshal skips struct fields with no matching tag.
type statefulSetRevisionData struct {
	Spec struct {
		Template corev1.PodTemplateSpec `json:"template"`
	} `json:"spec"`
}

func decodeStatefulSetRevisionTemplate(revision *appsv1.ControllerRevision) (*corev1.PodTemplateSpec, error) {
	var data statefulSetRevisionData
	if err := json.Unmarshal(revision.Data.Raw, &data); err != nil {
		return nil, fmt.Errorf("decoding controllerrevision %s: %w", revision.Name, err)
	}
	return &data.Spec.Template, nil
}

// GetPodResizeCondition reports whether podName currently has a PodResizePending or
// PodResizeInProgress condition and, if so, which type and reason.
//
// Checking only PodResizePending is not enough to tell a finished resize apart from one the
// kubelet is still actively applying: ResizePod writes the desired spec into pod.Spec
// synchronously, but PodResizePending only ever appears when the kubelet can't proceed
// immediately (Deferred/Infeasible) - once it *can* proceed, PodResizePending never appears at
// all, and the kubelet instead sets PodResizeInProgress for the entire time it's allocating and
// actuating the change (which can be non-trivial, e.g. a container restart under
// RestartContainer policy). Treating "no PodResizePending" as sufficient proof of success would
// let a pod get relabeled as fully resized while the kubelet is still in the middle of it.
// PodResizePending is checked first: per its own docs, if both conditions are present it means
// a new resize was requested mid-actuation of a previous one, which should be handled the same
// way any other Deferred/Infeasible condition is.
func (r *RedisFailoverChecker) GetPodResizeCondition(podName string, rFailover *redisfailoverv1.RedisFailover) (bool, corev1.PodConditionType, string, error) {
	pod, err := r.k8sService.GetPod(rFailover.Namespace, podName)
	if err != nil {
		return false, "", "", err
	}
	if pod == nil {
		return false, "", "", errors.New("pod not found")
	}
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodResizePending && cond.Status == corev1.ConditionTrue {
			return true, corev1.PodResizePending, cond.Reason, nil
		}
	}
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodResizeInProgress && cond.Status == corev1.ConditionTrue {
			return true, corev1.PodResizeInProgress, cond.Reason, nil
		}
	}
	return false, "", "", nil
}

// PodResourcesMatchDesired reports whether podName's redis container currently has the same
// resources as rFailover.Spec.Redis.Resources.
//
// Compares only the resource names present in the CR spec, not the whole ResourceRequirements
// struct - a namespace LimitRange can inject additional defaults (e.g. ephemeral-storage) into
// the live pod's Requests/Limits that never appear in the CR at all. A whole-struct equality
// check would never match in that case, causing ResizePod to be re-issued every reconcile
// until the timeout forces a DeletePod fallback - the feature would silently never succeed for
// any tenant under such a LimitRange. Extra keys the live pod has beyond what the CR asked for
// are simply ignored.
func (r *RedisFailoverChecker) PodResourcesMatchDesired(podName string, rFailover *redisfailoverv1.RedisFailover) (bool, error) {
	pod, err := r.k8sService.GetPod(rFailover.Namespace, podName)
	if err != nil {
		return false, err
	}
	if pod == nil {
		return false, errors.New("pod not found")
	}
	for _, container := range pod.Spec.Containers {
		if container.Name == "redis" {
			desired := rFailover.Spec.Redis.Resources
			return resourceListMatches(container.Resources.Requests, desired.Requests) &&
				resourceListMatches(container.Resources.Limits, desired.Limits), nil
		}
	}
	return false, errors.New("redis container not found in pod")
}

// resourceListMatches reports whether actual has the same quantity as desired for every
// resource name present in desired. A name in desired but absent from actual is a mismatch
// (resize hasn't applied); a name present in actual but not in desired (e.g. LimitRange
// defaulting an unrelated resource like ephemeral-storage) is ignored.
//
// cpu and memory are the exception: they're the only two resource types KEP-1287 resize can
// actually manage, so a value the CR used to ask for and has since removed (e.g. a dropped
// memory limit) must not be silently treated as already matching just because desired no
// longer mentions it - actual still carries the stale value until a real resize (or delete)
// propagates the removal. Every other resource name keeps the tolerant, desired-only check.
func resourceListMatches(actual, desired corev1.ResourceList) bool {
	for name, desiredQty := range desired {
		actualQty, ok := actual[name]
		if !ok || actualQty.Cmp(desiredQty) != 0 {
			return false
		}
	}
	for _, name := range [...]corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory} {
		if _, inActual := actual[name]; inActual {
			if _, inDesired := desired[name]; !inDesired {
				return false
			}
		}
	}
	return true
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

	port := getRedisPort(rFailover.Spec.Redis.Port)
	return r.redisClient.SlaveIsReady(ip, port, password)
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
