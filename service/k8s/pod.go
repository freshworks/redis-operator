package k8s

import (
	"context"
	"encoding/json"

	"k8s.io/apimachinery/pkg/types"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/freshworks/redis-operator/log"
	"github.com/freshworks/redis-operator/metrics"
)

// Pod the ServiceAccount service that knows how to interact with k8s to manage them
type Pod interface {
	GetPod(namespace string, name string) (*corev1.Pod, error)
	CreatePod(namespace string, pod *corev1.Pod) error
	UpdatePod(namespace string, pod *corev1.Pod) error
	CreateOrUpdatePod(namespace string, pod *corev1.Pod) error
	DeletePod(namespace string, name string) error
	ListPods(namespace string) (*corev1.PodList, error)
	UpdatePodLabels(namespace, podName string, labels map[string]string) error
	UpdatePodAnnotations(namespace, podName string, annotations map[string]string) error
	RemovePodAnnotation(namespace, podName string, annotationKey string) error
}

// PodService is the pod service implementation using API calls to kubernetes.
type PodService struct {
	kubeClient      kubernetes.Interface
	logger          log.Logger
	metricsRecorder metrics.Recorder
}

// NewPodService returns a new Pod KubeService.
func NewPodService(kubeClient kubernetes.Interface, logger log.Logger, metricsRecorder metrics.Recorder) *PodService {
	logger = logger.With("service", "k8s.pod")
	return &PodService{
		kubeClient:      kubeClient,
		logger:          logger,
		metricsRecorder: metricsRecorder,
	}
}

func (p *PodService) GetPod(namespace string, name string) (*corev1.Pod, error) {
	pod, err := p.kubeClient.CoreV1().Pods(namespace).Get(context.TODO(), name, metav1.GetOptions{})
	recordMetrics(namespace, "Pod", name, "GET", err, p.metricsRecorder)
	if err != nil {
		return nil, err
	}
	return pod, err
}

func (p *PodService) CreatePod(namespace string, pod *corev1.Pod) error {
	_, err := p.kubeClient.CoreV1().Pods(namespace).Create(context.TODO(), pod, metav1.CreateOptions{})
	recordMetrics(namespace, "Pod", pod.GetName(), "CREATE", err, p.metricsRecorder)
	if err != nil {
		return err
	}
	p.logger.WithField("namespace", namespace).WithField("pod", pod.Name).Debugf("pod created")
	return nil
}
func (p *PodService) UpdatePod(namespace string, pod *corev1.Pod) error {
	_, err := p.kubeClient.CoreV1().Pods(namespace).Update(context.TODO(), pod, metav1.UpdateOptions{})
	recordMetrics(namespace, "Pod", pod.GetName(), "UPDATE", err, p.metricsRecorder)
	if err != nil {
		return err
	}
	p.logger.WithField("namespace", namespace).WithField("pod", pod.Name).Debugf("pod updated")
	return nil
}
func (p *PodService) CreateOrUpdatePod(namespace string, pod *corev1.Pod) error {
	storedPod, err := p.GetPod(namespace, pod.Name)
	if err != nil {
		// If no resource we need to create.
		if errors.IsNotFound(err) {
			return p.CreatePod(namespace, pod)
		}
		return err
	}

	// Already exists, need to Update.
	// Set the correct resource version to ensure we are on the latest version. This way the only valid
	// namespace is our spec(https://github.com/kubernetes/community/blob/master/contributors/devel/api-conventions.md#concurrency-control-and-consistency),
	// we will replace the current namespace state.
	pod.ResourceVersion = storedPod.ResourceVersion
	return p.UpdatePod(namespace, pod)
}

func (p *PodService) DeletePod(namespace string, name string) error {
	err := p.kubeClient.CoreV1().Pods(namespace).Delete(context.TODO(), name, metav1.DeleteOptions{})
	recordMetrics(namespace, "Pod", name, "DELETE", err, p.metricsRecorder)
	return err
}

func (p *PodService) ListPods(namespace string) (*corev1.PodList, error) {
	pods, err := p.kubeClient.CoreV1().Pods(namespace).List(context.TODO(), metav1.ListOptions{})
	recordMetrics(namespace, "Pod", metrics.NOT_APPLICABLE, "LIST", err, p.metricsRecorder)
	return pods, err
}

// PatchStringValue specifies a patch operation for a string.
type PatchStringValue struct {
	Op    string      `json:"op"`
	Path  string      `json:"path"`
	Value interface{} `json:"value"`
}

func (p *PodService) UpdatePodLabels(namespace, podName string, labels map[string]string) error {
	p.logger.Infof("Update pod label, namespace: %s, pod name: %s, labels: %v", namespace, podName, labels)

	var payloads []interface{}
	for labelKey, labelValue := range labels {
		payload := PatchStringValue{
			Op:    "replace",
			Path:  "/metadata/labels/" + labelKey,
			Value: labelValue,
		}
		payloads = append(payloads, payload)
	}
	payloadBytes, _ := json.Marshal(payloads)

	_, err := p.kubeClient.CoreV1().Pods(namespace).Patch(context.TODO(), podName, types.JSONPatchType, payloadBytes, metav1.PatchOptions{})

	recordMetrics(namespace, "Pod", podName, "PATCH", err, p.metricsRecorder)
	if err != nil {
		p.logger.Errorf("Update pod labels failed, namespace: %s, pod name: %s, error: %v", namespace, podName, err)
	}
	return err
}

// modifyPodAnnotations is a helper function that gets a pod, modifies its annotations using the provided function, and updates it
func (p *PodService) modifyPodAnnotations(namespace, podName, operation string, modifyFunc func(map[string]string)) error {
	// Get the current pod
	pod, err := p.kubeClient.CoreV1().Pods(namespace).Get(context.TODO(), podName, metav1.GetOptions{})
	if err != nil {
		p.logger.Errorf("Failed to get pod %s in namespace %s: %v", podName, namespace, err)
		recordMetrics(namespace, "Pod", podName, "GET", err, p.metricsRecorder)
		return err
	}

	// Initialize annotations map if it doesn't exist
	if pod.Annotations == nil {
		pod.Annotations = make(map[string]string)
	}

	// Apply the modification function
	modifyFunc(pod.Annotations)

	// Update the pod
	_, err = p.kubeClient.CoreV1().Pods(namespace).Update(context.TODO(), pod, metav1.UpdateOptions{})
	recordMetrics(namespace, "Pod", podName, "UPDATE", err, p.metricsRecorder)
	if err != nil {
		p.logger.Errorf("%s pod annotations failed, namespace: %s, pod name: %s, error: %v", operation, namespace, podName, err)
	}
	return err
}

func (p *PodService) UpdatePodAnnotations(namespace, podName string, annotations map[string]string) error {
	p.logger.Infof("Update pod annotation, namespace: %s, pod name: %s, annotations: %v", namespace, podName, annotations)

	return p.modifyPodAnnotations(namespace, podName, "Update", func(podAnnotations map[string]string) {
		// Add/update the annotations
		for annotationKey, annotationValue := range annotations {
			podAnnotations[annotationKey] = annotationValue
		}
	})
}

func (p *PodService) RemovePodAnnotation(namespace, podName string, annotationKey string) error {
	p.logger.Infof("Remove pod annotation, namespace: %s, pod name: %s, annotation key: %s", namespace, podName, annotationKey)

	return p.modifyPodAnnotations(namespace, podName, "Remove", func(podAnnotations map[string]string) {
		// Remove the annotation if it exists
		delete(podAnnotations, annotationKey)
	})
}
