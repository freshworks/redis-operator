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

// resizeCondition builds a pod resize condition that transitioned age ago, for exercising
// resizeInProgressTimeout.
func resizeCondition(condType corev1.PodConditionType, reason string, age time.Duration) *corev1.PodCondition {
	return &corev1.PodCondition{
		Type:               condType,
		Status:             corev1.ConditionTrue,
		Reason:             reason,
		LastTransitionTime: metav1.NewTime(time.Now().Add(-age)),
	}
}

// Covers attemptPodResize's branches via UpdateRedisesPods, so the real master/slave call sites
// get exercised, not just the function in isolation.

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

// setupMasterResizeTest wires up mocks to reach attemptPodResize for the master pod.
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

// PodResourcesMatchDesired can read true before the kubelet finishes actuating - must wait, not
// relabel, while PodResizeInProgress with no error is within the timeout.
func TestAttemptPodResize_ResizeInProgress_WaitsRatherThanRelabeling(t *testing.T) {
	rf, mrfc, mrfh := setupMasterResizeTest()
	mrfc.On("PodResourcesMatchDesired", testMasterPod, rf).Once().Return(true, nil)
	mrfc.On("GetPodResizeCondition", testMasterPod, rf).Once().Return(resizeCondition(corev1.PodResizeInProgress, "", 30*time.Second), nil)
	// RelabelPodRevision/DeletePod deliberately not stubbed - neither must be called.

	err := newResizeTestHandler(rf, mrfc, mrfh).UpdateRedisesPods(rf)

	assert.NoError(t, err)
	mrfc.AssertExpectations(t)
	mrfh.AssertExpectations(t)
}

// An actuation error is not something to keep waiting on.
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

// Bounds a resize the kubelet never resolves (e.g. a target it can never actually actuate).
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

// The opt-in gate short-circuits before any resource-only check when ResizePolicy is unset.
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
	// stubbed - must never be called.

	err := newResizeTestHandler(rf, mrfc, mrfh).UpdateRedisesPods(rf)

	assert.NoError(t, err)
	mrfc.AssertExpectations(t)
	mrfh.AssertExpectations(t)
}

// Even with ResizePolicy set, a pod whose own diff isn't resource-only falls back to delete.
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

// A transient eligibility-check error skips the pod this reconcile - no delete, no error.
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

// Master and slave share identical logic - no per-role asymmetry.
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
