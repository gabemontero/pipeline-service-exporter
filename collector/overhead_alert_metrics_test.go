package collector

import (
	"context"
	"fmt"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/tektoncd/pipeline/pkg/apis/pipeline"
	"github.com/tektoncd/pipeline/pkg/apis/pipeline/v1"
	"github.com/tektoncd/pipeline/pkg/apis/pipeline/v1beta1"
	"github.com/tektoncd/pipeline/pkg/pod"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"knative.dev/pkg/apis"
	duckv1 "knative.dev/pkg/apis/duck/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"testing"
	"time"
)

func TestOverheadGapEventFilter_Update(t *testing.T) {
	objs := []client.Object{}
	scheme := runtime.NewScheme()
	_ = v1.AddToScheme(scheme)
	_ = v1beta1.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	filterObj := &overheadGapEventFilter{client: c}
	for _, tc := range []struct {
		name       string
		oldPR      *v1.PipelineRun
		newPR      *v1.PipelineRun
		newTRs     []*v1.TaskRun
		expectedRC bool
	}{
		{
			name:       "not done no status, no kids",
			oldPR:      &v1.PipelineRun{},
			newPR:      &v1.PipelineRun{},
			expectedRC: true,
		},
		{
			name:  "not done status unknown, still no kids",
			oldPR: &v1.PipelineRun{},
			newPR: &v1.PipelineRun{
				Status: v1.PipelineRunStatus{
					Status: duckv1.Status{
						Conditions: []apis.Condition{
							{
								Type:   apis.ConditionSucceeded,
								Status: corev1.ConditionUnknown,
							},
						},
					},
				},
			},
			expectedRC: true,
		},
		{
			name:  "not done taskruns throttled on quota",
			oldPR: &v1.PipelineRun{},
			newPR: &v1.PipelineRun{
				Status: v1.PipelineRunStatus{
					Status: duckv1.Status{
						Conditions: []apis.Condition{
							{
								Type:   apis.ConditionSucceeded,
								Status: corev1.ConditionUnknown,
							},
						},
					},
					PipelineRunStatusFields: v1.PipelineRunStatusFields{
						ChildReferences: []v1.ChildStatusReference{
							{
								TypeMeta: runtime.TypeMeta{Kind: "TaskRun"},
								Name:     "taskrun1",
							},
						},
					},
				},
			},
			newTRs: []*v1.TaskRun{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "taskrun1"},
					Status: v1.TaskRunStatus{
						Status: duckv1.Status{
							Conditions: []apis.Condition{
								{
									Type:   apis.ConditionSucceeded,
									Status: corev1.ConditionUnknown,
									Reason: pod.ReasonExceededResourceQuota,
								},
							},
						},
					},
				},
			},
			expectedRC: true,
		},
		{
			name:  "not done taskruns throttled on node",
			oldPR: &v1.PipelineRun{},
			newPR: &v1.PipelineRun{
				Status: v1.PipelineRunStatus{
					Status: duckv1.Status{
						Conditions: []apis.Condition{
							{
								Type:   apis.ConditionSucceeded,
								Status: corev1.ConditionUnknown,
							},
						},
					},
					PipelineRunStatusFields: v1.PipelineRunStatusFields{
						ChildReferences: []v1.ChildStatusReference{
							{
								TypeMeta: runtime.TypeMeta{Kind: "TaskRun"},
								Name:     "taskrun1",
							},
						},
					},
				},
			},
			newTRs: []*v1.TaskRun{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "taskrun2"},
					Status: v1.TaskRunStatus{
						Status: duckv1.Status{
							Conditions: []apis.Condition{
								{
									Type:   apis.ConditionSucceeded,
									Status: corev1.ConditionUnknown,
									Reason: pod.ReasonExceededNodeResources,
								},
							},
						},
					},
				},
			},
			expectedRC: true,
		},
		{
			name:  "not done throttle label set",
			oldPR: &v1.PipelineRun{},
			newPR: &v1.PipelineRun{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{THROTTLED_LABEL: "taskrun1"},
				},
				Status: v1.PipelineRunStatus{
					Status: duckv1.Status{
						Conditions: []apis.Condition{
							{
								Type:   apis.ConditionSucceeded,
								Status: corev1.ConditionUnknown,
							},
						},
					},
					PipelineRunStatusFields: v1.PipelineRunStatusFields{
						ChildReferences: []v1.ChildStatusReference{
							{
								TypeMeta: runtime.TypeMeta{Kind: "TaskRun"},
								Name:     "taskrun1",
							},
						},
					},
				},
			},
		},
		{
			name:  "just done succeed but previously throttled",
			oldPR: &v1.PipelineRun{},
			newPR: &v1.PipelineRun{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{THROTTLED_LABEL: "taskrun1"},
				},
				Status: v1.PipelineRunStatus{
					Status: duckv1.Status{
						Conditions: []apis.Condition{
							{
								Type:   apis.ConditionSucceeded,
								Status: corev1.ConditionTrue,
							},
						},
					},
				},
			},
		},
		{
			name:  "just done succeed",
			oldPR: &v1.PipelineRun{},
			newPR: &v1.PipelineRun{
				Status: v1.PipelineRunStatus{
					Status: duckv1.Status{
						Conditions: []apis.Condition{
							{
								Type:   apis.ConditionSucceeded,
								Status: corev1.ConditionTrue,
							},
						},
					},
				},
			},
			expectedRC: true,
		},
		{
			name:  "just done failed",
			oldPR: &v1.PipelineRun{},
			newPR: &v1.PipelineRun{
				Status: v1.PipelineRunStatus{
					Status: duckv1.Status{
						Conditions: []apis.Condition{
							{
								Type:   apis.ConditionSucceeded,
								Status: corev1.ConditionFalse,
							},
						},
					},
				},
			},
			expectedRC: true,
		},
		{
			name: "udpate after done",
			oldPR: &v1.PipelineRun{
				Status: v1.PipelineRunStatus{
					Status: duckv1.Status{
						Conditions: []apis.Condition{
							{
								Type:   apis.ConditionSucceeded,
								Status: corev1.ConditionFalse,
							},
						},
					},
				},
			},
			newPR: &v1.PipelineRun{
				Status: v1.PipelineRunStatus{
					Status: duckv1.Status{
						Conditions: []apis.Condition{
							{
								Type:   apis.ConditionSucceeded,
								Status: corev1.ConditionFalse,
							},
						},
					},
				},
			},
		},
	} {
		ctx := context.TODO()
		for _, tr := range tc.newTRs {
			err := c.Create(ctx, tr)
			assert.NoError(t, err)
		}
		ev := event.UpdateEvent{
			ObjectOld: tc.oldPR,
			ObjectNew: tc.newPR,
		}
		rc := filterObj.Update(ev)
		if rc != tc.expectedRC {
			t.Errorf(fmt.Sprintf("tc %s expected %v but got %v", tc.name, tc.expectedRC, rc))
		}
	}

}

func TestReconcileOverhead_Reconcile(t *testing.T) {
	// rather than using golang mocks, grabbed actual RHTAP pipelinerun/taskruns from staging
	// to drive the gap metric, given its trickiness
	objs := []client.Object{}
	scheme := runtime.NewScheme()
	_ = v1.AddToScheme(scheme)
	_ = v1beta1.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	overheadReconciler := buildReconciler(c, nil, nil)
	var err error
	// first we test with the samples pulled from actual RHTAP yaml to best capture the parallel task executions
	prs := []v1beta1.PipelineRun{}
	trs := []v1beta1.TaskRun{}
	prs, err = pipelineRunFromActualRHTAPYaml()
	if err != nil {
		t.Fatalf(fmt.Sprintf("%s", err.Error()))
	}
	trs, err = taskRunsFromActualRHTAPYaml()
	if err != nil {
		t.Fatalf(fmt.Sprintf("%s", err.Error()))
	}

	ctx := context.TODO()
	for _, trv1beta1 := range trs {
		// mimic what the tekton conversion webhook will do
		tr := &v1.TaskRun{}
		err = trv1beta1.ConvertTo(ctx, tr)
		assert.NoError(t, err)
		err = c.Create(ctx, tr)
		assert.NoError(t, err)
	}
	for index, prv1beta1 := range prs {
		// mimic what the tekton conversion webhook will do
		pr := &v1.PipelineRun{}
		err = prv1beta1.ConvertTo(ctx, pr)
		assert.NoError(t, err)
		err = c.Create(ctx, pr)
		assert.NoError(t, err)
		request := reconcile.Request{
			NamespacedName: types.NamespacedName{
				Namespace: pr.Namespace,
				Name:      pr.Name,
			},
		}
		_, err = overheadReconciler.Reconcile(ctx, request)
		label := prometheus.Labels{NS_LABEL: pr.Namespace, STATUS_LABEL: SUCCEEDED}
		// with our actual RHTAP samples the first entry had 0 scheduling overhead so we created a metric,
		// but the rest was filtered
		var observer prometheus.Observer
		var histogram prometheus.Histogram
		var metric *dto.Metric
		if index == 0 {
			validateHistogramVec(t, overheadReconciler.overheadCollector.scheduling, label, true)
		} else {
			observer, err = overheadReconciler.overheadCollector.scheduling.GetMetricWith(label)
			assert.NoError(t, err)
			assert.NotNil(t, observer)
			histogram = observer.(prometheus.Histogram)
			metric = &dto.Metric{}
			histogram.Write(metric)
			assert.NotNil(t, metric.Histogram)
			assert.NotNil(t, metric.Histogram.SampleCount)
			assert.Equal(t, *metric.Histogram.SampleCount, uint64(0))
		}
		observer, err = overheadReconciler.overheadCollector.execution.GetMetricWith(label)
		assert.NoError(t, err)
		assert.NotNil(t, observer)
		histogram = observer.(prometheus.Histogram)
		metric = &dto.Metric{}
		histogram.Write(metric)
		assert.NotNil(t, metric.Histogram)
		assert.NotNil(t, metric.Histogram.SampleCount)
		assert.Equal(t, *metric.Histogram.SampleCount, uint64(0))

	}
	unregisterStats(overheadReconciler)
}

func TestReconcileOverhead_Reconcile_MissingTaskRuns(t *testing.T) {
	// rather than using golang mocks, grabbed actual RHTAP pipelinerun/taskruns from staging
	// to drive the gap metric, given its trickiness
	objs := []client.Object{}
	scheme := runtime.NewScheme()
	_ = v1.AddToScheme(scheme)
	_ = v1beta1.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	overheadReconciler := buildReconciler(c, nil, nil)
	var err error
	// first we test with the samples pulled from actual RHTAP yaml to best capture the parallel task executions
	prs := []v1beta1.PipelineRun{}
	prs, err = pipelineRunFromActualRHTAPYaml()
	if err != nil {
		t.Fatalf(fmt.Sprintf("%s", err.Error()))
	}
	// but in this test we make sure no stats are generated if the taskruns are missing

	ctx := context.TODO()
	for _, prv1beta1 := range prs {
		// mimic what the tekton conversion webhook will do
		pr := &v1.PipelineRun{}
		err = prv1beta1.ConvertTo(ctx, pr)
		assert.NoError(t, err)
		err = c.Create(ctx, pr)
		assert.NoError(t, err)
		request := reconcile.Request{
			NamespacedName: types.NamespacedName{
				Namespace: pr.Namespace,
				Name:      pr.Name,
			},
		}
		_, err = overheadReconciler.Reconcile(ctx, request)
		label := prometheus.Labels{NS_LABEL: pr.Namespace, STATUS_LABEL: SUCCEEDED}
		validateHistogramVecZeroCount(t, overheadReconciler.overheadCollector.execution, label)
	}
	unregisterStats(overheadReconciler)

}

func TestReconcileOverhead_Reconcile_MockWithHighOverhead(t *testing.T) {
	// rather the golang mocks, grabbed actual RHTAP pipelinerun/taskruns from staging
	// to drive the gap metric, given its trickiness
	objs := []client.Object{}
	scheme := runtime.NewScheme()
	_ = v1.AddToScheme(scheme)
	_ = v1beta1.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	overheadReconciler := buildReconciler(c, nil, nil)

	var err error
	now := time.Now().UTC()

	// then some additional unit tests were we build simpler pipelineruns/taskruns that capture paths
	// related to completion times not being set
	mockTaskRuns := []*v1.TaskRun{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "test-taskrun-1",
				Namespace:         "test-namespace",
				UID:               types.UID("test-taskrun-1"),
				CreationTimestamp: metav1.NewTime(now),
				Labels:            map[string]string{pipeline.TaskLabelKey: "test-task-1"},
			},
			Status: v1.TaskRunStatus{
				Status: duckv1.Status{
					ObservedGeneration: 0,
					Conditions: duckv1.Conditions{{
						Type:   "Succeeded",
						Status: corev1.ConditionTrue,
					}},
					Annotations: nil,
				},
				TaskRunStatusFields: v1.TaskRunStatusFields{
					StartTime: &metav1.Time{Time: now},
				},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "test-taskrun-2",
				Namespace:         "test-namespace",
				UID:               types.UID("test-taskrun-2"),
				CreationTimestamp: metav1.NewTime(now.Add(5 * time.Second)),
				Labels:            map[string]string{pipeline.TaskLabelKey: "test-task-2"},
			},
			Status: v1.TaskRunStatus{
				Status: duckv1.Status{
					ObservedGeneration: 0,
					Conditions: duckv1.Conditions{{
						Type:   "Succeeded",
						Status: corev1.ConditionTrue,
					}},
					Annotations: nil,
				},
				TaskRunStatusFields: v1.TaskRunStatusFields{
					StartTime: &metav1.Time{Time: now.Add(10 * time.Second)},
				},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "test-taskrun-3",
				Namespace:         "test-namespace",
				UID:               types.UID("test-taskrun-3"),
				CreationTimestamp: metav1.NewTime(now.Add(20 * time.Second)),
				Labels:            map[string]string{pipeline.TaskLabelKey: "test-task-3"},
			},
			Status: v1.TaskRunStatus{
				Status: duckv1.Status{
					ObservedGeneration: 0,
					Conditions: duckv1.Conditions{{
						Type:   "Succeeded",
						Status: corev1.ConditionUnknown,
					}},
					Annotations: nil,
				},
				TaskRunStatusFields: v1.TaskRunStatusFields{
					StartTime: &metav1.Time{Time: now.Add(25 * time.Second)},
				},
			},
		},
	}
	mockPipelineRuns := []*v1.PipelineRun{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "test-pipelinerun-4",
				Namespace:         "test-namespace",
				UID:               types.UID("test-pipelinerun-4"),
				CreationTimestamp: metav1.NewTime(now),
			},
			Spec: v1.PipelineRunSpec{PipelineRef: &v1.PipelineRef{Name: "test-pipeline"}},
			Status: v1.PipelineRunStatus{
				Status: duckv1.Status{
					ObservedGeneration: 0,
					Conditions: duckv1.Conditions{{
						Type:   "Succeeded",
						Status: corev1.ConditionTrue,
					}},
					Annotations: nil,
				},
				PipelineRunStatusFields: v1.PipelineRunStatusFields{
					ChildReferences: []v1.ChildStatusReference{
						{
							TypeMeta: runtime.TypeMeta{
								Kind: "TaskRun",
							},
							Name: "test-taskrun-1",
						},
						{
							TypeMeta: runtime.TypeMeta{
								Kind: "TaskRun",
							},
							Name: "test-taskrun-2",
						},
						{
							TypeMeta: runtime.TypeMeta{
								Kind: "TaskRun",
							},
							Name: "test-taskrun-3",
						},
					},
					StartTime:      &metav1.Time{Time: now.Add(5 * time.Second)},
					CompletionTime: &metav1.Time{Time: now.Add(6 * time.Minute)},
				},
			},
		},
	}
	ctx := context.TODO()
	for _, tr := range mockTaskRuns {
		err = c.Create(ctx, tr)
		assert.NoError(t, err)
	}
	for _, pipelineRun := range mockPipelineRuns {
		err = c.Create(ctx, pipelineRun)
		assert.NoError(t, err)
		request := reconcile.Request{
			NamespacedName: types.NamespacedName{
				Namespace: pipelineRun.Namespace,
				Name:      pipelineRun.Name,
			},
		}
		_, err = overheadReconciler.Reconcile(ctx, request)
	}

	label := prometheus.Labels{NS_LABEL: "test-namespace", STATUS_LABEL: SUCCEEDED}
	validateHistogramVec(t, overheadReconciler.overheadCollector.execution, label, false)
	unregisterStats(overheadReconciler)
}

func TestReconcileOverhead_Reconcile_MockWithHighOverheadButThrottled(t *testing.T) {
	// rather the golang mocks, grabbed actual RHTAP pipelinerun/taskruns from staging
	// to drive the gap metric, given its trickiness
	objs := []client.Object{}
	scheme := runtime.NewScheme()
	_ = v1.AddToScheme(scheme)
	_ = v1beta1.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	overheadReconciler := buildReconciler(c, nil, nil)

	var err error
	now := time.Now().UTC()

	mockTaskRuns := []*v1.TaskRun{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "test-taskrun-1",
				Namespace:         "test-namespace",
				UID:               types.UID("test-taskrun-1"),
				CreationTimestamp: metav1.NewTime(now),
				Labels:            map[string]string{pipeline.TaskLabelKey: "test-task-1"},
			},
			Status: v1.TaskRunStatus{
				Status: duckv1.Status{
					ObservedGeneration: 0,
					Conditions: duckv1.Conditions{{
						Type:   "Succeeded",
						Status: corev1.ConditionTrue,
					}},
					Annotations: nil,
				},
				TaskRunStatusFields: v1.TaskRunStatusFields{
					StartTime: &metav1.Time{Time: now},
				},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "test-taskrun-2",
				Namespace:         "test-namespace",
				UID:               types.UID("test-taskrun-2"),
				CreationTimestamp: metav1.NewTime(now.Add(5 * time.Second)),
				Labels:            map[string]string{pipeline.TaskLabelKey: "test-task-2"},
			},
			Status: v1.TaskRunStatus{
				Status: duckv1.Status{
					ObservedGeneration: 0,
					Conditions: duckv1.Conditions{{
						Type:   "Succeeded",
						Status: corev1.ConditionTrue,
					}},
					Annotations: nil,
				},
				TaskRunStatusFields: v1.TaskRunStatusFields{
					StartTime: &metav1.Time{Time: now.Add(10 * time.Second)},
				},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "test-taskrun-3",
				Namespace:         "test-namespace",
				UID:               types.UID("test-taskrun-3"),
				CreationTimestamp: metav1.NewTime(now.Add(20 * time.Second)),
				Labels:            map[string]string{pipeline.TaskLabelKey: "test-task-3"},
			},
			Status: v1.TaskRunStatus{
				Status: duckv1.Status{
					ObservedGeneration: 0,
					Conditions: duckv1.Conditions{{
						Type:   "Succeeded",
						Status: corev1.ConditionUnknown,
						Reason: pod.ReasonExceededNodeResources,
					}},
					Annotations: nil,
				},
				TaskRunStatusFields: v1.TaskRunStatusFields{
					StartTime: &metav1.Time{Time: now.Add(25 * time.Second)},
				},
			},
		},
	}
	mockPipelineRuns := []*v1.PipelineRun{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "test-pipelinerun-4",
				Namespace:         "test-namespace",
				UID:               types.UID("test-pipelinerun-4"),
				CreationTimestamp: metav1.NewTime(now),
				Labels:            map[string]string{THROTTLED_LABEL: "test-taskrun-3"},
			},
			Spec: v1.PipelineRunSpec{PipelineRef: &v1.PipelineRef{Name: "test-pipeline"}},
			Status: v1.PipelineRunStatus{
				Status: duckv1.Status{
					ObservedGeneration: 0,
					Conditions: duckv1.Conditions{{
						Type:   "Succeeded",
						Status: corev1.ConditionTrue,
					}},
					Annotations: nil,
				},
				PipelineRunStatusFields: v1.PipelineRunStatusFields{
					ChildReferences: []v1.ChildStatusReference{
						{
							TypeMeta: runtime.TypeMeta{
								Kind: "TaskRun",
							},
							Name: "test-taskrun-1",
						},
						{
							TypeMeta: runtime.TypeMeta{
								Kind: "TaskRun",
							},
							Name: "test-taskrun-2",
						},
						{
							TypeMeta: runtime.TypeMeta{
								Kind: "TaskRun",
							},
							Name: "test-taskrun-3",
						},
					},
					StartTime:      &metav1.Time{Time: now.Add(5 * time.Second)},
					CompletionTime: &metav1.Time{Time: now.Add(6 * time.Minute)},
				},
			},
		},
	}
	ctx := context.TODO()
	for _, tr := range mockTaskRuns {
		err = c.Create(ctx, tr)
		assert.NoError(t, err)
	}
	for _, pipelineRun := range mockPipelineRuns {
		err = c.Create(ctx, pipelineRun)
		assert.NoError(t, err)
		request := reconcile.Request{
			NamespacedName: types.NamespacedName{
				Namespace: pipelineRun.Namespace,
				Name:      pipelineRun.Name,
			},
		}
		_, err = overheadReconciler.Reconcile(ctx, request)
		err = c.Get(ctx, types.NamespacedName{Namespace: pipelineRun.Namespace, Name: pipelineRun.Name}, pipelineRun)
		assert.NoError(t, err)
		_, throttled := pipelineRun.Labels[THROTTLED_LABEL]
		assert.True(t, throttled)
	}

	label := prometheus.Labels{NS_LABEL: "test-namespace", STATUS_LABEL: SUCCEEDED}
	validateHistogramVecZeroCount(t, overheadReconciler.overheadCollector.execution, label)
	unregisterStats(overheadReconciler)

}
