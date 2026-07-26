package redisfailover_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/freshworks/redis-operator/log"
	"github.com/freshworks/redis-operator/metrics"
	redisfailoverv1 "github.com/freshworks/redis-operator/api/redisfailover/v1"
	mRFService "github.com/freshworks/redis-operator/mocks/operator/redisfailover/service"
	mK8SService "github.com/freshworks/redis-operator/mocks/service/k8s"
	rfOperator "github.com/freshworks/redis-operator/operator/redisfailover"
)

// This file covers attemptPodResize's branches (checker.go), reached via UpdateRedisesPods so
// the real master/slave call sites - including the allowEviction wiring - are exercised, not
// just the function in isolation. Prior to this, none of these branches had any Go test
// coverage; only the manual EKS testing described in the PR body covered them.

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

// setupMasterResizeTest wires up the mocks needed to reach attemptPodResize for the master pod
// with a resize-only change already detected - a single redis IP (so the ready-check loop has
// no slaves to check), one stale master revision, no stale slaves.
func setupMasterResizeTest() (*redisfailoverv1.RedisFailover, *mRFService.RedisFailoverCheck, *mRFService.RedisFailoverHeal) {
	rf := generateRF(false, false, false)
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
// UpdateRedisesPods reaches attemptPodResize with allowEviction=false before ever considering
// the master.
func setupSlaveResizeTest() (*redisfailoverv1.RedisFailover, *mRFService.RedisFailoverCheck, *mRFService.RedisFailoverHeal) {
	rf := generateRF(false, false, false)
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

func TestAttemptPodResize_FirstAttempt(t *testing.T) {
	rf, mrfc, mrfh := setupMasterResizeTest()
	mrfc.On("GetResizeState", testMasterPod, rf).Once().Return(time.Time{}, "", false, nil)
	mrfh.On("SetResizeStartedAt", testMasterPod, rf, testSSVersion).Once().Return(nil)
	mrfh.On("ResizePod", testMasterPod, rf).Once().Return(nil)

	err := newResizeTestHandler(rf, mrfc, mrfh).UpdateRedisesPods(rf)

	assert.NoError(t, err)
	mrfc.AssertExpectations(t)
	mrfh.AssertExpectations(t)
}

// TestAttemptPodResize_StaleTargetRevisionTreatedAsFresh guards the fix for a leftover
// resize-started-at annotation from an already-resolved attempt: even though the tracked
// startedAt is old enough to exceed any timeout, a targetRevision that doesn't match the
// current ssUR must be treated as a fresh attempt (SetResizeStartedAt + ResizePod), not sent
// straight to DeletePod.
func TestAttemptPodResize_StaleTargetRevisionTreatedAsFresh(t *testing.T) {
	rf, mrfc, mrfh := setupMasterResizeTest()
	longAgo := time.Now().Add(-20 * time.Minute)
	mrfc.On("GetResizeState", testMasterPod, rf).Once().Return(longAgo, "some-older-revision", true, nil)
	mrfh.On("SetResizeStartedAt", testMasterPod, rf, testSSVersion).Once().Return(nil)
	mrfh.On("ResizePod", testMasterPod, rf).Once().Return(nil)

	err := newResizeTestHandler(rf, mrfc, mrfh).UpdateRedisesPods(rf)

	assert.NoError(t, err)
	mrfc.AssertExpectations(t)
	mrfh.AssertExpectations(t)
}

func TestAttemptPodResize_TimeoutFallsBackToDelete(t *testing.T) {
	rf, mrfc, mrfh := setupMasterResizeTest()
	// masterResizeTimeout is 6 minutes; 10 minutes ago exceeds it.
	tenMinutesAgo := time.Now().Add(-10 * time.Minute)
	mrfc.On("GetResizeState", testMasterPod, rf).Once().Return(tenMinutesAgo, testSSVersion, true, nil)
	mrfh.On("ClearResizeState", testMasterPod, rf).Once().Return(nil)
	mrfh.On("DeletePod", testMasterPod, rf).Once().Return(nil)

	err := newResizeTestHandler(rf, mrfc, mrfh).UpdateRedisesPods(rf)

	assert.NoError(t, err)
	mrfc.AssertExpectations(t)
	mrfh.AssertExpectations(t)
}

func TestAttemptPodResize_NoConditionAndResourcesMatch_Succeeds(t *testing.T) {
	rf, mrfc, mrfh := setupMasterResizeTest()
	recently := time.Now().Add(-1 * time.Minute)
	mrfc.On("GetResizeState", testMasterPod, rf).Once().Return(recently, testSSVersion, true, nil)
	mrfc.On("GetPodResizeCondition", testMasterPod, rf).Once().Return(false, "", nil)
	mrfc.On("PodResourcesMatchDesired", testMasterPod, rf).Once().Return(true, nil)
	mrfh.On("RelabelPodRevision", testMasterPod, rf, testSSVersion).Once().Return(nil)
	mrfh.On("ClearResizeState", testMasterPod, rf).Once().Return(nil)

	err := newResizeTestHandler(rf, mrfc, mrfh).UpdateRedisesPods(rf)

	assert.NoError(t, err)
	mrfc.AssertExpectations(t)
	mrfh.AssertExpectations(t)
}

// TestAttemptPodResize_NoConditionButResourcesMismatch_RetriesRatherThanSucceeding guards the
// fix for treating "no PodResizePending condition" as sufficient proof of success on its own -
// it's equally true when the resize call never reached the pod at all (e.g. an RBAC
// rejection). Resources not actually matching must trigger a retry, not RelabelPodRevision.
func TestAttemptPodResize_NoConditionButResourcesMismatch_RetriesRatherThanSucceeding(t *testing.T) {
	rf, mrfc, mrfh := setupMasterResizeTest()
	recently := time.Now().Add(-1 * time.Minute)
	mrfc.On("GetResizeState", testMasterPod, rf).Once().Return(recently, testSSVersion, true, nil)
	mrfc.On("GetPodResizeCondition", testMasterPod, rf).Once().Return(false, "", nil)
	mrfc.On("PodResourcesMatchDesired", testMasterPod, rf).Once().Return(false, nil)
	mrfh.On("ResizePod", testMasterPod, rf).Once().Return(nil)

	err := newResizeTestHandler(rf, mrfc, mrfh).UpdateRedisesPods(rf)

	assert.NoError(t, err)
	mrfc.AssertExpectations(t)
	mrfh.AssertExpectations(t)
	// RelabelPodRevision/ClearResizeState must NOT have been called - asserted implicitly:
	// mrfh has no stub for them, so testify would panic on an unexpected call.
}

func TestAttemptPodResize_Infeasible_FallsBackToDelete(t *testing.T) {
	rf, mrfc, mrfh := setupMasterResizeTest()
	recently := time.Now().Add(-1 * time.Minute)
	mrfc.On("GetResizeState", testMasterPod, rf).Once().Return(recently, testSSVersion, true, nil)
	mrfc.On("GetPodResizeCondition", testMasterPod, rf).Once().Return(true, corev1.PodReasonInfeasible, nil)
	mrfh.On("ClearResizeState", testMasterPod, rf).Once().Return(nil)
	mrfh.On("DeletePod", testMasterPod, rf).Once().Return(nil)

	err := newResizeTestHandler(rf, mrfc, mrfh).UpdateRedisesPods(rf)

	assert.NoError(t, err)
	mrfc.AssertExpectations(t)
	mrfh.AssertExpectations(t)
}

func TestAttemptPodResize_MasterDeferred_TriesEvictionWhenHeadroomNeeded(t *testing.T) {
	rf, mrfc, mrfh := setupMasterResizeTest()
	recently := time.Now().Add(-1 * time.Minute)
	mrfc.On("GetResizeState", testMasterPod, rf).Once().Return(recently, testSSVersion, true, nil)
	mrfc.On("GetPodResizeCondition", testMasterPod, rf).Once().Return(true, corev1.PodReasonDeferred, nil)
	mrfh.On("ResizePod", testMasterPod, rf).Once().Return(nil)
	mrfc.On("GetPodNode", testMasterPod, rf).Once().Return("node-1", nil)
	mrfc.On("ComputeRequiredHeadroom", rf, "node-1", testMasterPod).Once().Return(resource.MustParse("0"), resource.MustParse("2Gi"), nil)
	mrfh.On("FreeResizeHeadroom", rf, "node-1", mock.Anything, mock.Anything).Once().Return(nil)

	err := newResizeTestHandler(rf, mrfc, mrfh).UpdateRedisesPods(rf)

	assert.NoError(t, err)
	mrfc.AssertExpectations(t)
	mrfh.AssertExpectations(t)
}

// TestAttemptPodResize_MasterDeferred_SkipsEvictionWhenAlreadyFits guards against calling
// FreeResizeHeadroom needlessly: if both CPU and memory headroom are already satisfied
// (Sign() <= 0), the kubelet's own retry should be left to catch up on its own.
func TestAttemptPodResize_MasterDeferred_SkipsEvictionWhenAlreadyFits(t *testing.T) {
	rf, mrfc, mrfh := setupMasterResizeTest()
	recently := time.Now().Add(-1 * time.Minute)
	mrfc.On("GetResizeState", testMasterPod, rf).Once().Return(recently, testSSVersion, true, nil)
	mrfc.On("GetPodResizeCondition", testMasterPod, rf).Once().Return(true, corev1.PodReasonDeferred, nil)
	mrfh.On("ResizePod", testMasterPod, rf).Once().Return(nil)
	mrfc.On("GetPodNode", testMasterPod, rf).Once().Return("node-1", nil)
	mrfc.On("ComputeRequiredHeadroom", rf, "node-1", testMasterPod).Once().Return(resource.MustParse("0"), resource.MustParse("0"), nil)
	// FreeResizeHeadroom deliberately not stubbed - an unexpected call here fails the test.

	err := newResizeTestHandler(rf, mrfc, mrfh).UpdateRedisesPods(rf)

	assert.NoError(t, err)
	mrfc.AssertExpectations(t)
	mrfh.AssertExpectations(t)
}

// TestAttemptPodResize_SlaveDeferred_NeverEvicts guards the allowEviction=false gate: a
// Deferred slave resize must skip eviction entirely (no ResizePod re-issue, no
// ComputeRequiredHeadroom, no FreeResizeHeadroom) and fall straight back to the plain
// delete-based restart a slave would have gotten anyway.
func TestAttemptPodResize_SlaveDeferred_NeverEvicts(t *testing.T) {
	rf, mrfc, mrfh := setupSlaveResizeTest()
	recently := time.Now().Add(-1 * time.Minute)
	mrfc.On("GetResizeState", testSlavePod, rf).Once().Return(recently, testSSVersion, true, nil)
	mrfc.On("GetPodResizeCondition", testSlavePod, rf).Once().Return(true, corev1.PodReasonDeferred, nil)
	mrfh.On("ClearResizeState", testSlavePod, rf).Once().Return(nil)
	mrfh.On("DeletePod", testSlavePod, rf).Once().Return(nil)
	// ResizePod (re-issue), GetPodNode, ComputeRequiredHeadroom, FreeResizeHeadroom
	// deliberately not stubbed - any of them being called would fail the test.

	err := newResizeTestHandler(rf, mrfc, mrfh).UpdateRedisesPods(rf)

	assert.NoError(t, err)
	mrfc.AssertExpectations(t)
	mrfh.AssertExpectations(t)
}
