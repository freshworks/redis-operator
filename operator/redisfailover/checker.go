package redisfailover

import (
	"errors"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"

	redisfailoverv1 "github.com/freshworks/redis-operator/api/redisfailover/v1"
	"github.com/freshworks/redis-operator/metrics"
)

// masterResizeTimeout and slaveResizeTimeout bound how long an in-place resize attempt
// (including any headroom eviction) is allowed to stay Deferred before falling back to the
// delete-based rollout. The master gets a longer window because a master restart is costly
// (Sentinel failover); a slave restart is already considered cheap and acceptable today, so
// there's less reason to hold out long before falling back to what would have happened
// anyway. There is no requeue/backoff in this operator (see factory.go's resync), so these
// are evaluated fresh against the resize-started-at annotation on every ~30s reconcile.
const (
	masterResizeTimeout = 6 * time.Minute
	slaveResizeTimeout  = 2 * time.Minute
)

// UpdateRedisesPods deletes Redis pods with a stale StatefulSet revision (OnDelete rollout),
// or resizes them in place instead when the pending change is resource-only (see
// k8s.ResizeOnlyAnnotationKey).
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

	resizeOnly, err := r.rfChecker.GetStatefulSetResizeOnly(rf)
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
			if !resizeOnly {
				//Delete pod and wait next round to check if the new one is synced
				err = r.rfHealer.DeletePod(pod, rf)
				if err != nil {
					return err
				}
				return nil
			}
			return r.attemptPodResize(rf, pod, ssUR, slaveResizeTimeout, false)
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
			if !resizeOnly {
				err = r.rfHealer.DeletePod(master, rf)
				if err != nil {
					return err
				}
				return nil
			}
			return r.attemptPodResize(rf, master, ssUR, masterResizeTimeout, true)
		}
	}

	return nil
}

// attemptPodResize is reached only once UpdateRedisesPods has confirmed the pending
// StatefulSet revision change is resource-only (see k8s.ResizeOnlyAnnotationKey). It resizes
// podName (master or slave) in place instead of deleting it, and falls back to the existing
// delete-based rollout if the resize is Infeasible, Deferred with allowEviction false, or
// exceeds timeout.
// For the master, the replication-lag precondition is already enforced by the ready-check at
// the top of UpdateRedisesPods; slaves have no equivalent precondition, since growing a slave
// carries none of the risk that growing the master past what a slave can hold does.
// allowEviction gates whether a Deferred resize may evict other RedisFailovers' co-located
// slave pods to free headroom - true for the master, since a master restart is costly enough
// to justify disrupting another tenant's slave; false for slaves, since a slave restart is
// already cheap and acceptable, and there's no justification for evicting someone else's pod
// just to avoid one - a Deferred slave resize falls straight to delete instead.
func (r *RedisFailoverHandler) attemptPodResize(rf *redisfailoverv1.RedisFailover, podName, ssUR string, timeout time.Duration, allowEviction bool) error {
	startedAt, inProgress, err := r.rfChecker.GetResizeState(podName, rf)
	if err != nil {
		return err
	}

	if !inProgress {
		if err := r.rfHealer.SetResizeStartedAt(podName, rf); err != nil {
			return err
		}
		if err := r.rfHealer.ResizePod(podName, rf); err != nil {
			return err
		}
		// Give the kubelet a reconcile cycle to act on the request before checking outcome.
		return nil
	}

	if time.Since(startedAt) > timeout {
		r.logger.WithField("redisfailover", rf.ObjectMeta.Name).WithField("pod", podName).Warningf("resize timed out after %s, falling back to delete", timeout)
		if err := r.rfHealer.ClearResizeState(podName, rf); err != nil {
			return err
		}
		return r.rfHealer.DeletePod(podName, rf)
	}

	found, reason, err := r.rfChecker.GetPodResizeCondition(podName, rf)
	if err != nil {
		return err
	}

	if !found {
		// No PodResizePending condition is necessary but not sufficient proof the resize
		// applied - it's equally true if the resize call never reached the pod at all (e.g.
		// an RBAC rejection on the resize subresource never gets this far in the first
		// place). Confirm the pod's actual resources match before declaring success.
		matches, err := r.rfChecker.PodResourcesMatchDesired(podName, rf)
		if err != nil {
			return err
		}
		if !matches {
			r.logger.WithField("redisfailover", rf.ObjectMeta.Name).WithField("pod", podName).Warningf("no resize-pending condition but resources don't match desired spec yet, retrying resize")
			return r.rfHealer.ResizePod(podName, rf)
		}
		// Resize applied. Close the controller-revision-hash bookkeeping gap: a resize never
		// updates that label on its own, so without this the next reconcile would see
		// this pod's revision != ssUR again and re-enter this whole branch.
		if err := r.rfHealer.RelabelPodRevision(podName, rf, ssUR); err != nil {
			return err
		}
		return r.rfHealer.ClearResizeState(podName, rf)
	}

	switch reason {
	case corev1.PodReasonInfeasible:
		r.logger.WithField("redisfailover", rf.ObjectMeta.Name).WithField("pod", podName).Warningf("resize infeasible on current node, falling back to delete")
		if err := r.rfHealer.ClearResizeState(podName, rf); err != nil {
			return err
		}
		return r.rfHealer.DeletePod(podName, rf)

	case corev1.PodReasonDeferred:
		if !allowEviction {
			r.logger.WithField("redisfailover", rf.ObjectMeta.Name).WithField("pod", podName).Warningf("resize deferred and eviction not permitted for this pod, falling back to delete")
			if err := r.rfHealer.ClearResizeState(podName, rf); err != nil {
				return err
			}
			return r.rfHealer.DeletePod(podName, rf)
		}

		// Re-issue the resize on every retry, not just the first attempt. ResizePod only ran
		// once, when this attempt began; if the CR's resources changed since (e.g. a human
		// lowers the ask after seeing it's stuck), the pod's live resize target would
		// otherwise stay pinned to whatever was originally requested, and the only way it'd
		// ever pick up the new value is the eventual timeout-driven DeletePod fallback. This
		// keeps the pod's target in sync with the CR on every reconcile; it's a no-op patch
		// if nothing changed.
		if err := r.rfHealer.ResizePod(podName, rf); err != nil {
			return err
		}

		nodeName, err := r.rfChecker.GetPodNode(podName, rf)
		if err != nil {
			return err
		}
		requiredCPU, requiredMemory, err := r.rfChecker.ComputeRequiredHeadroom(rf, nodeName, podName)
		if err != nil {
			return err
		}
		if requiredCPU.Sign() <= 0 && requiredMemory.Sign() <= 0 {
			// Already fits from the node's perspective; let the kubelet's own retry catch up.
			return nil
		}
		if err := r.rfHealer.FreeResizeHeadroom(rf, nodeName, requiredCPU, requiredMemory); err != nil {
			r.logger.WithField("redisfailover", rf.ObjectMeta.Name).WithField("pod", podName).Warningf("could not free enough headroom yet, will keep retrying until timeout: %v", err)
		}
		return nil
	}

	return nil
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
