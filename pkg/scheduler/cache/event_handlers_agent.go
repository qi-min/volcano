package cache

// TODO: This event handlers will be removed to agent scheduler's cache once done
import (
	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
	fwk "k8s.io/kube-scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	schedutil "k8s.io/kubernetes/pkg/scheduler/util"
)

func (sc *SchedulerCache) AddPodToSchedulingQueue(obj interface{}) {
	_, pod, err := schedutil.As[*v1.Pod](nil, obj)
	if err != nil {
		klog.Errorf("Failed to convert objects to *v1.Pod: %v", err)
		return
	}

	sc.fastPathSchedulingQueue.Add(klog.Background(), pod)
}

func (sc *SchedulerCache) UpdatePodInSchedulingQueue(oldObj, newObj interface{}) {
	oldPod, newPod, err := schedutil.As[*v1.Pod](oldObj, newObj)
	if err != nil {
		klog.Errorf("Failed to convert objects to *v1.Pod: %v", err)
		return
	}

	if oldPod.ResourceVersion == newPod.ResourceVersion {
		return
	}

	sc.fastPathSchedulingQueue.Update(klog.Background(), oldPod, newPod)
}

func (sc *SchedulerCache) DeletePodFromSchedulingQueue(obj interface{}) {
	_, pod, err := schedutil.As[*v1.Pod](nil, obj)
	if err != nil {
		klog.Errorf("Failed to convert objects to *v1.Pod: %v", err)
		return
	}

	sc.fastPathSchedulingQueue.Delete(pod)
}

func (sc *SchedulerCache) AddPodToCache(obj interface{}) {
	_, pod, err := schedutil.As[*v1.Pod](nil, obj)
	if err != nil {
		klog.Errorf("Failed to convert objects to *v1.Pod: %v", err)
		return
	}

	sc.Mutex.Lock()
	defer sc.Mutex.Unlock()

	err = sc.addPod(pod)
	if err != nil {
		klog.Errorf("Failed to add pod <%s/%s> into cache: %v",
			pod.Namespace, pod.Name, err)
		return
	}
	klog.V(3).Infof("Added pod <%s/%v> into cache.", pod.Namespace, pod.Name)

	// Currently we still use AssignedPodAdded and only care about pod affinity and pod topology spread,
	// directly using MoveAllToActiveOrBackoffQueue may lead to a decrease in throughput.
	sc.fastPathSchedulingQueue.AssignedPodAdded(klog.Background(), pod)
}

func (sc *SchedulerCache) UpdatePodInCache(oldObj interface{}, newObj interface{}) {
	oldPod, newPod, err := schedutil.As[*v1.Pod](oldObj, newObj)
	if err != nil {
		klog.Errorf("Failed to convert objects to *v1.Pod: %v", err)
		return
	}

	sc.Mutex.Lock()
	defer sc.Mutex.Unlock()

	err = sc.updatePod(oldPod, newPod)
	if err != nil {
		klog.Errorf("Failed to update pod %v in cache: %v", oldPod.Name, err)
		return
	}

	klog.V(4).Infof("Updated pod <%s/%v> in cache.", oldPod.Namespace, oldPod.Name)

	events := framework.PodSchedulingPropertiesChange(newPod, oldPod)
	for _, evt := range events {
		// Currently we still use AssignedPodUpdated and only care about pod affinity and pod topology spread,
		// directly using MoveAllToActiveOrBackoffQueue may lead to a decrease in throughput.
		sc.fastPathSchedulingQueue.AssignedPodUpdated(klog.Background(), oldPod, newPod, evt)
	}
}

func (sc *SchedulerCache) DeletePodFromCache(obj interface{}) {
	_, pod, err := schedutil.As[*v1.Pod](nil, obj)
	if err != nil {
		klog.Errorf("Failed to convert objects to *v1.Pod: %v", err)
		return
	}

	sc.Mutex.Lock()
	defer sc.Mutex.Unlock()

	err = sc.deletePod(pod)
	if err != nil {
		klog.Errorf("Failed to delete pod %v from cache: %v", pod.Name, err)
		return
	}

	klog.V(3).Infof("Deleted pod <%s/%v> from cache.", pod.Namespace, pod.Name)

	// If QueueingHint is not enabled, the scheduler will move all unschedulable pods to backoffQ/activeQ when a pod delete event occurs,
	// therefore we can add queueing hint support in the future.
	sc.fastPathSchedulingQueue.MoveAllToActiveOrBackoffQueue(klog.Background(), framework.EventAssignedPodDelete, pod, nil, nil)
}

func (sc *SchedulerCache) AddNodeToCache(obj interface{}) {
	_, node, err := schedutil.As[*v1.Node](nil, obj)
	if err != nil {
		klog.Errorf("Failed to convert objects to *v1.Pod: %v", err)
		return
	}

	// sc.AddOrUpdateNode(node)
	evt := fwk.ClusterEvent{Resource: fwk.Node, ActionType: fwk.Add}
	// TODO: Should we add a preCheckForNode function to filter nodes before moving pods?
	sc.fastPathSchedulingQueue.MoveAllToActiveOrBackoffQueue(klog.Background(), evt, nil, node, nil)
}

func (sc *SchedulerCache) UpdateNodeInCache(oldObj, newObj interface{}) {
	oldNode, newNode, err := schedutil.As[*v1.Node](oldObj, newObj)
	if err != nil {
		klog.Errorf("Failed to convert objects to *v1.Pod: %v", err)
		return
	}

	// sc.AddOrUpdateNode(node)
	events := framework.NodeSchedulingPropertiesChange(newNode, oldNode)
	for _, evt := range events {
		// TODO: Should we add a preCheckForNode function to filter nodes before moving pods?
		sc.fastPathSchedulingQueue.MoveAllToActiveOrBackoffQueue(klog.Background(), evt, oldNode, newNode, nil)
	}
}

func (sc *SchedulerCache) DeleteNodeFromCache(obj interface{}) {
	_, node, err := schedutil.As[*v1.Node](nil, obj)
	if err != nil {
		klog.Errorf("Failed to convert objects to *v1.Pod: %v", err)
		return
	}

	evt := fwk.ClusterEvent{Resource: fwk.Node, ActionType: fwk.Delete}
	sc.fastPathSchedulingQueue.MoveAllToActiveOrBackoffQueue(klog.Background(), evt, node, nil, nil)
	// sc.RemoveNode(node)
}
