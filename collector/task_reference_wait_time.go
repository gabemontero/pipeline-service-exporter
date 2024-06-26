package collector

import (
	"fmt"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/tektoncd/pipeline/pkg/apis/pipeline/v1"
	"k8s.io/apimachinery/pkg/types"
	"knative.dev/pkg/apis"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

func NewTaskReferenceWaitTimeMetric() *prometheus.HistogramVec {
	labelNames := []string{NS_LABEL}
	waitMetric := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "taskrun_task_resolution_wait_milliseconds",
		Help:    "Duration in milliseconds for a resolution request for a task reference needed by a taskrun to be recognized as complete by the taskrun reconciler in the tekton controller. ",
		Buckets: prometheus.ExponentialBuckets(float64(100), float64(5), 6),
	}, labelNames)
	metrics.Registry.MustRegister(waitMetric)
	return waitMetric
}

type taskRefWaitTimeFilter struct {
	waitDuration *prometheus.HistogramVec
	// so knative/tekton allows for updates to a conditions last transition time without changing the reason of the condition,
	// but if the condition has not changed,  it leaves the transition time alone.  The tekton code right now has a constant
	// message so the condition should not change on any multiple calls.  That said, we'll add a log that captures that, and
	// if it is occuring, we'll need to track the original transition time either via state in this filter, or as a label/annotation
	// on the taskrun
}

func (f *taskRefWaitTimeFilter) Create(event.CreateEvent) bool {
	return false
}

func (f *taskRefWaitTimeFilter) Generic(event.GenericEvent) bool {
	return false
}

func (f *taskRefWaitTimeFilter) Delete(event.DeleteEvent) bool {
	return false
}

func (f *taskRefWaitTimeFilter) Update(e event.UpdateEvent) bool {

	oldTR, okold := e.ObjectOld.(*v1.TaskRun)
	newTR, oknew := e.ObjectNew.(*v1.TaskRun)
	if okold && oknew {
		newSucceedCondition := newTR.Status.GetCondition(apis.ConditionSucceeded)
		if newSucceedCondition == nil {
			return false
		}
		if !oldTR.IsDone() && newTR.IsDone() {
			// if we did not use some sort of resolve, set metric to 0
			if newTR.Spec.TaskRef == nil {
				labels := map[string]string{NS_LABEL: newTR.Namespace}
				f.waitDuration.With(labels).Observe(float64(0))
			}
			return false
		}
		if newTR.IsDone() {
			return false
		}
		oldSucceedCondtition := oldTR.Status.GetCondition(apis.ConditionSucceeded)
		if oldSucceedCondtition == nil {
			return false
		}
		oldReason := oldSucceedCondtition.Reason
		newReason := newSucceedCondition.Reason
		if oldReason == v1.TaskRunReasonResolvingTaskRef && newReason != v1.TaskRunReasonResolvingTaskRef {
			labels := map[string]string{NS_LABEL: newTR.Namespace}
			originalTime := oldSucceedCondtition.LastTransitionTime.Inner
			f.waitDuration.With(labels).Observe(float64(newSucceedCondition.LastTransitionTime.Inner.Sub(originalTime.Time).Milliseconds()))
			return false
		}
		// per current examination of Tekton code, we should not see any updates in transition time
		// if multiple SetCondition calls are made, as the Reason/Message fields should not change for resolving refs,
		// but if that changes, this log should be a warning;
		// after running with the log below for a few weeks, the difference has only ever been 1 second, so coverting to debug
		// log from info log
		if oldReason == v1.TaskRunReasonResolvingTaskRef && newReason == v1.TaskRunReasonResolvingTaskRef &&
			!oldSucceedCondtition.LastTransitionTime.Inner.Equal(&newSucceedCondition.LastTransitionTime.Inner) {
			ctrl.Log.V(6).Info(fmt.Sprintf("WARNING resolving condition for taskrun %s:%s changed from %#v to %#v",
				newTR.Namespace,
				newTR.Name,
				oldSucceedCondtition,
				newSucceedCondition))
			return false
		}
	}
	return false
}

func (f *taskRefWaitTimeFilter) getKey(tr *v1.TaskRun) string {
	key := types.NamespacedName{
		Namespace: tr.Namespace,
		Name:      tr.Name,
	}
	return key.String()
}
