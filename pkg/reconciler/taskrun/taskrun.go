/*
Copyright 2019 The Tekton Authors

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

package taskrun

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/hashicorp/go-multierror"
	"github.com/tektoncd/pipeline/pkg/apis/config"
	"github.com/tektoncd/pipeline/pkg/apis/pipeline"
	"github.com/tektoncd/pipeline/pkg/apis/pipeline/v1alpha1"
	listers "github.com/tektoncd/pipeline/pkg/client/listers/pipeline/v1alpha1"
	resourcelisters "github.com/tektoncd/pipeline/pkg/client/resource/listers/resource/v1alpha1"
	"github.com/tektoncd/pipeline/pkg/contexts"
	podconvert "github.com/tektoncd/pipeline/pkg/pod"
	"github.com/tektoncd/pipeline/pkg/reconciler"
	"github.com/tektoncd/pipeline/pkg/reconciler/taskrun/resources"
	"github.com/tektoncd/pipeline/pkg/reconciler/taskrun/resources/cloudevent"
	"github.com/tektoncd/pipeline/pkg/termination"
	"github.com/tektoncd/pipeline/pkg/workspace"
	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"
	"knative.dev/pkg/apis"
	"knative.dev/pkg/controller"
	"knative.dev/pkg/tracker"
)

const (
	// taskRunAgentName defines logging agent name for TaskRun Controller
	taskRunAgentName = "taskrun-controller"
)

// Reconciler implements controller.Reconciler for Configuration resources.
type Reconciler struct {
	*reconciler.Base

	// listers index properties about resources
	taskRunLister     listers.TaskRunLister
	taskLister        listers.TaskLister
	clusterTaskLister listers.ClusterTaskLister
	resourceLister    resourcelisters.PipelineResourceLister
	cloudEventClient  cloudevent.CEClient
	tracker           tracker.Interface
	entrypointCache   podconvert.EntrypointCache
	timeoutHandler    *reconciler.TimeoutSet
	metrics           *Recorder
}

// Check that our Reconciler implements controller.Reconciler
var _ controller.Reconciler = (*Reconciler)(nil)

// Reconcile compares the actual state with the desired, and attempts to
// converge the two. It then updates the Status block of the Task Run
// resource with the current status of the resource.
func (c *Reconciler) Reconcile(ctx context.Context, key string) error {
	// In case of reconcile errors, we store the error in a multierror, attempt
	// to update, and return the original error combined with any update error
	var merr error

	// Convert the namespace/name string into a distinct namespace and name
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		c.Logger.Errorf("invalid resource key: %s", key)
		return nil
	}

	// Get the Task Run resource with this namespace/name
	original, err := c.taskRunLister.TaskRuns(namespace).Get(name)
	if errors.IsNotFound(err) {
		// The resource no longer exists, in which case we stop processing.
		c.Logger.Infof("task run %q in work queue no longer exists", key)
		return nil
	} else if err != nil {
		c.Logger.Errorf("Error retrieving TaskRun %q: %s", name, err)
		return err
	}

	// Don't modify the informer's copy.
	tr := original.DeepCopy()

	// If the TaskRun is just starting, this will also set the starttime,
	// from which the timeout will immediately begin counting down.
	tr.Status.InitializeConditions()
	// In case node time was not synchronized, when controller has been scheduled to other nodes.
	if tr.Status.StartTime.Sub(tr.CreationTimestamp.Time) < 0 {
		c.Logger.Warnf("TaskRun %s createTimestamp %s is after the taskRun started %s", tr.GetRunKey(), tr.CreationTimestamp, tr.Status.StartTime)
		tr.Status.StartTime = &tr.CreationTimestamp
	}

	if tr.IsDone() {
		c.Logger.Infof("taskrun done : %s \n", tr.Name)
		var merr *multierror.Error
		// Try to send cloud events first
		cloudEventErr := cloudevent.SendCloudEvents(tr, c.cloudEventClient, c.Logger)
		// Regardless of `err`, we must write back any status update that may have
		// been generated by `sendCloudEvents`
		updateErr := c.updateStatusLabelsAndAnnotations(tr, original)
		merr = multierror.Append(cloudEventErr, updateErr)
		if cloudEventErr != nil {
			// Let's keep timeouts and sidecars running as long as we're trying to
			// send cloud events. So we stop here an return errors encountered this far.
			return merr.ErrorOrNil()
		}
		c.timeoutHandler.Release(tr)
		pod, err := c.KubeClientSet.CoreV1().Pods(tr.Namespace).Get(tr.Status.PodName, metav1.GetOptions{})
		if err == nil {
			err = podconvert.StopSidecars(c.Images.NopImage, c.KubeClientSet, *pod)
		} else if errors.IsNotFound(err) {
			return merr.ErrorOrNil()
		}
		if err != nil {
			c.Logger.Errorf("Error stopping sidecars for TaskRun %q: %v", name, err)
			merr = multierror.Append(merr, err)
		}

		go func(metrics *Recorder) {
			err := metrics.DurationAndCount(tr)
			if err != nil {
				c.Logger.Warnf("Failed to log the metrics : %v", err)
			}
			err = metrics.RecordPodLatency(pod, tr)
			if err != nil {
				c.Logger.Warnf("Failed to log the metrics : %v", err)
			}
		}(c.metrics)

		return merr.ErrorOrNil()
	}
	// Reconcile this copy of the task run and then write back any status
	// updates regardless of whether the reconciliation errored out.
	if err := c.reconcile(ctx, tr); err != nil {
		c.Logger.Errorf("Reconcile error: %v", err.Error())
		merr = multierror.Append(merr, err)
	}
	return multierror.Append(merr, c.updateStatusLabelsAndAnnotations(tr, original)).ErrorOrNil()
}

func (c *Reconciler) updateStatusLabelsAndAnnotations(tr, original *v1alpha1.TaskRun) error {
	var updated bool

	if !equality.Semantic.DeepEqual(original.Status, tr.Status) {
		// If we didn't change anything then don't call updateStatus.
		// This is important because the copy we loaded from the informer's
		// cache may be stale and we don't want to overwrite a prior update
		// to status with this stale state.
		if _, err := c.updateStatus(tr); err != nil {
			c.Logger.Warn("Failed to update taskRun status", zap.Error(err))
			return err
		}
		updated = true
	}

	// Since we are using the status subresource, it is not possible to update
	// the status and labels/annotations simultaneously.
	if !reflect.DeepEqual(original.ObjectMeta.Labels, tr.ObjectMeta.Labels) || !reflect.DeepEqual(original.ObjectMeta.Annotations, tr.ObjectMeta.Annotations) {
		if _, err := c.updateLabelsAndAnnotations(tr); err != nil {
			c.Logger.Warn("Failed to update TaskRun labels/annotations", zap.Error(err))
			return err
		}
		updated = true
	}

	if updated {
		go func(metrics *Recorder) {
			err := metrics.RunningTaskRuns(c.taskRunLister)
			if err != nil {
				c.Logger.Warnf("Failed to log the metrics : %v", err)
			}
		}(c.metrics)
	}

	return nil
}

func (c *Reconciler) getTaskFunc(tr *v1alpha1.TaskRun) (resources.GetTask, v1alpha1.TaskKind) {
	var gtFunc resources.GetTask
	kind := v1alpha1.NamespacedTaskKind
	if tr.Spec.TaskRef != nil && tr.Spec.TaskRef.Kind == v1alpha1.ClusterTaskKind {
		gtFunc = func(name string) (v1alpha1.TaskInterface, error) {
			t, err := c.PipelineClientSet.TektonV1alpha1().ClusterTasks().Get(name, metav1.GetOptions{})
			if err != nil {
				return nil, err
			}
			return t, nil
		}
		kind = v1alpha1.ClusterTaskKind
	} else {
		gtFunc = func(name string) (v1alpha1.TaskInterface, error) {
			t, err := c.PipelineClientSet.TektonV1alpha1().Tasks(tr.Namespace).Get(name, metav1.GetOptions{})
			if err != nil {
				return nil, err
			}
			return t, nil
		}
	}
	return gtFunc, kind
}

func (c *Reconciler) reconcile(ctx context.Context, tr *v1alpha1.TaskRun) error {
	// We may be reading a version of the object that was stored at an older version
	// and may not have had all of the assumed default specified.
	tr.SetDefaults(contexts.WithUpgradeViaDefaulting(ctx))

	// If the taskrun is cancelled, kill resources and update status
	if tr.IsCancelled() {
		before := tr.Status.GetCondition(apis.ConditionSucceeded)
		err := cancelTaskRun(tr, c.KubeClientSet, c.Logger)
		after := tr.Status.GetCondition(apis.ConditionSucceeded)
		reconciler.EmitEvent(c.Recorder, before, after, tr)
		return err
	}

	getTaskFunc, kind := c.getTaskFunc(tr)
	taskMeta, taskSpec, err := resources.GetTaskData(tr, getTaskFunc)
	if err != nil {
		c.Logger.Errorf("Failed to determine Task spec to use for taskrun %s: %v", tr.Name, err)
		tr.Status.SetCondition(&apis.Condition{
			Type:    apis.ConditionSucceeded,
			Status:  corev1.ConditionFalse,
			Reason:  podconvert.ReasonFailedResolution,
			Message: err.Error(),
		})
		return nil
	}

	// Propagate labels from Task to TaskRun.
	if tr.ObjectMeta.Labels == nil {
		tr.ObjectMeta.Labels = make(map[string]string, len(taskMeta.Labels)+1)
	}
	for key, value := range taskMeta.Labels {
		tr.ObjectMeta.Labels[key] = value
	}
	if tr.Spec.TaskRef != nil {
		tr.ObjectMeta.Labels[pipeline.GroupName+pipeline.TaskLabelKey] = taskMeta.Name
	}

	// Propagate annotations from Task to TaskRun.
	if tr.ObjectMeta.Annotations == nil {
		tr.ObjectMeta.Annotations = make(map[string]string, len(taskMeta.Annotations))
	}
	for key, value := range taskMeta.Annotations {
		tr.ObjectMeta.Annotations[key] = value
	}

	if tr.Spec.Timeout == nil {
		tr.Spec.Timeout = &metav1.Duration{Duration: config.DefaultTimeoutMinutes * time.Minute}
	}
	// Check if the TaskRun has timed out; if it is, this will set its status
	// accordingly.
	if CheckTimeout(tr) {
		if err := c.updateTaskRunStatusForTimeout(tr, c.KubeClientSet.CoreV1().Pods(tr.Namespace).Delete); err != nil {
			return err
		}
		return nil
	}

	rtr, err := resources.ResolveTaskResources(taskSpec, taskMeta.Name, kind, tr.Spec.Inputs.Resources, tr.Spec.Outputs.Resources, c.resourceLister.PipelineResources(tr.Namespace).Get)
	if err != nil {
		c.Logger.Errorf("Failed to resolve references for taskrun %s: %v", tr.Name, err)
		tr.Status.SetCondition(&apis.Condition{
			Type:    apis.ConditionSucceeded,
			Status:  corev1.ConditionFalse,
			Reason:  podconvert.ReasonFailedResolution,
			Message: err.Error(),
		})
		return nil
	}

	if err := ValidateResolvedTaskResources(tr.Spec.Inputs.Params, rtr); err != nil {
		c.Logger.Errorf("TaskRun %q resources are invalid: %v", tr.Name, err)
		tr.Status.SetCondition(&apis.Condition{
			Type:    apis.ConditionSucceeded,
			Status:  corev1.ConditionFalse,
			Reason:  podconvert.ReasonFailedValidation,
			Message: err.Error(),
		})
		return nil
	}

	if err := workspace.ValidateBindings(taskSpec.Workspaces, tr.Spec.Workspaces); err != nil {
		c.Logger.Errorf("TaskRun %q workspaces are invalid: %v", tr.Name, err)
		tr.Status.SetCondition(&apis.Condition{
			Type:    apis.ConditionSucceeded,
			Status:  corev1.ConditionFalse,
			Reason:  podconvert.ReasonFailedValidation,
			Message: err.Error(),
		})
	}

	// Initialize the cloud events if at least a CloudEventResource is defined
	// and they have not been initialized yet.
	// FIXME(afrittoli) This resource specific logic will have to be replaced
	// once we have a custom PipelineResource framework in place.
	c.Logger.Infof("Cloud Events: %s", tr.Status.CloudEvents)
	prs := make([]*v1alpha1.PipelineResource, 0, len(rtr.Outputs))
	for _, pr := range rtr.Outputs {
		prs = append(prs, pr)
	}
	cloudevent.InitializeCloudEvents(tr, prs)

	// Get the TaskRun's Pod if it should have one. Otherwise, create the Pod.
	var pod *corev1.Pod
	if tr.Status.PodName != "" {
		pod, err = c.KubeClientSet.CoreV1().Pods(tr.Namespace).Get(tr.Status.PodName, metav1.GetOptions{})
		if errors.IsNotFound(err) {
			// Keep going, this will result in the Pod being created below.
		} else if err != nil {
			c.Logger.Errorf("Error getting pod %q: %v", tr.Status.PodName, err)
			return err
		}
	}
	if pod == nil {
		pod, err = c.createPod(tr, rtr)
		if err != nil {
			c.handlePodCreationError(tr, err)
			return nil
		}
		go c.timeoutHandler.WaitTaskRun(tr, tr.Status.StartTime)
	}
	if err := c.tracker.Track(tr.GetBuildPodRef(), tr); err != nil {
		c.Logger.Errorf("Failed to create tracker for build pod %q for taskrun %q: %v", tr.Name, tr.Name, err)
		return err
	}

	if podconvert.IsPodExceedingNodeResources(pod) {
		c.Recorder.Eventf(tr, corev1.EventTypeWarning, podconvert.ReasonExceededNodeResources, "Insufficient resources to schedule pod %q", pod.Name)
	}

	if podconvert.SidecarsReady(pod.Status) {
		if err := podconvert.UpdateReady(c.KubeClientSet, *pod); err != nil {
			return err
		}
	}

	before := tr.Status.GetCondition(apis.ConditionSucceeded)

	// Convert the Pod's status to the equivalent TaskRun Status.
	tr.Status = podconvert.MakeTaskRunStatus(*tr, pod, *taskSpec)

	if err := updateTaskRunResourceResult(tr, pod.Status); err != nil {
		return err
	}

	after := tr.Status.GetCondition(apis.ConditionSucceeded)

	reconciler.EmitEvent(c.Recorder, before, after, tr)
	c.Logger.Infof("Successfully reconciled taskrun %s/%s with status: %#v", tr.Name, tr.Namespace, after)

	return nil
}

func (c *Reconciler) handlePodCreationError(tr *v1alpha1.TaskRun, err error) {
	var reason, msg string
	var succeededStatus corev1.ConditionStatus
	if isExceededResourceQuotaError(err) {
		succeededStatus = corev1.ConditionUnknown
		reason = podconvert.ReasonExceededResourceQuota
		backoff, currentlyBackingOff := c.timeoutHandler.GetBackoff(tr)
		if !currentlyBackingOff {
			go c.timeoutHandler.SetTaskRunTimer(tr, time.Until(backoff.NextAttempt))
		}
		msg = fmt.Sprintf("TaskRun Pod exceeded available resources, reattempted %d times", backoff.NumAttempts)
	} else {
		succeededStatus = corev1.ConditionFalse
		reason = podconvert.ReasonCouldntGetTask
		if tr.Spec.TaskRef != nil {
			msg = fmt.Sprintf("Missing or invalid Task %s/%s", tr.Namespace, tr.Spec.TaskRef.Name)
		} else {
			msg = fmt.Sprintf("Invalid TaskSpec")
		}
	}
	tr.Status.SetCondition(&apis.Condition{
		Type:    apis.ConditionSucceeded,
		Status:  succeededStatus,
		Reason:  reason,
		Message: fmt.Sprintf("%s: %v", msg, err),
	})
	c.Recorder.Eventf(tr, corev1.EventTypeWarning, "BuildCreationFailed", "Failed to create build pod %q: %v", tr.Name, err)
	c.Logger.Errorf("Failed to create build pod for task %q: %v", tr.Name, err)
}

func updateTaskRunResourceResult(taskRun *v1alpha1.TaskRun, podStatus corev1.PodStatus) error {
	if taskRun.IsSuccessful() {
		for idx, cs := range podStatus.ContainerStatuses {
			if cs.State.Terminated != nil {
				msg := cs.State.Terminated.Message
				r, err := termination.ParseMessage(msg)
				if err != nil {
					return fmt.Errorf("parsing message for container status %d: %v", idx, err)
				}
				taskRun.Status.ResourcesResult = append(taskRun.Status.ResourcesResult, r...)
			}
		}
	}
	return nil
}

func (c *Reconciler) updateStatus(taskrun *v1alpha1.TaskRun) (*v1alpha1.TaskRun, error) {
	newtaskrun, err := c.taskRunLister.TaskRuns(taskrun.Namespace).Get(taskrun.Name)
	if err != nil {
		return nil, fmt.Errorf("error getting TaskRun %s when updating status: %w", taskrun.Name, err)
	}
	if !reflect.DeepEqual(taskrun.Status, newtaskrun.Status) {
		newtaskrun.Status = taskrun.Status
		return c.PipelineClientSet.TektonV1alpha1().TaskRuns(taskrun.Namespace).UpdateStatus(newtaskrun)
	}
	return newtaskrun, nil
}

func (c *Reconciler) updateLabelsAndAnnotations(tr *v1alpha1.TaskRun) (*v1alpha1.TaskRun, error) {
	newTr, err := c.taskRunLister.TaskRuns(tr.Namespace).Get(tr.Name)
	if err != nil {
		return nil, fmt.Errorf("error getting TaskRun %s when updating labels/annotations: %w", tr.Name, err)
	}
	if !reflect.DeepEqual(tr.ObjectMeta.Labels, newTr.ObjectMeta.Labels) || !reflect.DeepEqual(tr.ObjectMeta.Annotations, newTr.ObjectMeta.Annotations) {
		newTr.ObjectMeta.Labels = tr.ObjectMeta.Labels
		newTr.ObjectMeta.Annotations = tr.ObjectMeta.Annotations
		return c.PipelineClientSet.TektonV1alpha1().TaskRuns(tr.Namespace).Update(newTr)
	}
	return newTr, nil
}

// createPod creates a Pod based on the Task's configuration, with pvcName as a volumeMount
// TODO(dibyom): Refactor resource setup/substitution logic to its own function in the resources package
func (c *Reconciler) createPod(tr *v1alpha1.TaskRun, rtr *resources.ResolvedTaskResources) (*corev1.Pod, error) {
	ts := rtr.TaskSpec.DeepCopy()
	inputResources, err := resourceImplBinding(rtr.Inputs, c.Images)
	if err != nil {
		c.Logger.Errorf("Failed to initialize input resources: %v", err)
		return nil, err
	}
	outputResources, err := resourceImplBinding(rtr.Outputs, c.Images)
	if err != nil {
		c.Logger.Errorf("Failed to initialize output resources: %v", err)
		return nil, err
	}

	// Get actual resource

	err = resources.AddOutputImageDigestExporter(c.Images.ImageDigestExporterImage, tr, ts, c.resourceLister.PipelineResources(tr.Namespace).Get)
	if err != nil {
		c.Logger.Errorf("Failed to create a pod for taskrun: %s due to output image resource error %v", tr.Name, err)
		return nil, err
	}

	ts, err = resources.AddInputResource(c.KubeClientSet, c.Images, rtr.TaskName, ts, tr, inputResources, c.Logger)
	if err != nil {
		c.Logger.Errorf("Failed to create a pod for taskrun: %s due to input resource error %v", tr.Name, err)
		return nil, err
	}

	ts, err = resources.AddOutputResources(c.KubeClientSet, c.Images, rtr.TaskName, ts, tr, outputResources, c.Logger)
	if err != nil {
		c.Logger.Errorf("Failed to create a pod for taskrun: %s due to output resource error %v", tr.Name, err)
		return nil, err
	}

	var defaults []v1alpha1.ParamSpec
	if ts.Inputs != nil {
		defaults = append(defaults, ts.Inputs.Params...)
	}
	// Apply parameter substitution from the taskrun.
	ts = resources.ApplyParameters(ts, tr, defaults...)

	// Apply bound resource substitution from the taskrun.
	ts = resources.ApplyResources(ts, inputResources, "inputs")
	ts = resources.ApplyResources(ts, outputResources, "outputs")

	// Apply workspace resource substitution
	ts = resources.ApplyWorkspaces(ts, ts.Workspaces, tr.Spec.Workspaces)

	ts, err = workspace.Apply(*ts, tr.Spec.Workspaces)
	if err != nil {
		c.Logger.Errorf("Failed to create a pod for taskrun: %s due to workspace error %v", tr.Name, err)
		return nil, err
	}

	pod, err := podconvert.MakePod(c.Images, tr, *ts, c.KubeClientSet, c.entrypointCache)
	if err != nil {
		return nil, fmt.Errorf("translating Build to Pod: %w", err)
	}

	c.Logger.Error("aaaaaaaaaaaa", pod)
	pod.Spec.HostAliases = append(pod.Spec.HostAliases, corev1.HostAlias{
		IP:        "10.193.28.1",
		Hostnames: []string{
			"registry.vivo.bj04.xyz",
		},
	})
	c.Logger.Error("bbbbbbbbbbb", pod)

	return c.KubeClientSet.CoreV1().Pods(tr.Namespace).Create(pod)
}

type DeletePod func(podName string, options *metav1.DeleteOptions) error

func (c *Reconciler) updateTaskRunStatusForTimeout(tr *v1alpha1.TaskRun, dp DeletePod) error {
	c.Logger.Infof("TaskRun %q has timed out, deleting pod", tr.Name)
	// tr.Status.PodName will be empty if the pod was never successfully created. This condition
	// can be reached, for example, by the pod never being schedulable due to limits imposed by
	// a namespace's ResourceQuota.
	if tr.Status.PodName != "" {
		if err := dp(tr.Status.PodName, &metav1.DeleteOptions{}); err != nil && !errors.IsNotFound(err) {
			c.Logger.Errorf("Failed to terminate pod: %v", err)
			return err
		}
	}

	timeout := tr.Spec.Timeout.Duration
	timeoutMsg := fmt.Sprintf("TaskRun %q failed to finish within %q", tr.Name, timeout.String())
	tr.Status.SetCondition(&apis.Condition{
		Type:    apis.ConditionSucceeded,
		Status:  corev1.ConditionFalse,
		Reason:  podconvert.ReasonTimedOut,
		Message: timeoutMsg,
	})
	// update tr completed time
	tr.Status.CompletionTime = &metav1.Time{Time: time.Now()}
	return nil
}

func isExceededResourceQuotaError(err error) bool {
	return err != nil && errors.IsForbidden(err) && strings.Contains(err.Error(), "exceeded quota")
}

// resourceImplBinding maps pipeline resource names to the actual resource type implementations
func resourceImplBinding(resources map[string]*v1alpha1.PipelineResource, images pipeline.Images) (map[string]v1alpha1.PipelineResourceInterface, error) {
	p := make(map[string]v1alpha1.PipelineResourceInterface)
	for rName, r := range resources {
		i, err := v1alpha1.ResourceFromType(r, images)
		if err != nil {
			return nil, fmt.Errorf("failed to create resource %s : %v with error: %w", rName, r, err)
		}
		p[rName] = i
	}
	return p, nil
}
