package controllers

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	kataconfigurationv1 "github.com/openshift/sandboxed-containers-operator/api/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	dispatcherOwnerLabel         = "kata-deploy-job-dispatcher/owner"
	dispatcherResultLabel        = "kata-deploy-job-dispatcher/result"
	dispatcherErrorAnnotation    = "kata-deploy-job-dispatcher/error"
	dispatcherFinishedAnnotation = "kata-deploy-job-dispatcher/finished-at"
)

// updateKataDeployStatus reports dispatcher work without consulting the MCO.
// Node results outlive the Jobs. Before a rollout converges, only results
// recorded after that rollout started belong to the desired generation.
func (r *KataConfigOpenShiftReconciler) updateKataDeployStatus() error {
	desired, err := r.dispatcherDesiredGeneration()
	if err != nil {
		return err
	}
	selectorString, err := r.dispatcherNodeSelector()
	if err != nil {
		return err
	}
	selector, err := labels.Parse(selectorString)
	if err != nil {
		return err
	}
	nodes := &corev1.NodeList{}
	if err := r.Client.List(context.TODO(), nodes); err != nil {
		return err
	}
	jobs := &batchv1.JobList{}
	if err := r.Client.List(context.TODO(), jobs, client.InNamespace(OperatorNamespace)); err != nil {
		return err
	}

	installPrefix := fmt.Sprintf("kata-%.12s", desired)
	cleanupPrefix := installPrefix + "-removed"
	deleting := !r.kataConfig.DeletionTimestamp.IsZero()
	if deleting {
		cleanupPrefix = fmt.Sprintf("kata-cleanup-%.8s", r.kataConfig.UID)
	}
	installJobs := map[string]*batchv1.Job{}
	cleanupJobs := map[string]*batchv1.Job{}
	var rolloutStarted metav1.Time
	for i := range jobs.Items {
		job := &jobs.Items[i]
		if job.Name == r.dispatcherJobName(desired) {
			for _, owner := range job.OwnerReferences {
				if owner.UID == r.kataConfig.UID {
					rolloutStarted = job.CreationTimestamp
				}
			}
		}
		nodeName := job.Spec.Template.Spec.NodeName
		switch job.Labels[dispatcherOwnerLabel] {
		case installPrefix:
			installJobs[nodeName] = job
		case cleanupPrefix:
			cleanupJobs[nodeName] = job
		}
	}
	converged := r.kataConfig.Annotations[annObservedGeneration] == desired
	sort.Slice(nodes.Items, func(i, j int) bool { return nodes.Items[i].Name < nodes.Items[j].Name })
	status := kataconfigurationv1.KataNodesStatus{}
	var failures []string
	for i := range nodes.Items {
		node := &nodes.Items[i]
		_, claimed := node.Labels[kataRuntimeLabel]
		if deleting || !selector.Matches(labels.Set(node.Labels)) {
			job := dispatcherJobForCurrentNode(cleanupJobs[node.Name], node, r.kataConfig.CreationTimestamp)
			cleanupStarted := rolloutStarted
			if deleting {
				cleanupStarted = *r.kataConfig.DeletionTimestamp
			}
			failedResult := dispatcherResultIsCurrent(node, job, r.kataConfig.CreationTimestamp,
				cleanupStarted, !deleting && converged) && node.Labels[dispatcherResultLabel] == "failed"
			if !claimed && !failedResult && (job == nil || jobHasCondition(job, batchv1.JobComplete)) {
				continue
			}
			if jobHasCondition(job, batchv1.JobFailed) || failedResult {
				status.FailedToUninstall = append(status.FailedToUninstall, node.Name)
				failures = append(failures, r.kataDeployNodeFailure(node, job))
			} else if job != nil {
				status.Uninstalling = append(status.Uninstalling, node.Name)
			} else {
				status.WaitingToUninstall = append(status.WaitingToUninstall, node.Name)
			}
			continue
		}

		status.NodeCount++
		job := dispatcherJobForCurrentNode(installJobs[node.Name], node, r.kataConfig.CreationTimestamp)
		currentResult := dispatcherResultIsCurrent(node, job, r.kataConfig.CreationTimestamp,
			rolloutStarted, converged)
		switch {
		case jobHasCondition(job, batchv1.JobFailed) || (currentResult && node.Labels[dispatcherResultLabel] == "failed"):
			status.FailedToInstall = append(status.FailedToInstall, node.Name)
			failures = append(failures, r.kataDeployNodeFailure(node, job))
		case job != nil && !jobHasCondition(job, batchv1.JobComplete):
			status.Installing = append(status.Installing, node.Name)
		case currentResult && node.Labels[dispatcherResultLabel] == "succeeded" && kataDeployNodeReady(node):
			status.Installed = append(status.Installed, node.Name)
		case job != nil || node.Labels[kataRuntimeLabel] == "false" || (currentResult && node.Labels[dispatcherResultLabel] == "succeeded"):
			status.Installing = append(status.Installing, node.Name)
		default:
			status.WaitingToInstall = append(status.WaitingToInstall, node.Name)
		}
	}
	status.ReadyNodeCount = len(status.Installed)
	r.kataConfig.Status.KataNodes = status

	// Preserve the deletion guard's explanation while Kata pods still exist.
	if cond := r.findInProgressCondition(); deleting && cond != nil && cond.Reason == "BlockedByExistingKataPods" {
		return nil
	}
	if cond := r.findInProgressCondition(); !deleting && r.kataConfig.Spec.EnablePeerPods && converged &&
		status.NodeCount > 0 && status.ReadyNodeCount == status.NodeCount && cond != nil && strings.HasPrefix(cond.Reason, "PodVMImage") {
		return nil
	}
	if len(failures) > 0 {
		r.setKataDeployProgress(corev1.ConditionTrue, "Failed", strings.Join(failures, "; "))
	} else if deleting {
		r.setKataDeployProgress(corev1.ConditionTrue, "Uninstalling",
			fmt.Sprintf("Removing Kata from %d remaining nodes", len(status.Uninstalling)+len(status.WaitingToUninstall)))
	} else if status.NodeCount == 0 {
		r.setKataDeployProgress(corev1.ConditionFalse, "NoEligibleNodes", "No worker nodes match the KataConfig selector")
	} else if status.ReadyNodeCount != status.NodeCount || !converged || len(status.Uninstalling)+len(status.WaitingToUninstall) > 0 {
		r.setKataDeployProgress(corev1.ConditionTrue, "Installing",
			fmt.Sprintf("%d of %d selected nodes are ready for Kata workloads", status.ReadyNodeCount, status.NodeCount))
	} else {
		r.setKataDeployProgress(corev1.ConditionFalse, "", "")
	}
	return nil
}

func jobHasCondition(job *batchv1.Job, condition batchv1.JobConditionType) bool {
	if job == nil {
		return false
	}
	for _, c := range job.Status.Conditions {
		if c.Type == condition && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func dispatcherJobForCurrentNode(job *batchv1.Job, node *corev1.Node, configCreated metav1.Time) *batchv1.Job {
	if job != nil && (job.CreationTimestamp.Before(&configCreated) || job.CreationTimestamp.Before(&node.CreationTimestamp)) {
		return nil
	}
	return job
}

func dispatcherResultIsCurrent(node *corev1.Node, job *batchv1.Job, configCreated, rolloutStarted metav1.Time, converged bool) bool {
	finished, err := time.Parse(time.RFC3339Nano, node.Annotations[dispatcherFinishedAnnotation])
	if err != nil || finished.Before(configCreated.Time) || finished.Before(node.CreationTimestamp.Time) {
		return false
	}
	if job != nil && finished.Before(job.CreationTimestamp.Time) {
		return false
	}
	return converged || (!rolloutStarted.IsZero() && !finished.Before(rolloutStarted.Time))
}

func kataDeployNodeReady(node *corev1.Node) bool {
	if node.Labels[kataRuntimeLabel] != "true" || !kataDeployNodeIsReady(node) {
		return false
	}
	for _, handler := range node.Status.RuntimeHandlers {
		if handler.Name == defaultKataDeployRuntime.Handler {
			return true
		}
	}
	return false
}

func kataDeployNodeIsReady(node *corev1.Node) bool {
	for _, condition := range node.Status.Conditions {
		if condition.Type == corev1.NodeReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

func kataDeployNodeStatusChanged(old, new *corev1.Node) bool {
	if !reflect.DeepEqual(old.Labels, new.Labels) || kataDeployNodeIsReady(old) != kataDeployNodeIsReady(new) ||
		!reflect.DeepEqual(old.Status.RuntimeHandlers, new.Status.RuntimeHandlers) {
		return true
	}
	for _, key := range []string{dispatcherFinishedAnnotation, dispatcherErrorAnnotation} {
		if old.Annotations[key] != new.Annotations[key] {
			return true
		}
	}
	return false
}

// Read the termination message recorded by the guest-prep stage. Only Pods
// owned by this Job can explain its failure; an older Job can reuse its name.
func (r *KataConfigOpenShiftReconciler) kataDeployNodeFailure(node *corev1.Node, job *batchv1.Job) string {
	if job != nil && job.UID != "" && jobHasCondition(job, batchv1.JobFailed) {
		pods := &corev1.PodList{}
		if err := r.Client.List(context.TODO(), pods, client.InNamespace(job.Namespace),
			client.MatchingLabels{"batch.kubernetes.io/job-name": job.Name}); err != nil {
			r.Log.Error(err, "Cannot read failed install Pod", "job", job.Name)
		} else {
			sort.Slice(pods.Items, func(i, j int) bool {
				return pods.Items[j].CreationTimestamp.Before(&pods.Items[i].CreationTimestamp)
			})
			for _, pod := range pods.Items {
				owned := false
				for _, owner := range pod.OwnerReferences {
					if owner.Kind == "Job" && owner.UID == job.UID && owner.Controller != nil && *owner.Controller {
						owned = true
					}
				}
				if !owned {
					continue
				}
				for _, container := range pod.Status.InitContainerStatuses {
					failed := container.State.Terminated
					if container.Name == "guest-prep" && failed != nil && failed.ExitCode != 0 && strings.TrimSpace(failed.Message) != "" {
						return fmt.Sprintf("Node %s: Job %s, guest-prep: %s", node.Name, job.Name, strings.TrimSpace(failed.Message))
					}
				}
			}
		}
	}
	return dispatcherNodeFailure(node, job)
}

func dispatcherNodeFailure(node *corev1.Node, job *batchv1.Job) string {
	finished, err := time.Parse(time.RFC3339Nano, node.Annotations[dispatcherFinishedAnnotation])
	if detail := node.Annotations[dispatcherErrorAnnotation]; detail != "" && err == nil && (job == nil || !finished.Before(job.CreationTimestamp.Time)) {
		return fmt.Sprintf("Node %s: %s", node.Name, detail)
	}
	if job != nil {
		for _, condition := range job.Status.Conditions {
			if condition.Type == batchv1.JobFailed && condition.Status == corev1.ConditionTrue {
				return fmt.Sprintf("Node %s: Job %s failed: %s %s", node.Name, job.Name, condition.Reason, condition.Message)
			}
		}
	}
	return fmt.Sprintf("Node %s: Kata installation failed", node.Name)
}

func (r *KataConfigOpenShiftReconciler) setKataDeployProgress(status corev1.ConditionStatus, reason, message string) {
	condition := r.findInProgressCondition()
	if condition == nil {
		condition = r.addInProgressCondition()
	}
	if condition.Status != status || condition.LastTransitionTime.IsZero() {
		condition.LastTransitionTime = metav1.Now()
	}
	condition.Status, condition.Reason, condition.Message = status, reason, message
}

// A selector change runs the dispatcher's cleanup pipeline on removed nodes.
// Apply the workload guard before that rollout can remove their runtime.
func (r *KataConfigOpenShiftReconciler) checkKataDeployNodeRemoval() error {
	selectorString, err := r.dispatcherNodeSelector()
	if err != nil {
		return err
	}
	selector, err := labels.Parse(selectorString)
	if err != nil {
		return err
	}
	nodes := &corev1.NodeList{}
	if err := r.Client.List(context.TODO(), nodes, client.HasLabels{kataRuntimeLabel}); err != nil {
		return err
	}
	removed := map[string]bool{}
	for _, node := range nodes.Items {
		if !selector.Matches(labels.Set(node.Labels)) {
			removed[node.Name] = true
		}
	}
	if len(removed) == 0 {
		return nil
	}
	pods := &corev1.PodList{}
	if err := r.Client.List(context.TODO(), pods); err != nil {
		return err
	}
	runtimeClasses, err := r.kataRuntimeClassesForDeletion()
	if err != nil {
		return err
	}
	var blocking []string
	for _, pod := range pods.Items {
		if removed[pod.Spec.NodeName] && pod.Spec.RuntimeClassName != nil && contains(runtimeClasses, *pod.Spec.RuntimeClassName) {
			blocking = append(blocking, fmt.Sprintf("%s/%s on node %s", pod.Namespace, pod.Name, pod.Spec.NodeName))
		}
	}
	if len(blocking) > 0 {
		sort.Strings(blocking)
		return fmt.Errorf("Kata node removal is blocked by pods: %s. Delete or move these pods before changing the selector", strings.Join(blocking, ", "))
	}
	return nil
}
