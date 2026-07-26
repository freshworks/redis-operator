package k8s

import (
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func basePodSpec() *corev1.PodSpec {
	return &corev1.PodSpec{
		Containers: []corev1.Container{
			{
				Name:  "redis",
				Image: "redis:7",
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("2Gi")},
				},
			},
			{
				Name:  "redis-exporter",
				Image: "redis-exporter:1",
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("50Mi")},
				},
			},
		},
	}
}

func TestIsResourceOnlyChange_ResourcesOnlyDiffers(t *testing.T) {
	oldSpec := basePodSpec()
	newSpec := basePodSpec()
	newSpec.Containers[0].Resources.Requests[corev1.ResourceMemory] = resource.MustParse("4Gi")

	assert.True(t, isResourceOnlyChange(oldSpec, newSpec, "redis"))
}

func TestIsResourceOnlyChange_NoDiffAtAll(t *testing.T) {
	oldSpec := basePodSpec()
	newSpec := basePodSpec()

	assert.True(t, isResourceOnlyChange(oldSpec, newSpec, "redis"))
}

func TestIsResourceOnlyChange_ImageAlsoChanged(t *testing.T) {
	oldSpec := basePodSpec()
	newSpec := basePodSpec()
	newSpec.Containers[0].Resources.Requests[corev1.ResourceMemory] = resource.MustParse("4Gi")
	newSpec.Containers[0].Image = "redis:8"

	assert.False(t, isResourceOnlyChange(oldSpec, newSpec, "redis"))
}

func TestIsResourceOnlyChange_CPUOnlyDiffers(t *testing.T) {
	oldSpec := basePodSpec()
	newSpec := basePodSpec()
	newSpec.Containers[0].Resources.Requests[corev1.ResourceCPU] = resource.MustParse("500m")

	assert.True(t, isResourceOnlyChange(oldSpec, newSpec, "redis"))
}

func TestIsResourceOnlyChange_EnvVarAlsoChanged(t *testing.T) {
	oldSpec := basePodSpec()
	newSpec := basePodSpec()
	newSpec.Containers[0].Resources.Requests[corev1.ResourceMemory] = resource.MustParse("4Gi")
	newSpec.Containers[0].Env = []corev1.EnvVar{{Name: "FOO", Value: "bar"}}

	assert.False(t, isResourceOnlyChange(oldSpec, newSpec, "redis"))
}

// TestIsResourceOnlyChange_OnlyExporterResourcesChanged guards the exporter-sidecar gap found
// in review: the in-place resize path only knows how to resize the "redis" container, so a
// resource-only change to any other container (e.g. the exporter sidecar) must NOT be
// classified as resize-only - it needs to fall through to the delete-based path instead,
// since resizing "redis" alone would silently leave the exporter container's resources
// unapplied while still being reported as a successful resize.
func TestIsResourceOnlyChange_OnlyExporterResourcesChanged(t *testing.T) {
	oldSpec := basePodSpec()
	newSpec := basePodSpec()
	newSpec.Containers[1].Resources.Requests[corev1.ResourceMemory] = resource.MustParse("100Mi")

	assert.False(t, isResourceOnlyChange(oldSpec, newSpec, "redis"))
}

// TestIsResourceOnlyChange_RedisAndExporterBothChanged: even though the redis container's own
// resource change would be resize-only in isolation, a simultaneous exporter resource change
// must still block the classification, since only "redis" can actually be resized.
func TestIsResourceOnlyChange_RedisAndExporterBothChanged(t *testing.T) {
	oldSpec := basePodSpec()
	newSpec := basePodSpec()
	newSpec.Containers[0].Resources.Requests[corev1.ResourceMemory] = resource.MustParse("4Gi")
	newSpec.Containers[1].Resources.Requests[corev1.ResourceMemory] = resource.MustParse("100Mi")

	assert.False(t, isResourceOnlyChange(oldSpec, newSpec, "redis"))
}
