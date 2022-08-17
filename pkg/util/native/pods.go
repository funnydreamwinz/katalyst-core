/*
Copyright 2022 The Katalyst Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package native

import (
	"fmt"
	"strings"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/apis/core/v1/helper/qos"
	kubelettypes "k8s.io/kubernetes/pkg/kubelet/types"

	"github.com/kubewharf/katalyst-core/pkg/consts"
)

// PodAnnotationFilter is used to filter pods annotated with a pair of specific key and value
func PodAnnotationFilter(pod *v1.Pod, key, value string) bool {
	if pod == nil || pod.Annotations == nil {
		return false
	}

	return pod.Annotations[key] == value
}

// FilterPods filter pods that filter func return true.
func FilterPods(pods []*v1.Pod, filterFunc func(*v1.Pod) (bool, error)) []*v1.Pod {
	var filtered []*v1.Pod
	for _, pod := range pods {
		if pod == nil {
			continue
		}

		if ok, err := filterFunc(pod); err != nil {
			klog.Errorf("filter pod %v err: %v", pod.Name, err)
		} else if ok {
			filtered = append(filtered, pod)
		}
	}

	return filtered
}

// SumUpPodRequestResources sum up resources in all containers request
// init container is included (count on the max request of all init containers)
func SumUpPodRequestResources(pod *v1.Pod) v1.ResourceList {
	res := make(v1.ResourceList)

	sumRequests := func(containers []v1.Container) {
		for _, container := range containers {
			res = AddResources(res, container.Resources.Requests)
		}

		if pod.Spec.Overhead != nil {
			res = AddResources(res, pod.Spec.Overhead)
		}
	}

	sumRequests(pod.Spec.Containers)
	for _, container := range pod.Spec.InitContainers {
		for resourceName := range container.Resources.Requests {
			quantity := container.Resources.Requests[resourceName].DeepCopy()
			if origin, ok := res[resourceName]; !ok || (&origin).Value() < quantity.Value() {
				res[resourceName] = quantity
			}
		}
	}

	return res
}

// SumUpPodLimitResources sum up resources in all containers request
// init container is included (count on the max limit of all init containers)
func SumUpPodLimitResources(pod *v1.Pod) v1.ResourceList {
	res := make(v1.ResourceList)

	sumLimits := func(containers []v1.Container) {
		for _, container := range containers {
			res = AddResources(res, container.Resources.Limits)
		}

		if pod.Spec.Overhead != nil {
			res = AddResources(res, pod.Spec.Overhead)
		}
	}

	sumLimits(pod.Spec.Containers)
	for _, container := range pod.Spec.InitContainers {
		for resourceName := range container.Resources.Limits {
			quantity := container.Resources.Limits[resourceName].DeepCopy()
			if origin, ok := res[resourceName]; !ok || (&origin).Value() < quantity.Value() {
				res[resourceName] = quantity
			}
		}
	}

	return res
}

func PodAndContainersAreTerminal(pod *v1.Pod) (containersTerminal, podWorkerTerminal bool) {
	status := pod.Status

	// A pod transitions into failed or succeeded from either container lifecycle (RestartNever container
	// fails) or due to external events like deletion or eviction. A terminal pod *should* have no running
	// containers, but to know that the pod has completed its lifecycle you must wait for containers to also
	// be terminal.
	containersTerminal = containerNotRunning(status.ContainerStatuses)
	// The kubelet must accept config changes from the pod spec until it has reached a point where changes would
	// have no effect on any running container.
	podWorkerTerminal = status.Phase == v1.PodFailed || status.Phase == v1.PodSucceeded || (pod.DeletionTimestamp != nil && containersTerminal)
	return
}

// PodIsTerminated returns whether the pod is at terminal state.
func PodIsTerminated(pod *v1.Pod) bool {
	if pod == nil {
		return true
	}
	_, podWorkerTerminal := PodAndContainersAreTerminal(pod)
	return podWorkerTerminal
}

// PodIsReady returns whether the pod is at ready state.
func PodIsReady(pod *v1.Pod) bool {
	if len(pod.Spec.Containers) != len(pod.Status.ContainerStatuses) {
		return false
	}
	for _, containerStatus := range pod.Status.ContainerStatuses {
		if !containerStatus.Ready {
			return false
		}
	}
	return true
}

// PodIsActive returns whether the pod is not terminated.
func PodIsActive(pod *v1.Pod) bool {
	return !PodIsTerminated(pod)
}

// FilterOutSkipEvictionPods return pods should be candidates to evict
// including native critical pods and user-defined filtered pods
func FilterOutSkipEvictionPods(pods []*v1.Pod, filterOutAnnotations, filterOutLabels sets.String) []*v1.Pod {
	var filteredPods []*v1.Pod
filter:
	for _, p := range pods {
		if p == nil || kubelettypes.IsCriticalPod(p) {
			continue
		}

		for key := range p.Annotations {
			if filterOutAnnotations.Has(key) {
				continue filter
			}
		}

		for key := range p.Labels {
			if filterOutLabels.Has(key) {
				continue filter
			}
		}

		filteredPods = append(filteredPods, p)
	}
	return filteredPods
}

// GeneratePodContainerName return a unique key for a container in a pod
func GeneratePodContainerName(podName, containerName string) consts.PodContainerName {
	return consts.PodContainerName(podName + "," + containerName)
}

// ParsePodContainerName parse key and return pod name and container name
func ParsePodContainerName(key consts.PodContainerName) (string, string, error) {
	containerKeys := strings.Split(string(key), ",")
	if len(containerKeys) != 2 {
		err := fmt.Errorf("split result's length mismatch")
		return "", "", err
	}
	return containerKeys[0], containerKeys[1], nil
}

// GenerateContainerName return a unique key for a container
func GenerateContainerName(containerName string) consts.ContainerName {
	return consts.ContainerName(containerName)
}

// ParseContainerName parse key and return container name
func ParseContainerName(key consts.ContainerName) string {
	return string(key)
}

// CheckQosClassChanged checks whether the pod's QosClass will change if annotationResources are applied to this pod
func CheckQosClassChanged(resources map[string]v1.ResourceRequirements, pod *v1.Pod) (bool, error) {
	if pod == nil {
		return false, fmt.Errorf("pod is nil")
	}

	podCopy := pod.DeepCopy()
	ApplyPodResources(resources, podCopy)

	return qos.GetPodQOS(podCopy) != qos.GetPodQOS(pod), nil
}

// ApplyPodResources is used to apply map[string]v1.ResourceRequirements to the given pod,
// and ignore the container-names / resource-names that not appear in the given map param
func ApplyPodResources(resources map[string]v1.ResourceRequirements, pod *v1.Pod) {
	for i := 0; i < len(pod.Spec.Containers); i++ {
		if containerResource, ok := resources[pod.Spec.Containers[i].Name]; ok {
			if pod.Spec.Containers[i].Resources.Requests == nil {
				pod.Spec.Containers[i].Resources.Requests = v1.ResourceList{}
			}
			if containerResource.Requests != nil {
				for resourceName, quantity := range containerResource.Requests {
					pod.Spec.Containers[i].Resources.Requests[resourceName] = quantity
				}
			}

			if pod.Spec.Containers[i].Resources.Limits == nil {
				pod.Spec.Containers[i].Resources.Limits = v1.ResourceList{}
			}
			if containerResource.Limits != nil {
				for resourceName, quantity := range containerResource.Limits {
					pod.Spec.Containers[i].Resources.Limits[resourceName] = quantity
				}
			}
		}
	}
}

func GetPodNamespaceNameKeyMap(podList []*v1.Pod) map[string]*v1.Pod {
	podMap := make(map[string]*v1.Pod, len(podList))
	for _, pod := range podList {
		if pod == nil {
			continue
		}

		key := GenerateUniqObjectNameKey(pod)
		if oldPod, ok := podMap[key]; ok && oldPod.CreationTimestamp.After(pod.CreationTimestamp.Time) {
			continue
		}

		podMap[key] = pod
	}
	return podMap
}

// IsAssignedPod selects pods that are assigned (scheduled and running).
func IsAssignedPod(pod *v1.Pod) bool {
	return len(pod.Spec.NodeName) != 0
}

// ParseHostPortsForPod gets host ports from pod spec
func ParseHostPortsForPod(pod *v1.Pod, portName string) []int32 {
	var res []int32
	for i := range pod.Spec.Containers {
		res = append(res, ParseHostPortsForContainer(&pod.Spec.Containers[i], portName)...)
	}
	return res
}

// GetNamespacedNameListFromSlice returns a slice of namespaced name
func GetNamespacedNameListFromSlice(podSlice []*v1.Pod) []string {
	namespacedNameList := make([]string, 0, len(podSlice))
	for _, pod := range podSlice {
		namespacedNameList = append(namespacedNameList, pod.Namespace+"/"+pod.Name)
	}
	return namespacedNameList
}
