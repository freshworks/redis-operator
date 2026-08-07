package redisfailover_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	corev1 "k8s.io/api/core/v1"

	redisfailoverv1 "github.com/freshworks/redis-operator/api/redisfailover/v1"
	"github.com/freshworks/redis-operator/log"
	"github.com/freshworks/redis-operator/metrics"
	mRFService "github.com/freshworks/redis-operator/mocks/operator/redisfailover/service"
	mK8SService "github.com/freshworks/redis-operator/mocks/service/k8s"
	rfOperator "github.com/freshworks/redis-operator/operator/redisfailover"
)

// This file covers attemptPodResize's branches (checker.go), reached via UpdateRedisesPods so
// the real master/slave call sites are exercised, not just the function in isolation. Master
// and slave now share identical logic, and there is no annotation-based state: the pod's live
// resources (via PodResourcesMatchDesired) and resize condition are the only state consulted.

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

// setupMasterResizeTest wires up the mocks needed to reach attemptPodResize for the master pod
// with a resize-only change already detected and ResizePolicy set on the CR (the opt-in gate) -
// a single redis IP (so the ready-check loop has no slaves to check), one stale master
// revision, no stale slaves.
func setupMasterResizeTest() (*redisfailoverv1.RedisFailover, *mRFService.RedisFailoverCheck, *mRFService.RedisFailoverHeal) {
	rf := generateRF(false, false, false)
	rf.Spec.Redis.ResizePolicy = testResizePolicy()
	mrfc := &mRFService.RedisFailoverCheck{}
	mrfh := &mRFService.RedisFailoverHeal{}

	mrfc.On("GetRedisesIPs", rf).Once().Return([]string{"1.1.1.1"}, nil)
	mrfc.On("GetMasterIP", rf).Once().Return("1.1.1.1", nil)
	mrfc.On("GetStatefulSetUpdateRevision", rf).Once().Return(testSSVersion, nil)
	mrfc.On("GetStatefulSetResizeOnly", rf).Once().Return(true, nil)
	mrfc.On("GetRedisesSlavesPods", rf).Once().Return([]string{}, nil)
	mrfc.On("GetRedisesMasterPod", rf).Once().Return(testMasterPod, nil)
	mrfc.On("GetRedisRevisionHash", testMasterPod, rf).Once().Return("old-revision", nil)

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
	mrfc.On("GetStatefulSetResizeOnly", rf).Once().Return(true, nil)
	mrfc.On("GetRedisesSlavesPods", rf).Once().Return([]string{testSlavePod}, nil)
	mrfc.On("GetRedisRevisionHash", testSlavePod, rf).Once().Return("old-revision", nil)

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
	mrfc.On("GetPodResizeCondition", testMasterPod, rf).Once().Return(true, corev1.PodReasonInfeasible, nil)
	mrfh.On("DeletePod", testMasterPod, rf).Once().Return(nil)

	err := newResizeTestHandler(rf, mrfc, mrfh).UpdateRedisesPods(rf)

	assert.NoError(t, err)
	mrfc.AssertExpectations(t)
	mrfh.AssertExpectations(t)
}

func TestAttemptPodResize_AlreadySubmitted_Deferred_FallsBackToDelete(t *testing.T) {
	rf, mrfc, mrfh := setupMasterResizeTest()
	mrfc.On("PodResourcesMatchDesired", testMasterPod, rf).Once().Return(true, nil)
	mrfc.On("GetPodResizeCondition", testMasterPod, rf).Once().Return(true, corev1.PodReasonDeferred, nil)
	mrfh.On("DeletePod", testMasterPod, rf).Once().Return(nil)
	// No eviction machinery exists anymore to stub - Deferred goes straight to delete, same as
	// Infeasible, with no waiting.

	err := newResizeTestHandler(rf, mrfc, mrfh).UpdateRedisesPods(rf)

	assert.NoError(t, err)
	mrfc.AssertExpectations(t)
	mrfh.AssertExpectations(t)
}

func TestAttemptPodResize_AlreadySubmitted_Success_RelabelsRevision(t *testing.T) {
	rf, mrfc, mrfh := setupMasterResizeTest()
	mrfc.On("PodResourcesMatchDesired", testMasterPod, rf).Once().Return(true, nil)
	mrfc.On("GetPodResizeCondition", testMasterPod, rf).Once().Return(false, "", nil)
	mrfh.On("RelabelPodRevision", testMasterPod, rf, testSSVersion).Once().Return(nil)

	err := newResizeTestHandler(rf, mrfc, mrfh).UpdateRedisesPods(rf)

	assert.NoError(t, err)
	mrfc.AssertExpectations(t)
	mrfh.AssertExpectations(t)
}

// TestUpdateRedisesPods_ResizePolicyUnset_NeverAttemptsResize proves the CRD-field opt-in gate
// is enforced independently of GetStatefulSetResizeOnly - even though the pending change is
// resource-only, a RedisFailover without ResizePolicy set must always delete/recreate.
func TestUpdateRedisesPods_ResizePolicyUnset_NeverAttemptsResize(t *testing.T) {
	rf, mrfc, mrfh := setupMasterResizeTest()
	rf.Spec.Redis.ResizePolicy = nil
	mrfh.On("DeletePod", testMasterPod, rf).Once().Return(nil)
	// PodResourcesMatchDesired/GetPodResizeCondition deliberately not stubbed - attemptPodResize
	// must never be reached.

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
	mrfc.On("GetPodResizeCondition", testSlavePod, rf).Once().Return(true, corev1.PodReasonDeferred, nil)
	mrfh.On("DeletePod", testSlavePod, rf).Once().Return(nil)

	err := newResizeTestHandler(rf, mrfc, mrfh).UpdateRedisesPods(rf)

	assert.NoError(t, err)
	mrfc.AssertExpectations(t)
	mrfh.AssertExpectations(t)
}
