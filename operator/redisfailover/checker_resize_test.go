package redisfailover_test

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	redisfailoverv1 "github.com/freshworks/redis-operator/api/redisfailover/v1"
	"github.com/freshworks/redis-operator/log"
	"github.com/freshworks/redis-operator/metrics"
	mRFService "github.com/freshworks/redis-operator/mocks/operator/redisfailover/service"
	mK8SService "github.com/freshworks/redis-operator/mocks/service/k8s"
	rfOperator "github.com/freshworks/redis-operator/operator/redisfailover"
)

// resizeCondition builds a pod resize condition that transitioned age ago - age matters only
// for PodResizeInProgress cases exercising the resizeInProgressTimeout backstop.
func resizeCondition(condType corev1.PodConditionType, reason string, age time.Duration) *corev1.PodCondition {
	return &corev1.PodCondition{
		Type:               condType,
		Status:             corev1.ConditionTrue,
		Reason:             reason,
		LastTransitionTime: metav1.NewTime(time.Now().Add(-age)),
	}
}

// This file covers attemptPodResize's branches (checker.go), reached via UpdateRedisesPods so
// the real master/slave call sites are exercised, not just the function in isolation. Master
// and slave now share identical logic, and there is no annotation-based state: the pod's live
// resources (via PodResourcesMatchDesired), resize condition, and its revision's resource-only-
// ness (via IsPodResourceOnlyChange, keyed by controller-revision-hash) are the only state
// consulted, checked fresh on every call.

const (
	testSSVersion = "new-revision"
	testMasterPod = "master-pod"
	testSlavePod  = "slave-pod"
)

func newResizeTestHandler(rf *redisfailoverv1.RedisFailover, mrfc *mRFService.RedisFailoverCheck, mrfh *mRFService.RedisFailoverHeal) *rfOperator.RedisFailoverHandler {
	config := generateConfig()
	mrfs := &mRFService.RedisFailoverClient{}
	mk := &mK8SService.Services{}
	return rfOperator.NewRedisFailoverHandler(config, mrfs, mrfc, mrfh, mk, metrics.Dummy, log.Dummy)
}

func testResizePolicy() []corev1.ContainerResizePolicy {
	return []corev1.ContainerResizePolicy{
		{ResourceName: corev1.ResourceCPU, RestartPolicy: corev1.NotRequired},
	}
}

// setupMasterResizeTest wires up the mocks needed to reach attemptPodResize for the master pod,
// with ResizePolicy set on the CR (the opt-in gate) and the master's own pending change
// confirmed resource-only via IsPodResourceOnlyChange - a single redis IP (so the ready-check
// loop has no slaves to check), one stale master revision, no stale slaves.
func setupMasterResizeTest() (*redisfailoverv1.RedisFailover, *mRFService.RedisFailoverCheck, *mRFService.RedisFailoverHeal) {
	rf := generateRF(false, false, false)
	rf.Spec.Redis.ResizePolicy = testResizePolicy()
	mrfc := &mRFService.RedisFailoverCheck{}
	mrfh := &mRFService.RedisFailoverHeal{}

	mrfc.On("GetRedisesIPs", rf).Once().Return([]string{"1.1.1.1"}, nil)
	mrfc.On("GetMasterIP", rf).Once().Return("1.1.1.1", nil)
	mrfc.On("GetStatefulSetUpdateRevision", rf).Once().Return(testSSVersion, nil)
	mrfc.On("GetRedisesSlavesPods", rf).Once().Return([]string{}, nil)
	mrfc.On("GetRedisesMasterPod", rf).Once().Return(testMasterPod, nil)
	mrfc.On("GetRedisRevisionHash", testMasterPod, rf).Once().Return("old-revision", nil)
	mrfc.On("IsPodResourceOnlyChange", "old-revision", rf).Once().Return(true, nil)

	return rf, mrfc, mrfh
}

// setupSlaveResizeTest is the slave-branch equivalent: one slave pod with a stale revision, so
// UpdateRedisesPods reaches attemptPodResize before ever considering the master.
func setupSlaveResizeTest() (*redisfailoverv1.RedisFailover, *mRFService.RedisFailoverCheck, *mRFService.RedisFailoverHeal) {
	rf := generateRF(false, false, false)
	rf.Spec.Redis.ResizePolicy = testResizePolicy()
	mrfc := &mRFService.RedisFailoverCheck{}
	mrfh := &mRFService.RedisFailoverHeal{}

	mrfc.On("GetRedisesIPs", rf).Once().Return([]string{"1.1.1.1", "2.2.2.2"}, nil)
	mrfc.On("GetMasterIP", rf).Once().Return("1.1.1.1", nil)
	mrfc.On("CheckRedisSlavesReady", "2.2.2.2", rf).Once().Return(true, nil)
	mrfc.On("GetStatefulSetUpdateRevision", rf).Once().Return(testSSVersion, nil)
	mrfc.On("GetRedisesSlavesPods", rf).Once().Return([]string{testSlavePod}, nil)
	mrfc.On("GetRedisRevisionHash", testSlavePod, rf).Once().Return("old-revision", nil)
	mrfc.On("IsPodResourceOnlyChange", "old-revision", rf).Once().Return(true, nil)

	return rf, mrfc, mrfh
}

func TestAttemptPodResize_NotYetSubmitted_Submits(t *testing.T) {
	rf, mrfc, mrfh := setupMasterResizeTest()
	mrfc.On("PodResourcesMatchDesired", testMasterPod, rf).Once().Return(false, nil)
	mrfh.On("ResizePod", testMasterPod, rf).Once().Return(nil)

	err := newResizeTestHandler(rf, mrfc, mrfh).UpdateRedisesPods(rf)

	assert.NoError(t, err)
	mrfc.AssertExpectations(t)
	mrfh.AssertExpectations(t)
}

func TestAttemptPodResize_AlreadySubmitted_Infeasible_FallsBackToDelete(t *testing.T) {
	rf, mrfc, mrfh := setupMasterResizeTest()
	mrfc.On("PodResourcesMatchDesired", testMasterPod, rf).Once().Return(true, nil)
	mrfc.On("GetPodResizeCondition", testMasterPod, rf).Once().Return(resizeCondition(corev1.PodResizePending, corev1.PodReasonInfeasible, 0), nil)
	mrfh.On("DeletePod", testMasterPod, rf).Once().Return(nil)

	err := newResizeTestHandler(rf, mrfc, mrfh).UpdateRedisesPods(rf)

	assert.NoError(t, err)
	mrfc.AssertExpectations(t)
	mrfh.AssertExpectations(t)
}

func TestAttemptPodResize_AlreadySubmitted_Deferred_FallsBackToDelete(t *testing.T) {
	rf, mrfc, mrfh := setupMasterResizeTest()
	mrfc.On("PodResourcesMatchDesired", testMasterPod, rf).Once().Return(true, nil)
	mrfc.On("GetPodResizeCondition", testMasterPod, rf).Once().Return(resizeCondition(corev1.PodResizePending, corev1.PodReasonDeferred, 0), nil)
	mrfh.On("DeletePod", testMasterPod, rf).Once().Return(nil)
	// No eviction machinery exists anymore to stub - Deferred goes straight to delete, same as
	// Infeasible, with no waiting.

	err := newResizeTestHandler(rf, mrfc, mrfh).UpdateRedisesPods(rf)

	assert.NoError(t, err)
	mrfc.AssertExpectations(t)
	mrfh.AssertExpectations(t)
}

// TestAttemptPodResize_ResizeInProgress_WaitsRatherThanRelabeling guards the fix for a TOCTOU
// gap: ResizePod writes the desired spec synchronously, so PodResourcesMatchDesired can already
// read true while the kubelet is still actively actuating the change (PodResizeInProgress, no
// error) - this must wait for a later reconcile rather than treating it as done, as long as it's
// within resizeInProgressTimeout.
func TestAttemptPodResize_ResizeInProgress_WaitsRatherThanRelabeling(t *testing.T) {
	rf, mrfc, mrfh := setupMasterResizeTest()
	mrfc.On("PodResourcesMatchDesired", testMasterPod, rf).Once().Return(true, nil)
	mrfc.On("GetPodResizeCondition", testMasterPod, rf).Once().Return(resizeCondition(corev1.PodResizeInProgress, "", 30*time.Second), nil)
	// RelabelPodRevision/DeletePod deliberately not stubbed - neither must be called while the
	// kubelet is still actuating.

	err := newResizeTestHandler(rf, mrfc, mrfh).UpdateRedisesPods(rf)

	assert.NoError(t, err)
	mrfc.AssertExpectations(t)
	mrfh.AssertExpectations(t)
}

// TestAttemptPodResize_ResizeInProgressWithError_FallsBackToDelete: an actuation error is not
// something to keep waiting on, regardless of how recently it transitioned.
func TestAttemptPodResize_ResizeInProgressWithError_FallsBackToDelete(t *testing.T) {
	rf, mrfc, mrfh := setupMasterResizeTest()
	mrfc.On("PodResourcesMatchDesired", testMasterPod, rf).Once().Return(true, nil)
	mrfc.On("GetPodResizeCondition", testMasterPod, rf).Once().Return(resizeCondition(corev1.PodResizeInProgress, corev1.PodReasonError, 0), nil)
	mrfh.On("DeletePod", testMasterPod, rf).Once().Return(nil)

	err := newResizeTestHandler(rf, mrfc, mrfh).UpdateRedisesPods(rf)

	assert.NoError(t, err)
	mrfc.AssertExpectations(t)
	mrfh.AssertExpectations(t)
}

// TestAttemptPodResize_ResizeInProgressExceedsTimeout_FallsBackToDelete guards the backstop for
// a resize that the kubelet can seemingly never finish or fail cleanly (e.g. a fractional-byte
// memory target that can never actually be actuated) - without this, PodResizeInProgress with no
// error would wait forever. LastTransitionTime, not any operator-side bookkeeping, is what ages
// out here.
func TestAttemptPodResize_ResizeInProgressExceedsTimeout_FallsBackToDelete(t *testing.T) {
	rf, mrfc, mrfh := setupMasterResizeTest()
	mrfc.On("PodResourcesMatchDesired", testMasterPod, rf).Once().Return(true, nil)
	mrfc.On("GetPodResizeCondition", testMasterPod, rf).Once().Return(resizeCondition(corev1.PodResizeInProgress, "", 10*time.Minute), nil)
	mrfh.On("DeletePod", testMasterPod, rf).Once().Return(nil)

	err := newResizeTestHandler(rf, mrfc, mrfh).UpdateRedisesPods(rf)

	assert.NoError(t, err)
	mrfc.AssertExpectations(t)
	mrfh.AssertExpectations(t)
}

func TestAttemptPodResize_AlreadySubmitted_Success_RelabelsRevision(t *testing.T) {
	rf, mrfc, mrfh := setupMasterResizeTest()
	mrfc.On("PodResourcesMatchDesired", testMasterPod, rf).Once().Return(true, nil)
	mrfc.On("GetPodResizeCondition", testMasterPod, rf).Once().Return((*corev1.PodCondition)(nil), nil)
	mrfh.On("RelabelPodRevision", testMasterPod, rf, testSSVersion).Once().Return(nil)

	err := newResizeTestHandler(rf, mrfc, mrfh).UpdateRedisesPods(rf)

	assert.NoError(t, err)
	mrfc.AssertExpectations(t)
	mrfh.AssertExpectations(t)
}

// TestUpdateRedisesPods_ResizePolicyUnset_NeverAttemptsResize proves the CRD-field opt-in gate
// is checked first and short-circuits before any API calls - even though the pending change
// would otherwise be resource-only, a RedisFailover without ResizePolicy set must always
// delete/recreate, and IsPodResourceOnlyChange must never even be called.
func TestUpdateRedisesPods_ResizePolicyUnset_NeverAttemptsResize(t *testing.T) {
	rf := generateRF(false, false, false)
	// rf.Spec.Redis.ResizePolicy deliberately left unset.
	mrfc := &mRFService.RedisFailoverCheck{}
	mrfh := &mRFService.RedisFailoverHeal{}

	mrfc.On("GetRedisesIPs", rf).Once().Return([]string{"1.1.1.1"}, nil)
	mrfc.On("GetMasterIP", rf).Once().Return("1.1.1.1", nil)
	mrfc.On("GetStatefulSetUpdateRevision", rf).Once().Return(testSSVersion, nil)
	mrfc.On("GetRedisesSlavesPods", rf).Once().Return([]string{}, nil)
	mrfc.On("GetRedisesMasterPod", rf).Once().Return(testMasterPod, nil)
	mrfc.On("GetRedisRevisionHash", testMasterPod, rf).Once().Return("old-revision", nil)
	mrfh.On("DeletePod", testMasterPod, rf).Once().Return(nil)
	// IsPodResourceOnlyChange/PodResourcesMatchDesired/GetPodResizeCondition deliberately not
	// stubbed - attemptPodResize must never be reached, and the gate must short-circuit before
	// even checking whether the pod's own diff is resource-only.

	err := newResizeTestHandler(rf, mrfc, mrfh).UpdateRedisesPods(rf)

	assert.NoError(t, err)
	mrfc.AssertExpectations(t)
	mrfh.AssertExpectations(t)
}

// TestUpdateRedisesPods_NotResourceOnlyForThisPod_FallsBackToDelete guards the fix for a real
// production bug: IsPodResourceOnlyChange is checked per pod, per call, rather than relying on a
// single cached StatefulSet-level signal. A previous design cached "was the last StatefulSet
// write resource-only" once per reconcile and reused it for every pod - which goes stale as
// soon as the StatefulSet itself stops changing (each remaining stale pod is then, incorrectly,
// always treated as resource-only, regardless of what its own pending diff actually contains).
// Here, even though ResizePolicy is set on the CR, this pod's own diff is confirmed
// non-resource-only, so it must fall back to delete rather than attempt a resize.
func TestUpdateRedisesPods_NotResourceOnlyForThisPod_FallsBackToDelete(t *testing.T) {
	rf := generateRF(false, false, false)
	rf.Spec.Redis.ResizePolicy = testResizePolicy()
	mrfc := &mRFService.RedisFailoverCheck{}
	mrfh := &mRFService.RedisFailoverHeal{}

	mrfc.On("GetRedisesIPs", rf).Once().Return([]string{"1.1.1.1"}, nil)
	mrfc.On("GetMasterIP", rf).Once().Return("1.1.1.1", nil)
	mrfc.On("GetStatefulSetUpdateRevision", rf).Once().Return(testSSVersion, nil)
	mrfc.On("GetRedisesSlavesPods", rf).Once().Return([]string{}, nil)
	mrfc.On("GetRedisesMasterPod", rf).Once().Return(testMasterPod, nil)
	mrfc.On("GetRedisRevisionHash", testMasterPod, rf).Once().Return("old-revision", nil)
	mrfc.On("IsPodResourceOnlyChange", "old-revision", rf).Once().Return(false, nil)
	mrfh.On("DeletePod", testMasterPod, rf).Once().Return(nil)
	// PodResourcesMatchDesired/GetPodResizeCondition deliberately not stubbed - attemptPodResize
	// must never be reached once IsPodResourceOnlyChange reports false for this pod.

	err := newResizeTestHandler(rf, mrfc, mrfh).UpdateRedisesPods(rf)

	assert.NoError(t, err)
	mrfc.AssertExpectations(t)
	mrfh.AssertExpectations(t)
}

// TestUpdateRedisesPods_TransientResizeEligibilityError_SkipsWithoutDeleteOrError guards the
// fix for treating a transient failure the same as a confirmed "not resizable": a timeout or
// throttling error while checking eligibility is genuinely unknown, not a confirmed answer, so
// it must skip this pod for the reconcile (no delete, no resize) and let it be re-evaluated
// fresh next time - rather than forcing a disruptive delete (a Sentinel failover, for the
// master) over a blip unrelated to the pod's actual state, and rather than returning the error
// and aborting the reconcile pass outright.
func TestUpdateRedisesPods_TransientResizeEligibilityError_SkipsWithoutDeleteOrError(t *testing.T) {
	rf := generateRF(false, false, false)
	rf.Spec.Redis.ResizePolicy = testResizePolicy()
	mrfc := &mRFService.RedisFailoverCheck{}
	mrfh := &mRFService.RedisFailoverHeal{}

	mrfc.On("GetRedisesIPs", rf).Once().Return([]string{"1.1.1.1"}, nil)
	mrfc.On("GetMasterIP", rf).Once().Return("1.1.1.1", nil)
	mrfc.On("GetStatefulSetUpdateRevision", rf).Once().Return(testSSVersion, nil)
	mrfc.On("GetRedisesSlavesPods", rf).Once().Return([]string{}, nil)
	mrfc.On("GetRedisesMasterPod", rf).Once().Return(testMasterPod, nil)
	mrfc.On("GetRedisRevisionHash", testMasterPod, rf).Once().Return("old-revision", nil)
	mrfc.On("IsPodResourceOnlyChange", "old-revision", rf).Once().Return(false, errors.New("etcdserver: request timed out"))
	// Neither DeletePod nor ResizePod stubbed - either being called fails the test.

	err := newResizeTestHandler(rf, mrfc, mrfh).UpdateRedisesPods(rf)

	assert.NoError(t, err)
	mrfc.AssertExpectations(t)
	mrfh.AssertExpectations(t)
}

// TestAttemptPodResize_SlaveUsesIdenticalLogicToMaster documents that the old
// allowEviction/timeout asymmetry between master and slave is gone - a Deferred slave resize
// now falls back to delete exactly like a Deferred master resize, via the same shared function.
func TestAttemptPodResize_SlaveUsesIdenticalLogicToMaster(t *testing.T) {
	rf, mrfc, mrfh := setupSlaveResizeTest()
	mrfc.On("PodResourcesMatchDesired", testSlavePod, rf).Once().Return(true, nil)
	mrfc.On("GetPodResizeCondition", testSlavePod, rf).Once().Return(resizeCondition(corev1.PodResizePending, corev1.PodReasonDeferred, 0), nil)
	mrfh.On("DeletePod", testSlavePod, rf).Once().Return(nil)

	err := newResizeTestHandler(rf, mrfc, mrfh).UpdateRedisesPods(rf)

	assert.NoError(t, err)
	mrfc.AssertExpectations(t)
	mrfh.AssertExpectations(t)
}
