package collector

import (
	"fmt"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/tektoncd/pipeline/pkg/apis/pipeline"
	"github.com/tektoncd/pipeline/pkg/apis/pipeline/v1"
	"knative.dev/pkg/apis"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

func NewPipelineReferenceWaitTimeMetric() *prometheus.HistogramVec {
	labelNames := []string{NS_LABEL}
	waitMetric := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "pipelinerun_pipeline_resolution_wait_milliseconds",
		Help:    "Duration in milliseconds for a resolution request for a pipeline reference needed by a pipelinerun to be recognized as complete by the pipelinerun reconciler in the tekton controller. ",
		Buckets: prometheus.ExponentialBuckets(float64(100), float64(5), 6),
	}, labelNames)
	metrics.Registry.MustRegister(waitMetric)
	return waitMetric
}

type pipelineRefWaitTimeFilter struct {
	waitDuration *prometheus.HistogramVec
	// so knative/tekton allows for updates to a conditions last transition time without changing the reason of the condition,
	// but if the condition has not changed,  it leaves the transition time alone.  The tekton code right now has a constant
	// message so the condition should not change on any multiple calls.  That said, we'll add a log that captures that, and
	// if it is occuring, we'll need to track the original transition time either via state in this filter, or as a label/annotation
	// on the pipelinerun
}

func (f *pipelineRefWaitTimeFilter) Create(event.CreateEvent) bool {
	return false
}

func (f *pipelineRefWaitTimeFilter) Generic(event.GenericEvent) bool {
	return false
}

func (f *pipelineRefWaitTimeFilter) Delete(event.DeleteEvent) bool {
	return false
}

func (f *pipelineRefWaitTimeFilter) Update(e event.UpdateEvent) bool {
	oldPR, okold := e.ObjectOld.(*v1.PipelineRun)
	newPR, oknew := e.ObjectNew.(*v1.PipelineRun)
	if okold && oknew {
		newSucceedCondition := newPR.Status.GetCondition(apis.ConditionSucceeded)
		if newSucceedCondition == nil {
			return false
		}
		if !oldPR.IsDone() && newPR.IsDone() {
			// if we did not use some sort of resolve, set metric to 0
			if newPR.Spec.PipelineRef == nil {
				labels := map[string]string{NS_LABEL: newPR.Namespace}
				f.waitDuration.With(labels).Observe(float64(0))
			}
		}
		if newPR.IsDone() {
			return false
		}
		oldSucceedCondtition := oldPR.Status.GetCondition(apis.ConditionSucceeded)
		if oldSucceedCondtition == nil {
			return false
		}
		oldReason := oldSucceedCondtition.Reason
		newReason := newSucceedCondition.Reason
		// wrt direct string reference, waiting for tag/release with constant moved to the api package
		if oldReason == "ResolvingPipelineRef" && newReason != "ResolvingPipelineRef" {
			labels := map[string]string{NS_LABEL: newPR.Namespace}
			originalTime := oldSucceedCondtition.LastTransitionTime.Inner
			f.waitDuration.With(labels).Observe(float64(newSucceedCondition.LastTransitionTime.Inner.Sub(originalTime.Time).Milliseconds()))
			return false
		}
		// wrt direct string reference, waiting for tag/release with constant moved to the api package;
		// otherwise, per current examination of Tekton code, we should not see any updates in transition time
		// if multiple SetCondition calls are made, as the Reason/Message fields should not change for resolving refs,
		// but if that changes, this log should be a warning;
		// after running with the log below for a few weeks, the difference has only ever been 1 second, so coverting to debug
		// log from info log
		if oldReason == "ResolvingPipelineRef" && newReason == "ResolvingPipelineRef" &&
			!oldSucceedCondtition.LastTransitionTime.Inner.Equal(&newSucceedCondition.LastTransitionTime.Inner) {
			ctrl.Log.V(6).Info(fmt.Sprintf("WARNING resolving condition for pipelinerun %s:%s changed from %#v to %#v",
				newPR.Namespace,
				newPR.Name,
				oldSucceedCondtition,
				newSucceedCondition))
			return false
		}
	}
	return false
}

func pipelineRef(labels map[string]string) string {
	p, _ := labels[pipeline.PipelineLabelKey]
	pr, _ := labels[pipeline.PipelineRunLabelKey]
	switch {
	case len(p) > 0:
		return p
	case len(pr) > 0:
		return pr
	}
	return ""
}
