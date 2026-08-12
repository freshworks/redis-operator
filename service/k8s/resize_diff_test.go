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

	assert.True(t, IsResourceOnlyChange(oldSpec, newSpec, "redis"))
}

func TestIsResourceOnlyChange_NoDiffAtAll(t *testing.T) {
	oldSpec := basePodSpec()
	newSpec := basePodSpec()

	assert.True(t, IsResourceOnlyChange(oldSpec, newSpec, "redis"))
}

func TestIsResourceOnlyChange_ImageAlsoChanged(t *testing.T) {
	oldSpec := basePodSpec()
	newSpec := basePodSpec()
	newSpec.Containers[0].Resources.Requests[corev1.ResourceMemory] = resource.MustParse("4Gi")
	newSpec.Containers[0].Image = "redis:8"

	assert.False(t, IsResourceOnlyChange(oldSpec, newSpec, "redis"))
}

func TestIsResourceOnlyChange_CPUOnlyDiffers(t *testing.T) {
	oldSpec := basePodSpec()
	newSpec := basePodSpec()
	newSpec.Containers[0].Resources.Requests[corev1.ResourceCPU] = resource.MustParse("500m")

	assert.True(t, IsResourceOnlyChange(oldSpec, newSpec, "redis"))
}

func TestIsResourceOnlyChange_EnvVarAlsoChanged(t *testing.T) {
	oldSpec := basePodSpec()
	newSpec := basePodSpec()
	newSpec.Containers[0].Resources.Requests[corev1.ResourceMemory] = resource.MustParse("4Gi")
	newSpec.Containers[0].Env = []corev1.EnvVar{{Name: "FOO", Value: "bar"}}

	assert.False(t, IsResourceOnlyChange(oldSpec, newSpec, "redis"))
}

// A resource-only change to a non-"redis" container (e.g. the exporter) must not be classified
// as resize-only, since only "redis" can actually be resized.
func TestIsResourceOnlyChange_OnlyExporterResourcesChanged(t *testing.T) {
	oldSpec := basePodSpec()
	newSpec := basePodSpec()
	newSpec.Containers[1].Resources.Requests[corev1.ResourceMemory] = resource.MustParse("100Mi")

	assert.False(t, IsResourceOnlyChange(oldSpec, newSpec, "redis"))
}

// A simultaneous exporter change still blocks classification even if redis's own change alone
// would have been resize-only.
func TestIsResourceOnlyChange_RedisAndExporterBothChanged(t *testing.T) {
	oldSpec := basePodSpec()
	newSpec := basePodSpec()
	newSpec.Containers[0].Resources.Requests[corev1.ResourceMemory] = resource.MustParse("4Gi")
	newSpec.Containers[1].Resources.Requests[corev1.ResourceMemory] = resource.MustParse("100Mi")

	assert.False(t, IsResourceOnlyChange(oldSpec, newSpec, "redis"))
}
