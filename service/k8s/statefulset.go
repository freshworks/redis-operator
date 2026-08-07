package k8s

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/labels"

	"github.com/freshworks/redis-operator/operator/redisfailover/util"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/freshworks/redis-operator/log"
	"github.com/freshworks/redis-operator/metrics"
)

// IsResourceOnlyChange reports whether the only difference between oldSpec and newSpec is
// containerName's resource requests/limits, by neutralizing that one field on copies of both
// (only for containerName, not every container) and comparing everything else. Uses
// apiequality.Semantic.DeepEqual rather than reflect.DeepEqual because raw reflection can
// report false differences on resource.Quantity.
//
// Scoped to a single container deliberately: the in-place resize path this feeds into only
// knows how to resize containerName (see ResizePod/PodResourcesMatchDesired). Neutralizing
// every container's Resources here would misclassify a resource-only change to some other
// container (e.g. the exporter sidecar) as resize-only, when this feature can't actually
// apply it - the resize would silently no-op for that container while still being marked
// successful. Requiring every other container to match exactly means such a change correctly
// falls through to the existing delete-based path instead.
//
// Callers must pass two objects that have both already been through API server defaulting
// (e.g. a live Pod's spec and a stored StatefulSet's template spec) - comparing a freshly-built,
// never-submitted Go struct against either would show spurious differences from the defaulting
// gap alone, regardless of what actually changed.
func IsResourceOnlyChange(oldSpec, newSpec *corev1.PodSpec, containerName string) bool {
	oldCopy := oldSpec.DeepCopy()
	newCopy := newSpec.DeepCopy()
	for i := range oldCopy.Containers {
		if oldCopy.Containers[i].Name == containerName {
			oldCopy.Containers[i].Resources = corev1.ResourceRequirements{}
		}
	}
	for i := range newCopy.Containers {
		if newCopy.Containers[i].Name == containerName {
			newCopy.Containers[i].Resources = corev1.ResourceRequirements{}
		}
	}
	return apiequality.Semantic.DeepEqual(oldCopy, newCopy)
}

// StatefulSet the StatefulSet service that knows how to interact with k8s to manage them
type StatefulSet interface {
	GetStatefulSet(namespace, name string) (*appsv1.StatefulSet, error)
	GetStatefulSetPods(namespace, name string) (*corev1.PodList, error)
	CreateStatefulSet(namespace string, statefulSet *appsv1.StatefulSet) error
	UpdateStatefulSet(namespace string, statefulSet *appsv1.StatefulSet) error
	CreateOrUpdateStatefulSet(namespace string, statefulSet *appsv1.StatefulSet) error
	DeleteStatefulSet(namespace string, name string) error
	ListStatefulSets(namespace string) (*appsv1.StatefulSetList, error)
	// GetControllerRevision returns the named ControllerRevision - for a StatefulSet, this is
	// the exact historical template (metadata + PodSpec) some revision of its pods was created
	// from, keyed by the same name every pod on that revision carries in its
	// controller-revision-hash label. Unlike a live pod's own spec, this is a pure template
	// snapshot that never went through pod-creation-time admission (scheduler, ServiceAccount
	// token injection, IRSA-style webhooks, etc.), so it's directly comparable to the
	// StatefulSet's current template with no admission-noise tolerance needed.
	GetControllerRevision(namespace, name string) (*appsv1.ControllerRevision, error)
}

// StatefulSetService is the service account service implementation using API calls to kubernetes.
type StatefulSetService struct {
	kubeClient      kubernetes.Interface
	logger          log.Logger
	metricsRecorder metrics.Recorder
}

// NewStatefulSetService returns a new StatefulSet KubeService.
func NewStatefulSetService(kubeClient kubernetes.Interface, logger log.Logger, metricsRecorder metrics.Recorder) *StatefulSetService {
	logger = logger.With("service", "k8s.statefulSet")
	return &StatefulSetService{
		kubeClient:      kubeClient,
		logger:          logger,
		metricsRecorder: metricsRecorder,
	}
}

// GetStatefulSet will retrieve the requested statefulset based on namespace and name
func (s *StatefulSetService) GetStatefulSet(namespace, name string) (*appsv1.StatefulSet, error) {
	statefulSet, err := s.kubeClient.AppsV1().StatefulSets(namespace).Get(context.TODO(), name, metav1.GetOptions{})
	recordMetrics(namespace, "StatefulSet", name, "GET", err, s.metricsRecorder)
	if err != nil {
		return nil, err
	}
	return statefulSet, err
}

// GetStatefulSetPods will give a list of pods that are managed by the statefulset
func (s *StatefulSetService) GetStatefulSetPods(namespace, name string) (*corev1.PodList, error) {
	statefulSet, err := s.GetStatefulSet(namespace, name)
	if err != nil {
		return nil, err
	}
	labels := []string{}
	for k, v := range statefulSet.Spec.Selector.MatchLabels {
		labels = append(labels, fmt.Sprintf("%s=%s", k, v))
	}
	selector := strings.Join(labels, ",")
	return s.kubeClient.CoreV1().Pods(namespace).List(context.TODO(), metav1.ListOptions{LabelSelector: selector})
}

// CreateStatefulSet will create the given statefulset
func (s *StatefulSetService) CreateStatefulSet(namespace string, statefulSet *appsv1.StatefulSet) error {
	_, err := s.kubeClient.AppsV1().StatefulSets(namespace).Create(context.TODO(), statefulSet, metav1.CreateOptions{})
	recordMetrics(namespace, "StatefulSet", statefulSet.GetName(), "CREATE", err, s.metricsRecorder)
	if err != nil {
		return err
	}
	s.logger.WithField("namespace", namespace).WithField("statefulSet", statefulSet.ObjectMeta.Name).Debugf("statefulSet created")
	return err
}

// UpdateStatefulSet will update the given statefulset
func (s *StatefulSetService) UpdateStatefulSet(namespace string, statefulSet *appsv1.StatefulSet) error {
	_, err := s.kubeClient.AppsV1().StatefulSets(namespace).Update(context.TODO(), statefulSet, metav1.UpdateOptions{})
	recordMetrics(namespace, "StatefulSet", statefulSet.GetName(), "UPDATE", err, s.metricsRecorder)
	if err != nil {
		return err
	}
	s.logger.WithField("namespace", namespace).WithField("statefulSet", statefulSet.ObjectMeta.Name).Debugf("statefulSet updated")
	return err
}

// CreateOrUpdateStatefulSet will update the statefulset or create it if does not exist
func (s *StatefulSetService) CreateOrUpdateStatefulSet(namespace string, statefulSet *appsv1.StatefulSet) error {
	storedStatefulSet, err := s.GetStatefulSet(namespace, statefulSet.Name)
	if err != nil {
		// If no resource we need to create.
		if errors.IsNotFound(err) {
			return s.CreateStatefulSet(namespace, statefulSet)
		}
		return err
	}

	// Already exists, need to Update.
	// Set the correct resource version to ensure we are on the latest version. This way the only valid
	// namespace is our spec(https://github.com/kubernetes/community/blob/master/contributors/devel/api-conventions.md#concurrency-control-and-consistency),
	// we will replace the current namespace state.
	statefulSet.ResourceVersion = storedStatefulSet.ResourceVersion
	// resize pvc
	// 1.Get the data already stored internally
	// 2.Get the desired data
	// 3.Start querying the pvc list when you find data inconsistencies
	// 3.1 Comparison using real pvc capacity and desired data
	// 3.1.1 Update if you find inconsistencies
	// 3.2 Writing successful updates to internal
	// 4. Set to old VolumeClaimTemplates to update.Prevent update error reporting
	// 5. Set to old annotations to update
	annotations := storedStatefulSet.Annotations
	if annotations == nil {
		annotations = map[string]string{
			"storageCapacity": "0",
		}
	}
	storedCapacity, _ := strconv.ParseInt(annotations["storageCapacity"], 0, 64)
	if len(statefulSet.Spec.VolumeClaimTemplates) != 0 {
		stateCapacity := statefulSet.Spec.VolumeClaimTemplates[0].Spec.Resources.Requests.Storage().Value()
		if storedCapacity != stateCapacity {
			rfName := strings.TrimPrefix(storedStatefulSet.Name, "rfr-")
			listOpt := metav1.ListOptions{
				LabelSelector: labels.FormatLabels(
					map[string]string{
						"app.kubernetes.io/component": "redis",
						"app.kubernetes.io/name":      strings.TrimPrefix(storedStatefulSet.Name, "rfr-"),
						"app.kubernetes.io/part-of":   "redis-failover",
					},
				),
			}
			pvcs, err := s.kubeClient.CoreV1().PersistentVolumeClaims(storedStatefulSet.Namespace).List(context.Background(), listOpt)
			if err != nil {
				return err
			}
			updateFailed := false
			realUpdate := false
			for _, pvc := range pvcs.Items {
				realCapacity := pvc.Spec.Resources.Requests.Storage().Value()
				if realCapacity != stateCapacity {
					realUpdate = true
					pvc.Spec.Resources.Requests = statefulSet.Spec.VolumeClaimTemplates[0].Spec.Resources.Requests
					_, err = s.kubeClient.CoreV1().PersistentVolumeClaims(storedStatefulSet.Namespace).Update(context.Background(), &pvc, metav1.UpdateOptions{})
					if err != nil {
						updateFailed = true
						s.logger.WithField("namespace", namespace).WithField("pvc", pvc.Name).Warningf("resize pvc failed:%s", err.Error())
					}
				}
			}
			if !updateFailed && len(pvcs.Items) != 0 {
				annotations["storageCapacity"] = fmt.Sprintf("%d", stateCapacity)
				storedStatefulSet.Annotations = annotations
				if realUpdate {
					s.logger.WithField("namespace", namespace).WithField("statefulSet", statefulSet.Name).Infof("resize statefulset pvcs from %d to %d Success", storedCapacity, stateCapacity)
				} else {
					s.logger.WithField("namespace", namespace).WithField("pvc", rfName).Warningf("set annotations,resize nothing")
				}
			}
		}
	}
	// set stored.volumeClaimTemplates
	statefulSet.Spec.VolumeClaimTemplates = storedStatefulSet.Spec.VolumeClaimTemplates

	statefulSet.Annotations = util.MergeAnnotations(storedStatefulSet.Annotations, statefulSet.Annotations)
	return s.UpdateStatefulSet(namespace, statefulSet)
}

// DeleteStatefulSet will delete the statefulset
func (s *StatefulSetService) DeleteStatefulSet(namespace, name string) error {
	propagation := metav1.DeletePropagationForeground
	err := s.kubeClient.AppsV1().StatefulSets(namespace).Delete(context.TODO(), name, metav1.DeleteOptions{PropagationPolicy: &propagation})
	recordMetrics(namespace, "StatefulSet", name, "DELETE", err, s.metricsRecorder)
	return err
}

// ListStatefulSets will retrieve a list of statefulset in the given namespace
func (s *StatefulSetService) ListStatefulSets(namespace string) (*appsv1.StatefulSetList, error) {
	stsList, err := s.kubeClient.AppsV1().StatefulSets(namespace).List(context.TODO(), metav1.ListOptions{})
	recordMetrics(namespace, "StatefulSet", metrics.NOT_APPLICABLE, "LIST", err, s.metricsRecorder)
	return stsList, err
}

// GetControllerRevision will retrieve the requested ControllerRevision based on namespace and name
func (s *StatefulSetService) GetControllerRevision(namespace, name string) (*appsv1.ControllerRevision, error) {
	revision, err := s.kubeClient.AppsV1().ControllerRevisions(namespace).Get(context.TODO(), name, metav1.GetOptions{})
	recordMetrics(namespace, "ControllerRevision", name, "GET", err, s.metricsRecorder)
	return revision, err
}
