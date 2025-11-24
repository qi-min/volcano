package cache

import (
	"time"

	corev1helpers "k8s.io/component-helpers/scheduling/corev1"
	fwk "k8s.io/kube-scheduler/framework"
	schedulingqueue "k8s.io/kubernetes/pkg/scheduler/backend/queue"
	"k8s.io/utils/clock"
)

// TODO: this file would be removed into separate agent cache package later

type schedulerOptions struct {
	clock                             clock.WithTicker
	podInitialBackoffSeconds          int64
	podMaxBackoffSeconds              int64
	podMaxInUnschedulablePodsDuration time.Duration
}

// TODO: these default values can be overriden by config file or command line args
var defaultSchedulerOptions = schedulerOptions{
	clock:                             clock.RealClock{},
	podInitialBackoffSeconds:          int64(schedulingqueue.DefaultPodInitialBackoffDuration.Seconds()),
	podMaxBackoffSeconds:              int64(schedulingqueue.DefaultPodMaxBackoffDuration.Seconds()),
	podMaxInUnschedulablePodsDuration: schedulingqueue.DefaultPodMaxInUnschedulablePodsDuration,
}

// Less is the function used by the activeQ heap algorithm to sort pods. Currently we use the same logic as PrioritySort plugin.
func Less(pInfo1, pInfo2 fwk.QueuedPodInfo) bool {
	p1 := corev1helpers.PodPriority(pInfo1.GetPodInfo().GetPod())
	p2 := corev1helpers.PodPriority(pInfo2.GetPodInfo().GetPod())
	return (p1 > p2) || (p1 == p2 && pInfo1.GetTimestamp().Before(pInfo2.GetTimestamp()))
}
