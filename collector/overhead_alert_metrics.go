package collector

import (
	"context"
	"fmt"
	"github.com/prometheus/client_golang/prometheus"
	v1 "github.com/tektoncd/pipeline/pkg/apis/pipeline/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"knative.dev/pkg/apis"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

type OverheadCollector struct {
	execution  *prometheus.HistogramVec
	scheduling *prometheus.HistogramVec
}

type ReconcileOverhead struct {
	client        client.Client
	scheme        *runtime.Scheme
	eventRecorder record.EventRecorder
	collector     *OverheadCollector
}

type overheadGapEventFilter struct {
	client client.Client
}

func (f *overheadGapEventFilter) Create(event.CreateEvent) bool {
	return false
}

func (f *overheadGapEventFilter) Delete(event.DeleteEvent) bool {
	return false
}

func (f *overheadGapEventFilter) Update(e event.UpdateEvent) bool {
	oldPR, okold := e.ObjectOld.(*v1.PipelineRun)
	newPR, oknew := e.ObjectNew.(*v1.PipelineRun)
	// the real-time filtering involes retrieving the taskruns that are childs of this pipelinerun, so we only
	// calculate when the pipelinerun transtions to done, and then compare the kinds; note - do not need to check for cancel,
	// as eventually those PRs will be marked done once any running TRs are done
	if okold && oknew {
		_, throttled := newPR.Labels[THROTTLED_LABEL]
		// if this pipelinerun endured throttling while running, given the requeue'ing the pipeline controller unfortunately entails,
		// we are punting on calculating overhead at this time
		if throttled {
			return false
		}
		// NOTE: confirmed that the succeeded condition is marked done and the completion timestamp is set at the same time
		if !oldPR.IsDone() && newPR.IsDone() {
			return true
		}
		// checking here to bypass the throttle check
		if oldPR.IsDone() && newPR.IsDone() {
			return false
		}

		// if we are newly throttled, reconcile so we can set our label and filter this entry, at least for now, from
		// our execution overhead,
		ctx := context.Background()
		newThrottled, _, _ := isPipelineRunThrottled(newPR, f.client, ctx)
		// seen timing issues where both the old and new pipelineruns are throttled, where rapid, concurrent updates
		// must have resulted in merging updates together before updating the watch, so we don't bother with a compare here;
		// we'll check our label before retagging
		if newThrottled {
			return true
		}

		// also seen some timing windows where if the taskrun is throttled quickly (like on pod count), the pipelinerun may not
		// get updated after the taskrun is throttled; so we reconcile until we have taskruns listed in the status,
		// where then in the reconcile we can requeue if the taskruns are not at least defined and seeded in the
		// pipelinerun
		if !isPipelineRunGoing(newPR, f.client, ctx) {
			return true
		}
	}
	return false
}

func (f *overheadGapEventFilter) Generic(event.GenericEvent) bool {
	return false
}
func NewOverheadCollector() *OverheadCollector {
	labelNames := []string{NS_LABEL, STATUS_LABEL}
	executionMetric := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "pipeline_service_execution_overhead_percentage",
		Help:    "Proportion of time elapsed between the completion of a TaskRun and the start of the next TaskRun within a PipelineRun to the total duration of successful PipelineRuns",
		Buckets: prometheus.DefBuckets,
	}, labelNames)
	schedulingMetric := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "pipeline_service_schedule_overhead_percentage",
		Help:    "Proportion of time elapsed waiting for the pipeline controller to receive create events compared to the total duration of successful PipelineRuns",
		Buckets: prometheus.DefBuckets,
	}, labelNames)
	collector := &OverheadCollector{execution: executionMetric, scheduling: schedulingMetric}
	metrics.Registry.MustRegister(executionMetric, schedulingMetric)
	return collector
}

func accumulateGaps(pr *v1.PipelineRun, oc client.Client, ctx context.Context) (float64, []GapEntry, bool) {
	if skipPipelineRun(pr) {
		return float64(0), []GapEntry{}, false
	}
	gapTotal := float64(0)

	sortedTaskRunsByCreateTimes, reverseOrderSortedTaskRunsByCompletionTimes, abort := sortTaskRunsForGapCalculations(pr, oc, ctx)

	if abort {
		return float64(0), []GapEntry{}, false
	}

	gapEntries := calculateGaps(ctx, pr, oc, sortedTaskRunsByCreateTimes, reverseOrderSortedTaskRunsByCompletionTimes)
	for _, gapEntry := range gapEntries {
		gapTotal = gapTotal + gapEntry.gap
	}

	return gapTotal, gapEntries, !abort
}

func (r *ExporterReconcile) ReconcileOverhead(ctx context.Context, request reconcile.Request) (reconcile.Result, error) {
	var cancel context.CancelFunc
	ctx, cancel = context.WithCancel(ctx)
	defer cancel()
	log := log.FromContext(ctx)

	pr := &v1.PipelineRun{}
	err := r.client.Get(ctx, types.NamespacedName{Namespace: request.Namespace, Name: request.Name}, pr)
	if err != nil && !errors.IsNotFound(err) {
		return reconcile.Result{}, err
	}
	if err != nil {
		log.V(4).Info(fmt.Sprintf("ignoring deleted pipelinerun %q", request.NamespacedName))
		return reconcile.Result{}, nil
	}
	succeedCondition := pr.Status.GetCondition(apis.ConditionSucceeded)
	if succeedCondition != nil && !succeedCondition.IsUnknown() {
		gapTotal, gapEntries, foundGaps := accumulateGaps(pr, r.client, ctx)
		if foundGaps {
			status := SUCCEEDED
			if succeedCondition.IsFalse() {
				status = FAILED
			}
			labels := map[string]string{NS_LABEL: pr.Namespace, STATUS_LABEL: status}
			totalDuration := float64(pr.Status.CompletionTime.Time.Sub(pr.Status.StartTime.Time).Milliseconds())
			if !filter(gapTotal, totalDuration) {
				overhead := gapTotal / totalDuration
				log.V(4).Info(fmt.Sprintf("registering execution metric for %s with gap %v and total %v and overhead %v",
					request.NamespacedName.String(), gapTotal, totalDuration, overhead))
				if overhead >= ALERT_RATIO {
					dbgStr := fmt.Sprintf("PipelineRun %s:%s has alert level execution overhead with a value of %v where gapTotal %v and totalDuration %v and individual gaps: \n", pr.Namespace, pr.Name, overhead, gapTotal, totalDuration)
					for _, ge := range gapEntries {
						s := fmt.Sprintf("  start %s end %s status %s gap %v\n", ge.completed, ge.upcoming, ge.status, ge.gap)
						dbgStr = dbgStr + s
					}
					log.Info(dbgStr)
				}
				r.overheadCollector.execution.With(labels).Observe(overhead)
			} else {
				log.V(4).Info(fmt.Sprintf("filtering execution metric for %s with gap %v and total %v",
					request.NamespacedName.String(), gapTotal, totalDuration))
			}
			scheduleDuration := calculateScheduledDuration(pr.CreationTimestamp.Time, pr.Status.StartTime.Time)
			if !filter(scheduleDuration, totalDuration) {
				overhead := scheduleDuration / totalDuration
				log.V(4).Info(fmt.Sprintf("registering scheduling metric for %s with gap %v and total %v and overhead %v",
					request.NamespacedName.String(), scheduleDuration, totalDuration, overhead))
				r.overheadCollector.scheduling.With(labels).Observe(overhead)
			} else {
				log.V(4).Info(fmt.Sprintf("filtering scheduling metric for %s with gap %v and total %v",
					request.NamespacedName.String(), scheduleDuration, totalDuration))
			}
		}
	} else {
		if !isPipelineRunGoing(pr, r.client, ctx) {
			return reconcile.Result{Requeue: true}, nil
		}
		// if still running, we set the label here instead of in the filter so we can retry on error if need be
		return reconcile.Result{}, tagPipelineRunsWithTaskRunsGettingThrottled(pr, r.client, ctx)
	}
	return reconcile.Result{}, nil
}
