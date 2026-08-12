package redisfailover

import (
	"errors"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"

	redisfailoverv1 "github.com/freshworks/redis-operator/api/redisfailover/v1"
	"github.com/freshworks/redis-operator/metrics"
)

// UpdateRedisesPods deletes Redis pods with a stale StatefulSet revision (OnDelete rollout),
// or resizes them in place instead when eligible (see canResizePod).
// DisableMasterRollout when true, only slave pods are rolled out on spec change (OnDelete); master is not deleted until the flag is removed.
func (r *RedisFailoverHandler) UpdateRedisesPods(rf *redisfailoverv1.RedisFailover) error {
	redises, err := r.rfChecker.GetRedisesIPs(rf)
	if err != nil {
		return err
	}

	masterIP := ""
	if !rf.Bootstrapping() {
		masterIP, _ = r.rfChecker.GetMasterIP(rf)
	}
	// No perform updates when nodes are syncing, still not connected, etc.
	for _, rip := range redises {
		if rip != masterIP {
			ready, err := r.rfChecker.CheckRedisSlavesReady(rip, rf)
			if err != nil {
				return err
			}
			if !ready {
				return nil
			}
		}
	}

	ssUR, err := r.rfChecker.GetStatefulSetUpdateRevision(rf)
	if err != nil {
		return err
	}

	redisesPods, err := r.rfChecker.GetRedisesSlavesPods(rf)
	if err != nil {
		return err
	}

	// Update stale pods with slave role
	for _, pod := range redisesPods {
		revision, err := r.rfChecker.GetRedisRevisionHash(pod, rf)
		if err != nil {
			return err
		}
		if revision != ssUR {
			canResize, err := r.canResizePod(rf, revision)
			if err != nil {
				// Transient, not confirmed either way - retry next reconcile rather than delete.
				r.logger.WithField("redisfailover", rf.ObjectMeta.Name).WithField("pod", pod).Warningf("could not determine resize eligibility, will retry next reconcile: %v", err)
				return nil
			}
			if !canResize {
				//Delete pod and wait next round to check if the new one is synced
				err = r.rfHealer.DeletePod(pod, rf)
				if err != nil {
					return err
				}
				return nil
			}
			return r.attemptPodResize(rf, pod, ssUR)
		}
	}

	if !rf.Bootstrapping() && !rf.Spec.Redis.DisableMasterRollout {
		master, err := r.rfChecker.GetRedisesMasterPod(rf)
		if err != nil {
			return err
		}

		masterRevision, err := r.rfChecker.GetRedisRevisionHash(master, rf)
		if err != nil {
			return err
		}
		if masterRevision != ssUR {
			canResize, err := r.canResizePod(rf, masterRevision)
			if err != nil {
				r.logger.WithField("redisfailover", rf.ObjectMeta.Name).WithField("pod", master).Warningf("could not determine resize eligibility, will retry next reconcile: %v", err)
				return nil
			}
			if !canResize {
				err = r.rfHealer.DeletePod(master, rf)
				if err != nil {
					return err
				}
				return nil
			}
			return r.attemptPodResize(rf, master, ssUR)
		}
	}

	return nil
}

// canResizePod reports whether the pod on podRevision is eligible for in-place resize. Checked
// fresh per pod, not cached, since a StatefulSet-level signal would go stale once the
// StatefulSet itself stops changing (see IsPodResourceOnlyChange). A non-nil error means
// genuinely transient/retryable, not a confirmed "no" - callers must not delete on it.
func (r *RedisFailoverHandler) canResizePod(rf *redisfailoverv1.RedisFailover, podRevision string) (bool, error) {
	if len(rf.Spec.Redis.ResizePolicy) == 0 {
		return false, nil
	}
	return r.rfChecker.IsPodResourceOnlyChange(podRevision, rf)
}

// resizeInProgressTimeout bounds how long a stuck PodResizeInProgress (no completion, no
// reported error) is tolerated before falling back to delete - e.g. a resize target the kubelet
// can never actually actuate. Deferred/Infeasible still fall back immediately; this is narrower.
// Read straight off the condition's own LastTransitionTime, no annotation needed.
const resizeInProgressTimeout = 5 * time.Minute

// attemptPodResize is reached only once canResizePod has confirmed podName is eligible. Master
// and slave follow identical logic. No annotation tracks "was a resize already submitted" - the
// resize subresource writes pod.Spec synchronously, so PodResourcesMatchDesired doubles as that
// signal. Infeasible/Deferred fall straight back to delete; PodResizeInProgress waits (kubelet
// is still applying the change) up to resizeInProgressTimeout before doing the same.
func (r *RedisFailoverHandler) attemptPodResize(rf *redisfailoverv1.RedisFailover, podName, ssUR string) error {
	matches, err := r.rfChecker.PodResourcesMatchDesired(podName, rf)
	if err != nil {
		return err
	}
	if !matches {
		return r.rfHealer.ResizePod(podName, rf)
	}

	cond, err := r.rfChecker.GetPodResizeCondition(podName, rf)
	if err != nil {
		return err
	}
	if cond != nil {
		if cond.Type == corev1.PodResizeInProgress && cond.Reason != corev1.PodReasonError {
			if time.Since(cond.LastTransitionTime.Time) < resizeInProgressTimeout {
				return nil
			}
			r.logger.WithField("redisfailover", rf.ObjectMeta.Name).WithField("pod", podName).Warningf("resize stuck in PodResizeInProgress for over %s with no resolution, falling back to delete", resizeInProgressTimeout)
			return r.rfHealer.DeletePod(podName, rf)
		}
		r.logger.WithField("redisfailover", rf.ObjectMeta.Name).WithField("pod", podName).Warningf("resize %s (%s), falling back to delete", cond.Type, cond.Reason)
		return r.rfHealer.DeletePod(podName, rf)
	}

	// A resize never updates controller-revision-hash on its own.
	return r.rfHealer.RelabelPodRevision(podName, rf, ssUR)
}

// CheckAndHeal runs verifcation checks to ensure the RedisFailover is in an expected and healthy state.
// If the checks do not match up to expectations, an attempt will be made to "heal" the RedisFailover into a healthy state.
func (r *RedisFailoverHandler) CheckAndHeal(rf *redisfailoverv1.RedisFailover) error {
	if rf.Bootstrapping() {
		return r.checkAndHealBootstrapMode(rf)
	}

	// Number of redis is equal as the set on the RF spec
	// Number of sentinel is equal as the set on the RF spec
	// Check only one master
	// Number of redis master is 1
	// All redis slaves have the same master
	// All sentinels points to the same redis master
	// Sentinel has not death nodes
	// Sentinel knows the correct slave number

	if !r.rfChecker.IsRedisRunning(rf) {
		setRedisCheckerMetrics(r.mClient, "redis", rf.Namespace, rf.Name, metrics.REDIS_REPLICA_MISMATCH, metrics.NOT_APPLICABLE, errors.New("not all replicas running"))
		r.logger.WithField("redisfailover", rf.ObjectMeta.Name).WithField("namespace", rf.ObjectMeta.Namespace).Debugf("Number of redis mismatch, waiting for redis statefulset reconcile")
		return nil
	}

	if !r.rfChecker.IsSentinelRunning(rf) {
		setRedisCheckerMetrics(r.mClient, "sentinel", rf.Namespace, rf.Name, metrics.SENTINEL_REPLICA_MISMATCH, metrics.NOT_APPLICABLE, errors.New("not all replicas running"))
		r.logger.WithField("redisfailover", rf.ObjectMeta.Name).WithField("namespace", rf.ObjectMeta.Namespace).Debugf("Number of sentinel mismatch, waiting for sentinel deployment reconcile")
		return nil
	}

	nMasters, err := r.rfChecker.GetNumberMasters(rf)
	if err != nil {
		return err
	}

	switch nMasters {
	case 0:
		setRedisCheckerMetrics(r.mClient, "redis", rf.Namespace, rf.Name, metrics.NO_MASTER, metrics.NOT_APPLICABLE, errors.New("no masters detected"))
		//when number of redis replicas is 1 , the redis is configured for standalone master mode
		//Configure to master
		if rf.Spec.Redis.Replicas == 1 {
			r.logger.WithField("redisfailover", rf.ObjectMeta.Name).WithField("namespace", rf.ObjectMeta.Namespace).Infof("Resource spec with standalone master - operator will set the master")
			err = r.rfHealer.SetOldestAsMaster(rf)
			setRedisCheckerMetrics(r.mClient, "redis", rf.Namespace, rf.Name, metrics.NO_MASTER, metrics.NOT_APPLICABLE, err)
			if err != nil {
				r.logger.WithField("redisfailover", rf.ObjectMeta.Name).WithField("namespace", rf.ObjectMeta.Namespace).Errorf("Error in Setting oldest Pod as master")
				return err
			}
			return nil
		}
		//During the First boot(New deployment or all pods of the statefulsets have restarted),
		//Sentinesl will not be able to choose the master , so operator should select a master
		//Also in scenarios where Sentinels is not in a position to choose a master like , No quorum reached
		//Operator can choose a master , These scenarios can be checked by asking the all the sentinels
		//if its in a postion to choose a master also check if the redis is configured with local host IP as master.
		r.logger.WithField("redisfailover", rf.ObjectMeta.Name).WithField("namespace", rf.ObjectMeta.Namespace).Warningf("Number of Masters running is 0")
		maxUptime, err := r.rfChecker.GetMaxRedisPodTime(rf)
		if err != nil {
			return err
		}

		r.logger.WithField("redisfailover", rf.ObjectMeta.Name).WithField("namespace", rf.ObjectMeta.Namespace).Infof("No master avaiable but max pod up time is : %f", maxUptime.Round(time.Second).Seconds())
		//Check If Sentinel has quorum to take a failover decision
		noqrm_cnt, err := r.rfChecker.CheckSentinelQuorum(rf)
		if err != nil {
			// Sentinels are not in a situation to choose a master we pick one
			r.logger.WithField("redisfailover", rf.ObjectMeta.Name).WithField("namespace", rf.ObjectMeta.Namespace).Warningf("Quorum not available for sentinel to choose master,estimated unhealthy sentinels :%d , Operator to step-in", noqrm_cnt)
			err2 := r.rfHealer.SetOldestAsMaster(rf)
			setRedisCheckerMetrics(r.mClient, "redis", rf.Namespace, rf.Name, metrics.NO_MASTER, metrics.NOT_APPLICABLE, err2)
			if err2 != nil {
				r.logger.WithField("redisfailover", rf.ObjectMeta.Name).WithField("namespace", rf.ObjectMeta.Namespace).Errorf("Error in Setting oldest Pod as master")
				return err2
			}
		} else {
			//sentinels are having a quorum to make a failover , but check if redis are not having local hostip (first boot) as master
			status, err2 := r.rfChecker.CheckIfMasterLocalhost(rf)
			if err2 != nil {
				r.logger.WithField("redisfailover", rf.ObjectMeta.Name).WithField("namespace", rf.ObjectMeta.Namespace).Errorf("CheckIfMasterLocalhost failed retry later")
				return err2
			} else if status {
				// all avaialable redis pods have local host ip as master
				r.logger.WithField("redisfailover", rf.ObjectMeta.Name).WithField("namespace", rf.ObjectMeta.Namespace).Errorf("all available redis is having local loop back as master , operator initiates master selection")
				err3 := r.rfHealer.SetOldestAsMaster(rf)
				setRedisCheckerMetrics(r.mClient, "redis", rf.Namespace, rf.Name, metrics.NO_MASTER, metrics.NOT_APPLICABLE, err3)
				if err3 != nil {
					r.logger.WithField("redisfailover", rf.ObjectMeta.Name).WithField("namespace", rf.ObjectMeta.Namespace).Errorf("Error in Setting oldest Pod as master")
					return err3
				}

			} else {

				// We'll wait until failover is done
				r.logger.WithField("redisfailover", rf.ObjectMeta.Name).WithField("namespace", rf.ObjectMeta.Namespace).Infof("no master found, wait until failover or fix manually")
				setRedisCheckerMetrics(r.mClient, "redis", rf.Namespace, rf.Name, metrics.NO_MASTER, metrics.NOT_APPLICABLE, errors.New("no master not fixed, wait until failover or fix manually"))
				return nil
			}

		}

	case 1:
		setRedisCheckerMetrics(r.mClient, "redis", rf.Namespace, rf.Name, metrics.NUMBER_OF_MASTERS, metrics.NOT_APPLICABLE, nil)
	default:
		setRedisCheckerMetrics(r.mClient, "redis", rf.Namespace, rf.Name, metrics.NUMBER_OF_MASTERS, metrics.NOT_APPLICABLE, errors.New("multiple masters detected"))
		return errors.New("more than one master, fix manually")
	}

	master, err := r.rfChecker.GetMasterIP(rf)
	if err != nil {
		return err
	}

	err = r.rfChecker.CheckAllSlavesFromMaster(master, rf)
	setRedisCheckerMetrics(r.mClient, "redis", rf.Namespace, rf.Name, metrics.SLAVE_WRONG_MASTER, metrics.NOT_APPLICABLE, err)
	if err != nil {
		r.logger.WithField("redisfailover", rf.ObjectMeta.Name).WithField("namespace", rf.ObjectMeta.Namespace).Warningf("Slave not associated to master: %s", err.Error())
		if err = r.rfHealer.SetMasterOnAll(master, rf); err != nil {
			return err
		}
	}

	err = r.applyRedisCustomConfig(rf)
	setRedisCheckerMetrics(r.mClient, "redis", rf.Namespace, rf.Name, metrics.APPLY_REDIS_CONFIG, metrics.NOT_APPLICABLE, err)
	if err != nil {
		return err
	}

	err = r.UpdateRedisesPods(rf)
	if err != nil {
		return err
	}

	sentinels, err := r.rfChecker.GetSentinelsIPs(rf)
	if err != nil {
		return err
	}

	port := getRedisPort(rf.Spec.Redis.Port)
	for _, sip := range sentinels {
		err = r.rfChecker.CheckSentinelMonitor(sip, rf.MasterName(), master, port)
		setRedisCheckerMetrics(r.mClient, "sentinel", rf.Namespace, rf.Name, metrics.SENTINEL_WRONG_MASTER, sip, err)
		if err != nil {
			r.logger.WithField("redisfailover", rf.ObjectMeta.Name).WithField("namespace", rf.ObjectMeta.Namespace).Warningf("Fixing sentinel not monitoring expected master: %s", err.Error())
			if err := r.rfHealer.NewSentinelMonitor(sip, master, rf); err != nil {
				return err
			}
		}
	}
	return r.checkAndHealSentinels(rf, sentinels)
}

func (r *RedisFailoverHandler) checkAndHealBootstrapMode(rf *redisfailoverv1.RedisFailover) error {

	if !r.rfChecker.IsRedisRunning(rf) {
		setRedisCheckerMetrics(r.mClient, "redis", rf.Namespace, rf.Name, metrics.REDIS_REPLICA_MISMATCH, metrics.NOT_APPLICABLE, errors.New("not all replicas running"))
		r.logger.WithField("redisfailover", rf.ObjectMeta.Name).WithField("namespace", rf.ObjectMeta.Namespace).Debugf("Number of redis mismatch, waiting for redis statefulset reconcile")
		return nil
	}

	err := r.UpdateRedisesPods(rf)
	if err != nil {
		return err
	}
	err = r.applyRedisCustomConfig(rf)
	setRedisCheckerMetrics(r.mClient, "redis", rf.Namespace, rf.Name, metrics.APPLY_REDIS_CONFIG, metrics.NOT_APPLICABLE, err)
	if err != nil {
		return err
	}

	bootstrapSettings := rf.Spec.BootstrapNode
	err = r.rfHealer.SetExternalMasterOnAll(bootstrapSettings.Host, bootstrapSettings.Port, rf)
	setRedisCheckerMetrics(r.mClient, "redis", rf.Namespace, rf.Name, metrics.APPLY_EXTERNAL_MASTER, metrics.NOT_APPLICABLE, err)
	if err != nil {
		return err
	}

	if rf.SentinelsAllowed() {
		if !r.rfChecker.IsSentinelRunning(rf) {
			setRedisCheckerMetrics(r.mClient, "sentinel", rf.Namespace, rf.Name, metrics.SENTINEL_REPLICA_MISMATCH, metrics.NOT_APPLICABLE, errors.New("not all replicas running"))
			r.logger.WithField("redisfailover", rf.ObjectMeta.Name).WithField("namespace", rf.ObjectMeta.Namespace).Debugf("Number of sentinel mismatch, waiting for sentinel deployment reconcile")
			return nil
		}

		sentinels, err := r.rfChecker.GetSentinelsIPs(rf)
		if err != nil {
			return err
		}
		for _, sip := range sentinels {
			err = r.rfChecker.CheckSentinelMonitor(sip, rf.MasterName(), bootstrapSettings.Host, bootstrapSettings.Port)
			setRedisCheckerMetrics(r.mClient, "sentinel", rf.Namespace, rf.Name, metrics.SENTINEL_WRONG_MASTER, sip, err)
			if err != nil {
				r.logger.WithField("redisfailover", rf.ObjectMeta.Name).WithField("namespace", rf.ObjectMeta.Namespace).Warningf("Fixing sentinel not monitoring expected master: %s", err.Error())
				if err := r.rfHealer.NewSentinelMonitorWithPort(sip, bootstrapSettings.Host, bootstrapSettings.Port, rf); err != nil {
					return err
				}
			}
		}
		return r.checkAndHealSentinels(rf, sentinels)
	}
	return nil
}

func (r *RedisFailoverHandler) applyRedisCustomConfig(rf *redisfailoverv1.RedisFailover) error {
	redises, err := r.rfChecker.GetRedisesIPs(rf)
	if err != nil {
		return err
	}
	for _, rip := range redises {
		if err := r.rfHealer.SetRedisCustomConfig(rip, rf); err != nil {
			return err
		}
	}
	return nil
}

func (r *RedisFailoverHandler) checkAndHealSentinels(rf *redisfailoverv1.RedisFailover, sentinels []string) error {
	for _, sip := range sentinels {
		err := r.rfChecker.CheckSentinelNumberInMemory(sip, rf)
		setRedisCheckerMetrics(r.mClient, "sentinel", rf.Namespace, rf.Name, metrics.SENTINEL_NUMBER_IN_MEMORY_MISMATCH, sip, err)
		if err != nil {
			r.logger.WithField("redisfailover", rf.ObjectMeta.Name).WithField("namespace", rf.ObjectMeta.Namespace).Warningf("Sentinel %s mismatch number of sentinels in memory. resetting", sip)
			if err := r.rfHealer.RestoreSentinel(sip); err != nil {
				return err
			}
		}

	}
	for _, sip := range sentinels {
		err := r.rfChecker.CheckSentinelSlavesNumberInMemory(sip, rf)
		setRedisCheckerMetrics(r.mClient, "sentinel", rf.Namespace, rf.Name, metrics.REDIS_SLAVES_NUMBER_IN_MEMORY_MISMATCH, sip, err)
		if err != nil {
			r.logger.WithField("redisfailover", rf.ObjectMeta.Name).WithField("namespace", rf.ObjectMeta.Namespace).Warningf("Sentinel %s mismatch number of expected slaves in memory. resetting", sip)
			if err := r.rfHealer.RestoreSentinel(sip); err != nil {
				return err
			}
		}
	}
	for _, sip := range sentinels {
		err := r.rfHealer.SetSentinelCustomConfig(sip, rf)
		setRedisCheckerMetrics(r.mClient, "sentinel", rf.Namespace, rf.Name, metrics.APPLY_SENTINEL_CONFIG, sip, err)
		if err != nil {
			return err
		}
	}
	return nil
}

func getRedisPort(p int32) string {
	return strconv.Itoa(int(p))
}

func setRedisCheckerMetrics(metricsClient metrics.Recorder, mode /* redis or sentinel? */ string, rfNamespace string, rfName string, property string, IP string, err error) {
	switch mode {
	case "sentinel":
		if err != nil {
			metricsClient.RecordSentinelCheck(rfNamespace, rfName, property, IP, metrics.STATUS_UNHEALTHY)
		} else {
			metricsClient.RecordSentinelCheck(rfNamespace, rfName, property, IP, metrics.STATUS_HEALTHY)
		}
	default: // redis
		if err != nil {
			metricsClient.RecordRedisCheck(rfNamespace, rfName, property, IP, metrics.STATUS_UNHEALTHY)
		} else {
			metricsClient.RecordRedisCheck(rfNamespace, rfName, property, IP, metrics.STATUS_HEALTHY)
		}
	}
}
