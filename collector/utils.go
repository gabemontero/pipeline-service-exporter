/*
 Copyright 2023 The Pipeline Service Authors.

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

package collector

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/tektoncd/pipeline/pkg/apis/pipeline"
	"github.com/tektoncd/pipeline/pkg/apis/pipeline/v1"
	"github.com/tektoncd/pipeline/pkg/pod"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"knative.dev/pkg/apis"
	"os"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	FILTER_THRESHOLD  = "FILTER_THRESHOLD"
	DEFAULT_THRESHOLD = float64(300000) // 5 minutes in milliseconds
	ALERT_RATIO       = float64(0.05)
	NS_LABEL          = "namespace"
	TASK_NAME_LABEL   = "taskname"
	STATUS_LABEL      = "status"
	SUCCEEDED         = "succeded"
	FAILED            = "failed"
	THROTTLED_LABEL   = "pipelineservice.appstudio.io/throttled"
)

func pipelineRunPipelineRef(pr *v1.PipelineRun) string {
	val := ""
	ref := pr.Spec.PipelineRef
	if ref != nil {
		val = ref.Name
		if len(val) == 0 {
			for _, p := range ref.Params {
				if strings.TrimSpace(p.Name) == "name" {
					return p.Value.StringVal
				}
			}
		}
	} else {
		if len(pr.GenerateName) > 0 {
			return pr.GenerateName
		}
		// at this point, the pipelinerun name should not have any random aspects, and is
		// essentially a constant, with a minimal cardinality impact
		val = pr.Name
	}
	return val
}

func taskRef(labels map[string]string) string {
	task, _ := labels[pipeline.TaskLabelKey]
	pipelineTask, _ := labels[pipeline.PipelineTaskLabelKey]
	clusterTask, _ := labels[pipeline.ClusterTaskLabelKey]
	taskRun, _ := labels[pipeline.TaskRunLabelKey]
	switch {
	case len(task) > 0:
		return task
	case len(pipelineTask) > 0:
		return pipelineTask
	case len(clusterTask) > 0:
		return clusterTask
	case len(taskRun) > 0:
		return taskRun
	}
	return ""
}

func optionalMetricEnabled(envVarName string) bool {
	env := os.Getenv(envVarName)
	enabled := len(env) > 0
	// any random setting means true
	if enabled {
		// allow for users to turn off by setting to false
		bv, err := strconv.ParseBool(env)
		if err == nil && !bv {
			return false
		}
		return true
	}
	return false
}

const PodCreateFilterEnvName = "POD_CREATE_METRIC_NAMESPACE_FILTER"

func podCreateNameSpaceFilter() map[string]struct{} {
	namespaceFilter := map[string]struct{}{}
	env := os.Getenv(PodCreateFilterEnvName)
	namespaces := strings.Split(env, ",")
	for _, ns := range namespaces {
		namespaceFilter[ns] = struct{}{}
	}
	return namespaceFilter
}

const PipelineRunKickoffFilterEnvName = "PIPELINERUN_KICKOFF_METRIC_NAMESPACE_FILTER"

func pipelineRunKickoffNameSpaceFilter() map[string]struct{} {
	namespaceFilter := map[string]struct{}{}
	env := os.Getenv(PipelineRunKickoffFilterEnvName)
	namespaces := strings.Split(env, ",")
	for _, ns := range namespaces {
		namespaceFilter[ns] = struct{}{}
	}
	return namespaceFilter
}

func calculateScheduledDuration(created, started time.Time) float64 {
	if created.IsZero() || started.IsZero() {
		return 0
	}
	return float64(started.Sub(created).Milliseconds())
}

func skipPipelineRun(pr *v1.PipelineRun) bool {
	if len(pr.Status.ChildReferences) < 1 {
		return true
	}
	// in case there are gaps between a pipelinerun being marked done but the complete timestamp is not set, with the
	// understanding that the complete timestamp is not processed before any completed taskrun complete timestamps have been processed
	if pr.Status.CompletionTime == nil {
		return true
	}

	// we've seen a few times now that quota/node throttling artificially inflates our execution overhead,
	// vs. concurrency contention in our controller;
	// with our separate throttle metrics, we can alert on those if need be; our controller runtime filter
	// should prevent a Reconcile when this label is set, but just in case, let's check here as well
	trName, throttled := pr.Labels[THROTTLED_LABEL]
	if throttled {
		ctrl.Log.Info(fmt.Sprintf("Skipping overhead for pipelinerun %s:%s because taskrun %s was throttled", pr.Namespace, pr.Name, trName))
		return true
	}
	return false
}

func sortTaskRunsForGapCalculations(pr *v1.PipelineRun, oc client.Client, ctx context.Context) ([]*v1.TaskRun, []*v1.TaskRun, bool) {
	sortedTaskRunsByCreateTimes := []*v1.TaskRun{}
	reverseOrderSortedTaskRunsByCompletionTimes := []*v1.TaskRun{}
	// prior testing in staging proved that with enough concurrency, this array is minimally not sorted based on when
	// the task runs were created, so we explicitly sort for that; also, this sorting will allow us to effectively
	// address parallel taskruns vs. taskrun dependencies and ordering (where tekton does not create a taskrun until its dependencies
	// have completed).
	for _, kidRef := range pr.Status.ChildReferences {
		if kidRef.Kind != "TaskRun" {
			continue
		}
		kid := &v1.TaskRun{}
		err := oc.Get(ctx, types.NamespacedName{Namespace: pr.Namespace, Name: kidRef.Name}, kid)
		if err != nil {
			ctrl.Log.Info(fmt.Sprintf("could not calculate gap for taskrun %s:%s: %s", pr.Namespace, kidRef.Name, err.Error()))
			return nil, nil, true
		}

		sortedTaskRunsByCreateTimes = append(sortedTaskRunsByCreateTimes, kid)
		// don't add taskruns that did not complete i.e. presumably timed out of failed; any taskruns that dependended
		// on should not have even been created
		if kid.Status.CompletionTime != nil {
			reverseOrderSortedTaskRunsByCompletionTimes = append(reverseOrderSortedTaskRunsByCompletionTimes, kid)

		}
	}
	sort.SliceStable(sortedTaskRunsByCreateTimes, func(i, j int) bool {
		return sortedTaskRunsByCreateTimes[i].CreationTimestamp.Time.Before(sortedTaskRunsByCreateTimes[j].CreationTimestamp.Time)
	})
	sort.SliceStable(reverseOrderSortedTaskRunsByCompletionTimes, func(i, j int) bool {
		return reverseOrderSortedTaskRunsByCompletionTimes[i].Status.CompletionTime.Time.After(reverseOrderSortedTaskRunsByCompletionTimes[j].Status.CompletionTime.Time)
	})
	return sortedTaskRunsByCreateTimes, reverseOrderSortedTaskRunsByCompletionTimes, false
}

func isPipelineRunThrottled(pr *v1.PipelineRun, oc client.Client, ctx context.Context) (bool, string, error) {
	throttled := false
	throttledTaskRun := ""
	var err error
	for _, kidRef := range pr.Status.ChildReferences {
		if kidRef.Kind != "TaskRun" {
			continue
		}
		kid := &v1.TaskRun{}
		err = oc.Get(ctx, types.NamespacedName{Namespace: pr.Namespace, Name: kidRef.Name}, kid)
		if err != nil && !errors.IsNotFound(err) {
			ctrl.Log.Info(fmt.Sprintf("could not get taskrun %s:%s: %s", pr.Namespace, kidRef.Name, err.Error()))
			return false, "", err
		}
		if isTaskRunThrottled(kid) {
			throttled = true
			throttledTaskRun = kid.Name
			break
		}
	}
	return throttled, throttledTaskRun, nil
}

func isTaskRunThrottled(tr *v1.TaskRun) bool {
	succeedCondition := tr.Status.GetCondition(apis.ConditionSucceeded)
	if succeedCondition != nil && succeedCondition.Status == corev1.ConditionUnknown {
		switch succeedCondition.Reason {
		case pod.ReasonExceededResourceQuota:
			return true
		case pod.ReasonExceededNodeResources:
			return true
		}
	}
	return false
}

func isPipelineRunGoing(pr *v1.PipelineRun, oc client.Client, ctx context.Context) bool {
	for _, kidRef := range pr.Status.ChildReferences {
		if kidRef.Kind != "TaskRun" {
			continue
		}
		return true
	}
	return false
}

func tagPipelineRunsWithTaskRunsGettingThrottled(pr *v1.PipelineRun, oc client.Client, ctx context.Context) error {
	throttled, throttledTaskRun, err := isPipelineRunThrottled(pr, oc, ctx)
	if err != nil {
		return err
	}
	// for our purposes, labelling only the first throttling instances is sufficient
	if pr.Labels == nil {
		pr.Labels = map[string]string{}
	}
	_, previouslyLabelled := pr.Labels[THROTTLED_LABEL]
	if throttled && !previouslyLabelled {
		changedPR := pr.DeepCopy()
		if changedPR.Labels == nil {
			changedPR.Labels = map[string]string{}
		}
		changedPR.Labels[THROTTLED_LABEL] = throttledTaskRun
		ctrl.Log.Info(fmt.Sprintf("Tagging PipelineRun %s:%s as throttled because of %s", pr.Namespace, pr.Name, throttledTaskRun))
		err = oc.Patch(ctx, changedPR, client.MergeFrom(pr))
		if err != nil && errors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

type GapEntry struct {
	status    string
	pipeline  string
	completed string
	upcoming  string
	gap       float64
}

func calculateGaps(ctx context.Context, pr *v1.PipelineRun, oc client.Client, sortedTaskRunsByCreateTimes []*v1.TaskRun, reverseOrderSortedTaskRunsByCompletionTimes []*v1.TaskRun) []GapEntry {
	gapEntries := []GapEntry{}
	prRef := pipelineRunPipelineRef(pr)
	for index, tr := range sortedTaskRunsByCreateTimes {
		succeedCondition := pr.Status.GetCondition(apis.ConditionSucceeded)
		if succeedCondition == nil {
			ctrl.Log.Info(fmt.Sprintf("WARNING: pipielinerun %s:%s marked done but has nil succeed condition", pr.Namespace, pr.Name))
			continue
		}
		if succeedCondition.IsUnknown() {
			ctrl.Log.Info(fmt.Sprintf("WARNING: pipielinerun %s:%s marked done but has unknown succeed condition", pr.Namespace, pr.Name))
			continue
		}
		gapEntry := GapEntry{}
		status := SUCCEEDED
		if succeedCondition.IsFalse() {
			status = FAILED
		}
		gapEntry.status = status
		gapEntry.pipeline = prRef

		if index == 0 {
			// our first task is simple, just work off of the pipelinerun
			gapEntry.gap = float64(tr.CreationTimestamp.Time.Sub(pr.CreationTimestamp.Time).Milliseconds())
			gapEntry.completed = prRef
			gapEntry.upcoming = taskRef(tr.Labels)
			gapEntries = append(gapEntries, gapEntry)
			ctrl.Log.V(6).Info(fmt.Sprintf("first task %s for pipeline %s has gap %v", taskRef(tr.Labels), prRef, gapEntry.gap))
			continue
		}

		firstKid := sortedTaskRunsByCreateTimes[0]

		// so using the first taskrun completion time addresses sequential / chaining dependencies;
		// for parallel, if the first taskrun's completion time is not after this taskrun's create time,
		// that means parallel taskruns, and we work off of the pipelinerun; NOTE: this focuses on "top level" parallel task runs
		// with absolutely no dependencies.  Once any sort of dependency is established, there are no more top level parallel taskruns.
		if firstKid.Status.CompletionTime != nil && firstKid.Status.CompletionTime.Time.After(tr.CreationTimestamp.Time) {
			ctrl.Log.V(4).Info(fmt.Sprintf("task %s considered parallel for pipeline %s", taskRef(tr.Labels), prRef))
			gapEntry.gap = float64(tr.CreationTimestamp.Time.Sub(pr.CreationTimestamp.Time).Milliseconds())
			gapEntry.completed = prRef
			gapEntry.upcoming = taskRef(tr.Labels)
			gapEntries = append(gapEntries, gapEntry)
			continue
		}

		// Conversely, task run chains can run in parallel, and a taskrun can depend on multiple chains or threads of taskruns. We want to find the chain
		// that finished last, but before we are created.  We traverse through our reverse sorted on completion time list to determine that.  But yes, we don't reproduce the DAG
		// graph (there is no clean dependency import path in tekton for that) to confirm the edges.  This approximation is sufficient.

		// get whatever completed first
		timeToCalculateWith := time.Time{}
		trToCalculateWith := &v1.TaskRun{}
		completedID := prRef
		if len(reverseOrderSortedTaskRunsByCompletionTimes) > 0 {
			trToCalculateWith = reverseOrderSortedTaskRunsByCompletionTimes[len(reverseOrderSortedTaskRunsByCompletionTimes)-1]
			completedID = taskRef(trToCalculateWith.Labels)
			timeToCalculateWith = trToCalculateWith.Status.CompletionTime.Time
		} else {
			// if no taskruns completed, that means any taskruns created were created as part of the initial pipelinerun creation,
			// so use the pipelinerun creation time
			timeToCalculateWith = pr.CreationTimestamp.Time
		}
		for _, tr2 := range reverseOrderSortedTaskRunsByCompletionTimes {
			if tr2.Name == tr.Name {
				continue
			}
			ctrl.Log.V(8).Info(fmt.Sprintf("comparing candidate %s to current task %s", taskRef(tr2.Labels), taskRef(tr.Labels)))
			if !tr2.Status.CompletionTime.Time.After(tr.CreationTimestamp.Time) {
				ctrl.Log.V(8).Info(fmt.Sprintf("%s did not complete after so use it to compute gap for current task %s", taskRef(tr2.Labels), taskRef(tr.Labels)))
				trToCalculateWith = tr2
				completedID = taskRef(trToCalculateWith.Labels)
				timeToCalculateWith = tr2.Status.CompletionTime.Time
				break
			}
			ctrl.Log.V(8).Info(fmt.Sprintf("skipping %s as a gap candidate for current task %s is OK", taskRef(tr2.Labels), taskRef(tr.Labels)))
		}
		gapEntry.gap = float64(tr.CreationTimestamp.Time.Sub(timeToCalculateWith).Milliseconds())
		gapEntry.completed = completedID
		gapEntry.upcoming = taskRef(tr.Labels)
		ctrl.Log.V(6).Info(fmt.Sprintf("gap entry completed %s upcoming %s gap %v", gapEntry.completed, gapEntry.upcoming, gapEntry.gap))
		gapEntries = append(gapEntries, gapEntry)
	}
	return gapEntries
}

func filter(numerator, denominator float64) bool {
	threshold := DEFAULT_THRESHOLD
	thresholdStr := os.Getenv(FILTER_THRESHOLD)
	if len(thresholdStr) > 0 {
		thresholdOverride, err := strconv.ParseFloat(thresholdStr, 64)
		if err != nil {
			ctrl.Log.V(6).Info(fmt.Sprintf("error parsing %s env of %s: %s", FILTER_THRESHOLD, thresholdStr, err.Error()))
		} else {
			threshold = thresholdOverride
		}
	}
	// if overhead is non-zero, but total duration is less that 40 seconds,
	// this is a simpler, most likely user defined pipeline which does not fall
	// under our image building based overhead concerns
	if numerator > 0 && denominator < threshold {
		return true
	}
	//TODO we don't have a sense for it yet, but at some point we may get an idea
	// of what is unacceptable overhead regardless of total duration, where we don't
	// try to mitigate the tekton controller and user pipelineruns sharing the same
	// cluster resources
	return false
}

func createJSONFormattedString(o any) string {
	buf, err := json.MarshalIndent(o, "", "    ")
	dbgStr := ""
	if err != nil {
		dbgStr = err.Error()
	} else {
		dbgStr = string(buf)
	}
	return dbgStr
}

func buildLastScanCopy(collector PollCollector, m map[string]map[string]struct{}) map[string]map[string]struct{} {
	keys := []string{}
	for k := range m {
		keys = append(keys, k)
	}
	innerReset(collector, keys)
	// create deep copy for comparing from last scan
	cacheCopy := map[string]map[string]struct{}{}
	for ns, trMap := range m {
		newPrMap := map[string]struct{}{}
		for tr, v := range trMap {
			newPrMap[tr] = v
		}
		cacheCopy[ns] = newPrMap
	}
	return cacheCopy
}

func zeroOutPriorHitNamespacesThatAreNowEmpty(collector PollCollector, lastScan map[string]map[string]struct{}, currentScan map[string]map[string]struct{}) {
	for ns := range lastScan {
		_, inMostRecentScan := currentScan[ns]
		if !inMostRecentScan {
			collector.ZeroCollector(ns)
		}
	}
}

type DeadlockDetector func() bool

type DeadlockTracker struct {
	collector         PollCollector
	deadlocked        DeadlockDetector
	filter            map[string]struct{}
	flaggedNamespaces map[string]struct{}
	lastScan          map[string]map[string]struct{}
	currentScan       map[string]map[string]struct{}
}

func (d *DeadlockTracker) PerformDeadlockDetection(name, ns string) {
	_, filterNS := d.filter[ns]
	if filterNS {
		return
	}

	objMap, alreadyHit := d.currentScan[ns]
	if !alreadyHit {
		objMap = map[string]struct{}{}
		d.currentScan[ns] = objMap
	}

	cacheCopyObjMap, nsHitLastTime := d.lastScan[ns]
	if !nsHitLastTime {
		cacheCopyObjMap = map[string]struct{}{}
	}
	_, objHitLastTime := cacheCopyObjMap[name]

	if d.deadlocked() {
		objMap[name] = struct{}{}
		// so this particular entity has not passed the test for more than one cycle so
		// bump the gauge for the namespace; we don't have a name label because of metric cardinality
		if objHitLastTime {
			d.collector.IncCollector(ns)
			d.flaggedNamespaces[ns] = struct{}{}
		}
	}

	_, ok := d.flaggedNamespaces[ns]
	if ok {
		return
	}
	d.collector.ZeroCollector(ns)
}
