package collector

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/tektoncd/pipeline/pkg/apis/pipeline/v1"
	"knative.dev/pkg/apis"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

type PipelineRunScheduledCollector struct {
	durationScheduled *prometheus.HistogramVec
	prSchedNameLabel  bool
}

func NewPipelineRunScheduledMetric() *prometheus.HistogramVec {
	labelNames := []string{NS_LABEL, STATUS_LABEL}
	durationScheduled := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name: "pipelinerun_duration_scheduled_seconds",
		Help: "Duration in seconds for a PipelineRun to be 'scheduled', meaning it has been received by the Tekton controller.  This is an indication of how quickly create events from the API server are arriving to the Tekton controller.",
		// reminder: exponential buckets need a start value greater than 0
		// the results in buckets of 0.1, 0.5, 2.5, 12.5, 62.5, 312.5 seconds
		Buckets: prometheus.ExponentialBuckets(0.1, 5, 6),
	}, labelNames)

	metrics.Registry.MustRegister(durationScheduled)

	return durationScheduled
}

func bumpPipelineRunScheduledDuration(scheduleDuration float64, pr *v1.PipelineRun, metric *prometheus.HistogramVec) {
	succeededCondition := pr.Status.GetCondition(apis.ConditionSucceeded)
	status := SUCCEEDED
	if succeededCondition.IsFalse() {
		status = FAILED
	}
	labels := map[string]string{NS_LABEL: pr.Namespace, STATUS_LABEL: status}
	metric.With(labels).Observe(scheduleDuration)
}

func calculateScheduledDurationPipelineRun(pipelineRun *v1.PipelineRun) float64 {
	return calculateScheduledDuration(pipelineRun.CreationTimestamp.Time, pipelineRun.Status.StartTime.Time) / 1000
}

type startTimeEventFilter struct {
	metric *prometheus.HistogramVec
}

func (f *startTimeEventFilter) Create(event.CreateEvent) bool {
	return false
}

func (f *startTimeEventFilter) Delete(event.DeleteEvent) bool {
	return false
}

func (f *startTimeEventFilter) Update(e event.UpdateEvent) bool {

	oldPR, okold := e.ObjectOld.(*v1.PipelineRun)
	newPR, oknew := e.ObjectNew.(*v1.PipelineRun)
	if okold && oknew {
		if !oldPR.IsDone() && newPR.IsDone() {
			bumpPipelineRunScheduledDuration(calculateScheduledDurationPipelineRun(newPR), newPR, f.metric)
			return false
		}
	}
	return false
}

func (f *startTimeEventFilter) Generic(event.GenericEvent) bool {
	return false
}
