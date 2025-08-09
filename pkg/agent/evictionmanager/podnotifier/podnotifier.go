package podnotifier

import (
	"context"
	"fmt"
	"github.com/kubewharf/katalyst-core/pkg/agent/evictionmanager/rule"
	"k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	"sync"
)

// PodNotifier implements the notify soft eviction actions for given pods.
type PodNotifier interface {
	// Name returns name as identifier for a specific notifier.
	Name() string

	// Start pod notifier logic.
	Start(ctx context.Context)

	// NotifyPods notify a list pods.
	NotifyPods(rpList rule.RuledEvictPodList) error

	// NotifyPod notify a pod.
	NotifyPod(rp *rule.RuledEvictPod) error
}

// SynchronizedPodNotifier trigger notify actions immediately after
// receiving notify requests; only returns true if all pods are
// successfully notified.
type SynchronizedPodNotifier struct {
	notifier Notifier
}

func NewSynchronizedPodNotifier(notifier Notifier) PodNotifier {
	return &SynchronizedPodNotifier{
		notifier: notifier,
	}
}

func (s *SynchronizedPodNotifier) Name() string { return "synchronized-pod-notifier" }

func (s *SynchronizedPodNotifier) Start(_ context.Context) {
	klog.Infof("[synchronized] pod-notifier run with notifier %v", s.notifier.Name())
	defer klog.Infof("[synchronized] pod-notifier started")
}

func (s *SynchronizedPodNotifier) NotifyPod(rp *rule.RuledEvictPod) error {
	if rp == nil || rp.Pod == nil {
		return fmt.Errorf("NotifyPod got nil pod")
	}

	err := s.notifier.Notify(context.Background(), rp.Pod, rp.Reason, rp.EvictionPluginName)
	if err != nil {
		return fmt.Errorf("notify pod: %s/%s failed with error: %v", rp.Pod.Namespace, rp.Pod.Name, err)
	}

	return nil
}

func (s *SynchronizedPodNotifier) NotifyPods(rpList rule.RuledEvictPodList) error {
	var errList []error
	var mtx sync.Mutex

	klog.Infof("[synchronized] pod-notifier evict %d totally", len(rpList))
	syncNodeUtilizationAndAdjust := func(i int) {
		err := s.NotifyPod(rpList[i])

		mtx.Lock()
		if err != nil {
			errList = append(errList, err)
		}
		mtx.Unlock()
	}
	workqueue.ParallelizeUntil(context.Background(), 3, len(rpList), syncNodeUtilizationAndAdjust)

	klog.Infof("[synchronized] successfully evict %d totally", len(rpList)-len(errList))
	return errors.NewAggregate(errList)
}
