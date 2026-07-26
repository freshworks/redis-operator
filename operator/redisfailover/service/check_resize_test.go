package service

import (
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
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

func TestResourceListMatches_EmptyDesiredAlwaysMatches(t *testing.T) {
	actual := corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("2Gi")}
	desired := corev1.ResourceList{}

	assert.True(t, resourceListMatches(actual, desired))
}
