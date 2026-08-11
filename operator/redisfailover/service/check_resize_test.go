package service

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	redisfailoverv1 "github.com/freshworks/redis-operator/api/redisfailover/v1"
	"github.com/freshworks/redis-operator/log"
	"github.com/freshworks/redis-operator/metrics"
	mK8SService "github.com/freshworks/redis-operator/mocks/service/k8s"
)

// resourceListMatches backs PodResourcesMatchDesired. These tests cover the fix for its
// fragility against namespace LimitRange defaulting: a live pod's Requests/Limits can gain
// extra resource names (e.g. ephemeral-storage) that never appear in the CR spec at all - a
// whole-struct equality check would never match in that case, so the comparison must only
// require the resource names actually present in desired.

func TestResourceListMatches_ExactMatch(t *testing.T) {
	actual := corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("2Gi"), corev1.ResourceCPU: resource.MustParse("1")}
	desired := corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("2Gi"), corev1.ResourceCPU: resource.MustParse("1")}

	assert.True(t, resourceListMatches(actual, desired))
}

func TestResourceListMatches_ExtraKeyOnActualIgnored(t *testing.T) {
	// Simulates a namespace LimitRange injecting a default the CR spec never mentioned.
	actual := corev1.ResourceList{
		corev1.ResourceMemory:           resource.MustParse("2Gi"),
		corev1.ResourceEphemeralStorage: resource.MustParse("1Gi"),
	}
	desired := corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("2Gi")}

	assert.True(t, resourceListMatches(actual, desired))
}

func TestResourceListMatches_DesiredKeyMissingFromActual(t *testing.T) {
	// The resize genuinely hasn't applied yet - a key desired asks for isn't on the pod at all.
	actual := corev1.ResourceList{}
	desired := corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("2Gi")}

	assert.False(t, resourceListMatches(actual, desired))
}

func TestResourceListMatches_ValueMismatch(t *testing.T) {
	actual := corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")}
	desired := corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("2Gi")}

	assert.False(t, resourceListMatches(actual, desired))
}

func TestResourceListMatches_EquivalentQuantitiesDifferentRepresentation(t *testing.T) {
	// "2048Mi" and "2Gi" are numerically equal but constructed from different strings - this
	// guards against a naive comparison that trips on resource.Quantity's cached string form
	// instead of comparing the actual value (the same class of pitfall apiequality.Semantic
	// .DeepEqual exists to avoid, here handled via Quantity.Cmp instead).
	actual := corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("2048Mi")}
	desired := corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("2Gi")}

	assert.True(t, resourceListMatches(actual, desired))
}

func TestResourceListMatches_BothEmptyMatches(t *testing.T) {
	// A container with no requests/limits configured at all on either side (no LimitRange, no
	// CR-level ask) is a genuine match - nothing to compare, nothing stale.
	assert.True(t, resourceListMatches(corev1.ResourceList{}, corev1.ResourceList{}))
}

func TestResourceListMatches_NonResizableExtraKeyStillIgnoredWhenDesiredEmpty(t *testing.T) {
	// Unlike cpu/memory, an unrelated resource type (e.g. LimitRange-injected
	// ephemeral-storage) present only on actual is still tolerated even when desired is
	// entirely empty - this feature only ever manages cpu/memory, so it has no opinion on
	// anything else.
	actual := corev1.ResourceList{corev1.ResourceEphemeralStorage: resource.MustParse("1Gi")}
	desired := corev1.ResourceList{}

	assert.True(t, resourceListMatches(actual, desired))
}

// TestResourceListMatches_MemoryRemovedFromDesiredButStillOnActual_ReportsMismatch guards the
// fix for a real gap: if the CR drops a resource key it used to specify (e.g. removing a memory
// limit), the old desired-keys-only comparison would treat that as an immediate match - since
// desired no longer mentions memory, nothing was ever checked - permanently leaving the pod's
// stale value in place. cpu/memory must now agree on presence, not just on value when present.
func TestResourceListMatches_MemoryRemovedFromDesiredButStillOnActual_ReportsMismatch(t *testing.T) {
	actual := corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("2Gi")}
	desired := corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")}

	assert.False(t, resourceListMatches(actual, desired))
}

// IsPodResourceOnlyChange diffs the ControllerRevision snapshot for a pod's current revision
// against the StatefulSet's current template - both pure template objects, never touched by
// pod-creation-time admission (the scheduler, ServiceAccount token injection, IRSA-style
// webhooks, DefaultTolerationSeconds, etc.), so no noise-tolerance logic is needed at all,
// unlike comparing a live pod's own (admission-mutated) spec would require.

func newTestRedisFailover() *redisfailoverv1.RedisFailover {
	return &redisfailoverv1.RedisFailover{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "testns"},
		Spec: redisfailoverv1.RedisFailoverSpec{
			Redis: redisfailoverv1.RedisSettings{Replicas: 1},
		},
	}
}

func templateSpec(image, memory string) corev1.PodSpec {
	return corev1.PodSpec{
		Containers: []corev1.Container{
			{
				Name:  "redis",
				Image: image,
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse(memory)},
				},
				VolumeMounts: []corev1.VolumeMount{
					{Name: "redis-config", MountPath: "/redis"},
				},
			},
		},
		Volumes: []corev1.Volume{
			{Name: "redis-config", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{}}},
		},
	}
}

// newTestControllerRevision builds a ControllerRevision whose Data.Raw matches the real shape
// the StatefulSet controller writes: a strategic-merge "replace" patch carrying the full
// template verbatim, wrapped under spec.template. The "$patch" key must round-trip harmlessly
// through decodeStatefulSetRevisionTemplate.
func newTestControllerRevision(name, namespace, image, memory string) *appsv1.ControllerRevision {
	type patchEnvelope struct {
		Spec struct {
			Template struct {
				Patch    string            `json:"$patch"`
				Metadata metav1.ObjectMeta `json:"metadata"`
				Spec     corev1.PodSpec    `json:"spec"`
			} `json:"template"`
		} `json:"spec"`
	}
	var envelope patchEnvelope
	envelope.Spec.Template.Patch = "replace"
	envelope.Spec.Template.Spec = templateSpec(image, memory)
	raw, err := json.Marshal(envelope)
	if err != nil {
		panic(err)
	}
	return &appsv1.ControllerRevision{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Data:       runtime.RawExtension{Raw: raw},
	}
}

func TestIsPodResourceOnlyChange_OnlyResourcesDiffer(t *testing.T) {
	rf := newTestRedisFailover()
	ssName := GetRedisName(rf)
	oldRevisionName := ssName + "-oldrevision"
	revision := newTestControllerRevision(oldRevisionName, rf.Namespace, "redis:7", "1Gi")
	ss := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: ssName, Namespace: rf.Namespace},
		Spec:       appsv1.StatefulSetSpec{Template: corev1.PodTemplateSpec{Spec: templateSpec("redis:7", "2Gi")}},
	}
	ms := &mK8SService.Services{}
	ms.On("GetControllerRevision", rf.Namespace, oldRevisionName).Once().Return(revision, nil)
	ms.On("GetStatefulSet", rf.Namespace, ss.Name).Once().Return(ss, nil)
	checker := NewRedisFailoverChecker(ms, nil, log.Dummy, metrics.Dummy)

	got, err := checker.IsPodResourceOnlyChange(oldRevisionName, rf)

	assert.NoError(t, err)
	assert.True(t, got)
}

func TestIsPodResourceOnlyChange_ImageAlsoDiffers_ReportsFalse(t *testing.T) {
	rf := newTestRedisFailover()
	ssName := GetRedisName(rf)
	oldRevisionName := ssName + "-oldrevision"
	revision := newTestControllerRevision(oldRevisionName, rf.Namespace, "redis:7", "1Gi")
	ss := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: ssName, Namespace: rf.Namespace},
		Spec:       appsv1.StatefulSetSpec{Template: corev1.PodTemplateSpec{Spec: templateSpec("redis:7.2", "2Gi")}},
	}
	ms := &mK8SService.Services{}
	ms.On("GetControllerRevision", rf.Namespace, oldRevisionName).Once().Return(revision, nil)
	ms.On("GetStatefulSet", rf.Namespace, ss.Name).Once().Return(ss, nil)
	checker := NewRedisFailoverChecker(ms, nil, log.Dummy, metrics.Dummy)

	got, err := checker.IsPodResourceOnlyChange(oldRevisionName, rf)

	assert.NoError(t, err)
	assert.False(t, got)
}

// TestIsPodResourceOnlyChange_RevisionPruned_ReportsFalseNotError guards against the
// StatefulSet controller having already garbage-collected the pod's revision (beyond
// RevisionHistoryLimit) - there's nothing safe to compare against, so this must fall back to
// delete (false) rather than erroring the whole reconcile.
func TestIsPodResourceOnlyChange_RevisionPruned_ReportsFalseNotError(t *testing.T) {
	rf := newTestRedisFailover()
	oldRevisionName := GetRedisName(rf) + "-longgone"
	ms := &mK8SService.Services{}
	ms.On("GetControllerRevision", rf.Namespace, oldRevisionName).Once().
		Return(nil, k8serrors.NewNotFound(schema.GroupResource{Group: "apps", Resource: "controllerrevisions"}, oldRevisionName))
	checker := NewRedisFailoverChecker(ms, nil, log.Dummy, metrics.Dummy)

	got, err := checker.IsPodResourceOnlyChange(oldRevisionName, rf)

	assert.NoError(t, err)
	assert.False(t, got)
}

// TestIsPodResourceOnlyChange_TransientControllerRevisionError_PropagatesForRetry and
// TestIsPodResourceOnlyChange_TransientStatefulSetError_PropagatesForRetry guard the distinction
// between "permanently unknowable" (pruned revision, malformed data - falls back to false) and
// "transiently unknowable" (a timeout, throttling, a dropped connection - genuinely retryable).
// A transient failure must surface as a real error rather than resolving to (false, nil): the
// caller (UpdateRedisesPods) treats an error here as "skip this pod, retry next reconcile," not
// as "not resizable" - collapsing the two would force a disruptive delete (a Sentinel failover,
// for the master) over a blip that had nothing to do with the pod and would likely have
// resolved on its own by the next reconcile.

func TestIsPodResourceOnlyChange_TransientControllerRevisionError_PropagatesForRetry(t *testing.T) {
	rf := newTestRedisFailover()
	oldRevisionName := GetRedisName(rf) + "-oldrevision"
	ms := &mK8SService.Services{}
	ms.On("GetControllerRevision", rf.Namespace, oldRevisionName).Once().
		Return(nil, errors.New("etcdserver: request timed out"))
	checker := NewRedisFailoverChecker(ms, nil, log.Dummy, metrics.Dummy)

	got, err := checker.IsPodResourceOnlyChange(oldRevisionName, rf)

	assert.Error(t, err)
	assert.False(t, got)
}

func TestIsPodResourceOnlyChange_TransientStatefulSetError_PropagatesForRetry(t *testing.T) {
	rf := newTestRedisFailover()
	ssName := GetRedisName(rf)
	oldRevisionName := ssName + "-oldrevision"
	revision := newTestControllerRevision(oldRevisionName, rf.Namespace, "redis:7", "1Gi")
	ms := &mK8SService.Services{}
	ms.On("GetControllerRevision", rf.Namespace, oldRevisionName).Once().Return(revision, nil)
	ms.On("GetStatefulSet", rf.Namespace, ssName).Once().Return(nil, errors.New("etcdserver: request timed out"))
	checker := NewRedisFailoverChecker(ms, nil, log.Dummy, metrics.Dummy)

	got, err := checker.IsPodResourceOnlyChange(oldRevisionName, rf)

	assert.Error(t, err)
	assert.False(t, got)
}
