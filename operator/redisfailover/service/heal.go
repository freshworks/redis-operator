package service

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	redisfailoverv1 "github.com/freshworks/redis-operator/api/redisfailover/v1"
	"github.com/freshworks/redis-operator/log"
	"github.com/freshworks/redis-operator/service/k8s"
	"github.com/freshworks/redis-operator/service/redis"
	v1 "k8s.io/api/core/v1"
)

// RedisFailoverHeal defines the interface able to fix the problems on the redis failovers
type RedisFailoverHeal interface {
	MakeMaster(ip string, rFailover *redisfailoverv1.RedisFailover) error
	SetOldestAsMaster(rFailover *redisfailoverv1.RedisFailover) error
	SetMasterOnAll(masterIP string, rFailover *redisfailoverv1.RedisFailover) error
	SetExternalMasterOnAll(masterIP string, masterPort string, rFailover *redisfailoverv1.RedisFailover) error
	NewSentinelMonitor(ip string, monitor string, rFailover *redisfailoverv1.RedisFailover) error
	NewSentinelMonitorWithPort(ip string, monitor string, port string, rFailover *redisfailoverv1.RedisFailover) error
	RestoreSentinel(ip string, rFailover *redisfailoverv1.RedisFailover) error
	SetSentinelCustomConfig(ip string, rFailover *redisfailoverv1.RedisFailover) error
	SetRedisCustomConfig(ip string, rFailover *redisfailoverv1.RedisFailover) error
	DeletePod(podName string, rFailover *redisfailoverv1.RedisFailover) error
}

// RedisFailoverHealer is our implementation of RedisFailoverCheck interface
type RedisFailoverHealer struct {
	k8sService  k8s.Services
	redisClient redis.Client
	logger      log.Logger
}

// NewRedisFailoverHealer creates an object of the RedisFailoverChecker struct
func NewRedisFailoverHealer(k8sService k8s.Services, redisClient redis.Client, logger log.Logger) *RedisFailoverHealer {
	logger = logger.With("service", "redis.healer")
	return &RedisFailoverHealer{
		k8sService:  k8sService,
		redisClient: redisClient,
		logger:      logger,
	}
}

func (r *RedisFailoverHealer) setMasterLabelIfNecessary(namespace string, pod v1.Pod) error {
	for labelKey, labelValue := range pod.ObjectMeta.Labels {
		if labelKey == redisRoleLabelKey && labelValue == redisRoleLabelMaster {
			return nil
		}
	}
	return r.k8sService.UpdatePodLabels(namespace, pod.ObjectMeta.Name, generateRedisMasterRoleLabel())
}

func (r *RedisFailoverHealer) setSlaveLabelIfNecessary(namespace string, pod v1.Pod) error {
	for labelKey, labelValue := range pod.ObjectMeta.Labels {
		if labelKey == redisRoleLabelKey && labelValue == redisRoleLabelSlave {
			return nil
		}
	}
	return r.k8sService.UpdatePodLabels(namespace, pod.ObjectMeta.Name, generateRedisSlaveRoleLabel())
}

func (r *RedisFailoverHealer) setMasterAnnotationIfNecessary(namespace string, pod v1.Pod, rf *redisfailoverv1.RedisFailover) error {
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

func (r *RedisFailoverHealer) setSlaveAnnotationIfNecessary(namespace string, pod v1.Pod, rf *redisfailoverv1.RedisFailover) error {
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

func (r *RedisFailoverHealer) MakeMaster(ip string, rf *redisfailoverv1.RedisFailover) error {
	password, err := k8s.GetRedisPassword(r.k8sService, rf)
	if err != nil {
		return err
	}

	tlsConfig, err := tlsConfigFor(r.k8sService, rf)
	if err != nil {
		return err
	}

	port := getRedisPort(rf.Spec.Redis.Port)
	err = r.redisClient.MakeMaster(ip, port, password, tlsConfig)
	if err != nil {
		return err
	}

	rps, err := r.k8sService.GetStatefulSetPods(rf.Namespace, GetRedisName(rf))
	if err != nil {
		return err
	}
	for _, rp := range rps.Items {
		if rp.Status.PodIP == ip {
			err = r.setMasterLabelIfNecessary(rf.Namespace, rp)
			if err != nil {
				return err
			}
			err = r.setMasterAnnotationIfNecessary(rf.Namespace, rp, rf)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// SetOldestAsMaster puts all redis to the same master, choosen by order of appearance
func (r *RedisFailoverHealer) SetOldestAsMaster(rf *redisfailoverv1.RedisFailover) error {
	ssp, err := r.k8sService.GetStatefulSetPods(rf.Namespace, GetRedisName(rf))
	if err != nil {
		return err
	}
	if len(ssp.Items) < 1 {
		return errors.New("number of redis pods are 0")
	}

	// Order the pods so we start by the oldest one
	sort.Slice(ssp.Items, func(i, j int) bool {
		return ssp.Items[i].CreationTimestamp.Before(&ssp.Items[j].CreationTimestamp)
	})

	password, err := k8s.GetRedisPassword(r.k8sService, rf)
	if err != nil {
		return err
	}

	tlsConfig, err := tlsConfigFor(r.k8sService, rf)
	if err != nil {
		return err
	}

	port := getRedisPort(rf.Spec.Redis.Port)
	newMasterIP := ""
	for _, pod := range ssp.Items {
		if newMasterIP == "" {
			newMasterIP = pod.Status.PodIP
			r.logger.WithField("redisfailover", rf.ObjectMeta.Name).WithField("namespace", rf.ObjectMeta.Namespace).Infof("New master is %s with ip %s", pod.Name, newMasterIP)
			if err := r.redisClient.MakeMaster(newMasterIP, port, password, tlsConfig); err != nil {
				newMasterIP = ""
				r.logger.WithField("redisfailover", rf.ObjectMeta.Name).WithField("namespace", rf.ObjectMeta.Namespace).Errorf("Make new master failed, master ip: %s, error: %v", pod.Status.PodIP, err)
				continue
			}

			err = r.setMasterLabelIfNecessary(rf.Namespace, pod)
			if err != nil {
				return err
			}
			err = r.setMasterAnnotationIfNecessary(rf.Namespace, pod, rf)
			if err != nil {
				return err
			}

			newMasterIP = pod.Status.PodIP
		} else {
			r.logger.Infof("Making pod %s slave of %s", pod.Name, newMasterIP)
			if err := r.redisClient.MakeSlaveOfWithPort(pod.Status.PodIP, port, newMasterIP, port, password, tlsConfig); err != nil {
				r.logger.WithField("redisfailover", rf.ObjectMeta.Name).WithField("namespace", rf.ObjectMeta.Namespace).Errorf("Make slave failed, slave pod ip: %s, master ip: %s, error: %v", pod.Status.PodIP, newMasterIP, err)
			}

			err = r.setSlaveLabelIfNecessary(rf.Namespace, pod)
			if err != nil {
				return err
			}

			err = r.setSlaveAnnotationIfNecessary(rf.Namespace, pod, rf)
			if err != nil {
				return err
			}
		}
	}
	if newMasterIP == "" {
		return errors.New("SetOldestAsMaster- unable to set master")
	} else {
		return nil
	}
}

// SetMasterOnAll puts all redis nodes as a slave of a given master
func (r *RedisFailoverHealer) SetMasterOnAll(masterIP string, rf *redisfailoverv1.RedisFailover) error {
	ssp, err := r.k8sService.GetStatefulSetPods(rf.Namespace, GetRedisName(rf))
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

	port := getRedisPort(rf.Spec.Redis.Port)
	for _, pod := range ssp.Items {
		//During this configuration process if there is a new master selected , bailout
		isMaster, err := r.redisClient.IsMaster(masterIP, port, password, tlsConfig)
		if err != nil || !isMaster {
			r.logger.WithField("redisfailover", rf.ObjectMeta.Name).WithField("namespace", rf.ObjectMeta.Namespace).Errorf("check master failed maybe this node is not ready(ip changed), or sentinel made a switch: %s", masterIP)
			return err
		} else {
			if pod.Status.PodIP == masterIP {
				continue
			}
			r.logger.WithField("redisfailover", rf.ObjectMeta.Name).WithField("namespace", rf.ObjectMeta.Namespace).Infof("Making pod %s slave of %s", pod.Name, masterIP)
			if err := r.redisClient.MakeSlaveOfWithPort(pod.Status.PodIP, port, masterIP, port, password, tlsConfig); err != nil {
				r.logger.WithField("redisfailover", rf.ObjectMeta.Name).WithField("namespace", rf.ObjectMeta.Namespace).Errorf("Make slave failed, slave ip: %s, master ip: %s, error: %v", pod.Status.PodIP, masterIP, err)
				return err
			}

			err = r.setSlaveLabelIfNecessary(rf.Namespace, pod)
			if err != nil {
				return err
			}
			err = r.setSlaveAnnotationIfNecessary(rf.Namespace, pod, rf)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// SetExternalMasterOnAll puts all redis nodes as a slave of a given master outside of
// the current RedisFailover instance
func (r *RedisFailoverHealer) SetExternalMasterOnAll(masterIP, masterPort string, rf *redisfailoverv1.RedisFailover) error {
	ssp, err := r.k8sService.GetStatefulSetPods(rf.Namespace, GetRedisName(rf))
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

	localPort := getRedisPort(rf.Spec.Redis.Port)
	for _, pod := range ssp.Items {
		r.logger.WithField("redisfailover", rf.ObjectMeta.Name).WithField("namespace", rf.ObjectMeta.Namespace).Infof("Making pod %s slave of %s:%s", pod.Name, masterIP, masterPort)
		if err := r.redisClient.MakeSlaveOfWithPort(pod.Status.PodIP, localPort, masterIP, masterPort, password, tlsConfig); err != nil {
			return err
		}

		err = r.setSlaveAnnotationIfNecessary(rf.Namespace, pod, rf)
		if err != nil {
			return err
		}
	}
	return nil
}

// NewSentinelMonitor changes the master that Sentinel has to monitor
func (r *RedisFailoverHealer) NewSentinelMonitor(ip string, monitor string, rf *redisfailoverv1.RedisFailover) error {
	quorum := strconv.Itoa(int(getQuorum(rf)))

	password, err := k8s.GetRedisPassword(r.k8sService, rf)
	if err != nil {
		return err
	}

	tlsConfig, err := tlsConfigFor(r.k8sService, rf)
	if err != nil {
		return err
	}

	port := getRedisPort(rf.Spec.Redis.Port)
	return r.redisClient.MonitorRedisWithPort(ip, monitor, port, quorum, password, rf.MasterName(), tlsConfig)
}

// NewSentinelMonitorWithPort changes the master that Sentinel has to monitor by the provided IP and Port
func (r *RedisFailoverHealer) NewSentinelMonitorWithPort(ip string, monitor string, monitorPort string, rf *redisfailoverv1.RedisFailover) error {
	quorum := strconv.Itoa(int(getQuorum(rf)))

	password, err := k8s.GetRedisPassword(r.k8sService, rf)
	if err != nil {
		return err
	}

	tlsConfig, err := tlsConfigFor(r.k8sService, rf)
	if err != nil {
		return err
	}

	return r.redisClient.MonitorRedisWithPort(ip, monitor, monitorPort, quorum, password, rf.MasterName(), tlsConfig)
}

// RestoreSentinel clears the number of sentinels on memory.
func (r *RedisFailoverHealer) RestoreSentinel(ip string, rf *redisfailoverv1.RedisFailover) error {
	r.logger.Debugf("Restoring sentinel %s", ip)
	tlsConfig, err := tlsConfigFor(r.k8sService, rf)
	if err != nil {
		return err
	}
	return r.redisClient.ResetSentinel(ip, tlsConfig)
}

// SetSentinelCustomConfig will call sentinel to set the configuration given in config
func (r *RedisFailoverHealer) SetSentinelCustomConfig(ip string, rf *redisfailoverv1.RedisFailover) error {
	r.logger.WithField("redisfailover", rf.ObjectMeta.Name).WithField("namespace", rf.ObjectMeta.Namespace).Debugf("Setting the custom config on sentinel %s...", ip)
	tlsConfig, err := tlsConfigFor(r.k8sService, rf)
	if err != nil {
		return err
	}
	return r.redisClient.SetCustomSentinelConfig(ip, rf.MasterName(), rf.Spec.Sentinel.CustomConfig, tlsConfig)
}

// SetRedisCustomConfig will call redis to set the configuration given in config
func (r *RedisFailoverHealer) SetRedisCustomConfig(ip string, rf *redisfailoverv1.RedisFailover) error {
	r.logger.WithField("redisfailover", rf.ObjectMeta.Name).WithField("namespace", rf.ObjectMeta.Namespace).Debugf("Setting the custom config on redis %s...", ip)

	password, err := k8s.GetRedisPassword(r.k8sService, rf)
	if err != nil {
		return err
	}

	// Get memory usage for this Redis pod
	podMemory, err := r.getRedisPodMemoryUsage(ip, rf)
	if err != nil {
		r.logger.WithField("redisfailover", rf.ObjectMeta.Name).WithField("namespace", rf.ObjectMeta.Namespace).Warningf("Failed to get memory usage for Redis IP %s: %v", ip, err)
		// Continue with podMemory = 0, which will skip memory validation
	}

	// Validate and filter maxmemory configuration
	validatedConfig, err := r.validateMaxMemoryConfig(rf.Spec.Redis.CustomConfig, podMemory, ip, rf)
	if err != nil {
		r.logger.WithField("redisfailover", rf.ObjectMeta.Name).WithField("namespace", rf.ObjectMeta.Namespace).Errorf("maxmemory validation failed for Redis IP %s: %v", ip, err)
	}

	tlsConfig, err := tlsConfigFor(r.k8sService, rf)
	if err != nil {
		return err
	}

	port := getRedisPort(rf.Spec.Redis.Port)
	return r.redisClient.SetCustomRedisConfig(ip, port, validatedConfig, password, tlsConfig)
}

// getRedisPodMemoryUsage retrieves the memory limit or request for a Redis pod by its IP
func (r *RedisFailoverHealer) getRedisPodMemoryUsage(redisIP string, rf *redisfailoverv1.RedisFailover) (int64, error) {
	// Get the specific pod by listing with field selector for IP
	pods, err := r.k8sService.ListPodsWithFieldSelector(rf.Namespace, "status.podIP="+redisIP)
	if err != nil {
		return 0, fmt.Errorf("failed to get pod with IP %s: %w", redisIP, err)
	}

	if len(pods.Items) == 0 {
		return 0, fmt.Errorf("no pod found with IP %s", redisIP)
	}

	// Use the first pod (there should only be one with a specific IP)
	targetPod := pods.Items[0]

	// Check if the pod is running
	if targetPod.Status.Phase != v1.PodRunning {
		return 0, fmt.Errorf("pod %s is not running, current phase: %s", targetPod.Name, targetPod.Status.Phase)
	}

	// Get configured memory from pod spec (prioritize Requests over Limits)
	for _, container := range targetPod.Spec.Containers {
		if container.Name == "redis" {
			// First priority: Check Requests
			if memRequest := container.Resources.Requests.Memory(); memRequest != nil {
				return memRequest.Value(), nil
			}
			// Second priority: Check Limits
			if memLimit := container.Resources.Limits.Memory(); memLimit != nil {
				return memLimit.Value(), nil
			}
		}
	}

	// If no resource limits/requests are set, return 0 (validation will be skipped)
	return 0, fmt.Errorf("no memory configuration found for pod %s", targetPod.Name)
}

// validateMaxMemoryConfig validates maxmemory configuration against pod memory using percentage-based threshold
func (r *RedisFailoverHealer) validateMaxMemoryConfig(customConfig []string, podMemory int64, ip string, rf *redisfailoverv1.RedisFailover) ([]string, error) {
	validatedConfig := make([]string, 0, len(customConfig))
	var validationErrors []error

	// Get the memory overhead percentage (default is 10%)
	reservedPodMemoryPercent := rf.Spec.Redis.ReservedPodMemoryPercent
	if reservedPodMemoryPercent <= 0 {
		reservedPodMemoryPercent = 10 // fallback to default if not set properly
	}

	for _, configLine := range customConfig {
		// Check if this is a maxmemory configuration line (not maxmemory-policy or other maxmemory-* directives)
		if strings.HasPrefix(configLine, "maxmemory ") {
			// Parse maxmemory value
			parts := strings.Fields(configLine)
			if len(parts) >= 2 {
				maxMemoryStr := parts[1]
				maxMemoryBytes, err := ParseMemorySize(maxMemoryStr)
				if err != nil {
					r.logger.WithField("redisfailover", rf.ObjectMeta.Name).WithField("namespace", rf.ObjectMeta.Namespace).Warningf("Failed to parse maxmemory value '%s' for Redis IP %s: %v, skipping this config line", maxMemoryStr, ip, err)
					validationErrors = append(validationErrors, fmt.Errorf("invalid maxmemory configuration '%s': %w", configLine, err))
					continue // Skip this invalid config line but continue with others
				}

				// Calculate allowed memory: pod memory * (100 - threshold) / 100
				// For example: if reservedPodMemoryPercent is 10%, then allowed memory is only 90% of pod memory
				if podMemory > 0 {
					allowedMemory := podMemory * int64(100-reservedPodMemoryPercent) / 100
					if maxMemoryBytes > allowedMemory {
						r.logger.WithField("redisfailover", rf.ObjectMeta.Name).WithField("namespace", rf.ObjectMeta.Namespace).Errorf("maxmemory configuration %d bytes exceeds allowed limit %d bytes (%d%% of pod memory %d bytes, overhead: %d%%) for Redis IP %s, skipping this config line", maxMemoryBytes, allowedMemory, 100-reservedPodMemoryPercent, podMemory, reservedPodMemoryPercent, ip)
						validationErrors = append(validationErrors, fmt.Errorf("maxmemory %d bytes exceeds allowed limit %d bytes (%d%% of pod memory %d bytes, overhead: %d%%)", maxMemoryBytes, allowedMemory, 100-reservedPodMemoryPercent, podMemory, reservedPodMemoryPercent))
						continue // Skip this invalid maxmemory line but continue with others
					}
				}
			}
		}

		// Add all valid configurations (including valid maxmemory and all other configs like maxmemory-policy)
		validatedConfig = append(validatedConfig, configLine)
	}

	// Combine all validation errors into a single error
	if len(validationErrors) > 0 {
		return validatedConfig, errors.Join(validationErrors...)
	}

	return validatedConfig, nil
}

// ParseMemorySize parses Redis memory size strings (e.g., "1gb", "512mb", "1024")
func ParseMemorySize(sizeStr string) (int64, error) {
	sizeStr = strings.ToLower(strings.TrimSpace(sizeStr))

	// Handle plain numbers (bytes)
	if val, err := strconv.ParseInt(sizeStr, 10, 64); err == nil {
		return val, nil
	}

	// Handle suffixed values
	var multiplier int64 = 1
	var numStr string

	if strings.HasSuffix(sizeStr, "gb") || strings.HasSuffix(sizeStr, "g") {
		multiplier = 1024 * 1024 * 1024
		if strings.HasSuffix(sizeStr, "gb") {
			numStr = sizeStr[:len(sizeStr)-2]
		} else {
			numStr = sizeStr[:len(sizeStr)-1]
		}
	} else if strings.HasSuffix(sizeStr, "mb") || strings.HasSuffix(sizeStr, "m") {
		multiplier = 1024 * 1024
		if strings.HasSuffix(sizeStr, "mb") {
			numStr = sizeStr[:len(sizeStr)-2]
		} else {
			numStr = sizeStr[:len(sizeStr)-1]
		}
	} else if strings.HasSuffix(sizeStr, "kb") || strings.HasSuffix(sizeStr, "k") {
		multiplier = 1024
		if strings.HasSuffix(sizeStr, "kb") {
			numStr = sizeStr[:len(sizeStr)-2]
		} else {
			numStr = sizeStr[:len(sizeStr)-1]
		}
	} else {
		return 0, fmt.Errorf("unsupported memory size format: %s", sizeStr)
	}

	val, err := strconv.ParseFloat(numStr, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid numeric value in memory size: %s", sizeStr)
	}

	return int64(val * float64(multiplier)), nil
}

// DeletePod delete a failing pod so kubernetes relaunch it again
func (r *RedisFailoverHealer) DeletePod(podName string, rFailover *redisfailoverv1.RedisFailover) error {
	r.logger.WithField("redisfailover", rFailover.ObjectMeta.Name).WithField("namespace", rFailover.ObjectMeta.Namespace).Infof("Deleting pods %s...", podName)
	return r.k8sService.DeletePod(rFailover.Namespace, podName)
}
