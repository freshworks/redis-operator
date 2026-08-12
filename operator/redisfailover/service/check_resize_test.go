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

// resourceListMatches backs PodResourcesMatchDesired - tolerant of LimitRange-injected extras,
// but not of a cpu/memory value the CR used to ask for and has since removed.

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

// A memory limit dropped from the CR but still present on the live pod must not read as a match.
func TestResourceListMatches_MemoryRemovedFromDesiredButStillOnActual_ReportsMismatch(t *testing.T) {
	actual := corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("2Gi")}
	desired := corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")}

	assert.False(t, resourceListMatches(actual, desired))
}

// IsPodResourceOnlyChange diffs a ControllerRevision snapshot against the current template.

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

// newTestControllerRevision matches the real Data.Raw shape the StatefulSet controller writes.
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

// A pruned revision (beyond RevisionHistoryLimit) falls back to delete, not an error.
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

// Unlike a pruned revision, a transient error (timeout, throttling) must surface as a real
// error, not resolve to false - the caller retries instead of deleting over a passing blip.

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
