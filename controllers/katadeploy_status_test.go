package controllers

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	secv1 "github.com/openshift/api/security/v1"
	kataconfigurationv1 "github.com/openshift/sandboxed-containers-operator/api/v1"
	"github.com/prometheus/client_golang/prometheus"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

var statusTestFinished = time.Date(2026, 10, 14, 15, 0, 0, 0, time.UTC)

func TestKataDeployStatusReconcilePersistsCounters(t *testing.T) {
	node := statusTestNode("worker-a", "succeeded")
	r, _ := statusTestReconciler(t, true)
	if err := appsv1.AddToScheme(r.Scheme); err != nil {
		t.Fatal(err)
	}
	if err := secv1.AddToScheme(r.Scheme); err != nil {
		t.Fatal(err)
	}
	r.Client = fake.NewClientBuilder().WithScheme(r.Scheme).
		WithStatusSubresource(&kataconfigurationv1.KataConfig{}).
		WithRuntimeObjects(r.kataConfig.DeepCopy(), node,
			&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: FgConfigMapName, Namespace: OperatorNamespace}, Data: map[string]string{DeploymentModeConfig: "KataDeploy"}},
			&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: dashboardConfigMapName, Namespace: OperatorNamespace}},
		).Build()
	t.Setenv("RELATED_IMAGE_KATA_MONITOR", "quay.io/test/monitor:test")
	request := ctrl.Request{NamespacedName: types.NamespacedName{Name: r.kataConfig.Name}}
	for _, ready := range []corev1.ConditionStatus{corev1.ConditionTrue, corev1.ConditionFalse} {
		if err := r.Client.Get(context.Background(), client.ObjectKeyFromObject(node), node); err != nil {
			t.Fatal(err)
		}
		node.Status.Conditions[0].Status = ready
		if err := r.Client.Status().Update(context.Background(), node); err != nil {
			t.Fatal(err)
		}
		if _, err := r.Reconcile(context.Background(), request); err != nil {
			t.Fatal(err)
		}
		stored := &kataconfigurationv1.KataConfig{}
		if err := r.Client.Get(context.Background(), request.NamespacedName, stored); err != nil {
			t.Fatal(err)
		}
		wantReady := 0
		if ready == corev1.ConditionTrue {
			wantReady = 1
		}
		if stored.Status.KataNodes.NodeCount != 1 || stored.Status.KataNodes.ReadyNodeCount != wantReady {
			t.Fatalf("Reconcile persisted wrong counters: %+v", stored.Status.KataNodes)
		}
	}
}

func statusTestNode(name, result string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				"node-role.kubernetes.io/worker": "",
				kataRuntimeLabel:                 "true",
				dispatcherResultLabel:            result,
			},
			Annotations: map[string]string{dispatcherFinishedAnnotation: statusTestFinished.Format(time.RFC3339)},
		},
		Status: corev1.NodeStatus{
			Conditions:      []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
			RuntimeHandlers: []corev1.NodeRuntimeHandler{{Name: defaultKataDeployRuntime.Handler}},
		},
	}
}

func statusTestJob(name, prefix, node string, condition batchv1.JobConditionType) *batchv1.Job {
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: OperatorNamespace,
			Labels:            map[string]string{dispatcherOwnerLabel: prefix},
			CreationTimestamp: metav1.NewTime(statusTestFinished.Add(-time.Minute)),
		},
		Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{NodeName: node}}},
	}
	if condition != "" {
		job.Status.Conditions = []batchv1.JobCondition{{Type: condition, Status: corev1.ConditionTrue, Reason: "BackoffLimitExceeded"}}
	}
	return job
}

func statusTestReconciler(t *testing.T, converged bool, objects ...runtime.Object) (*KataConfigOpenShiftReconciler, string) {
	t.Helper()
	r := newDispatcherTestReconciler(objects...)
	r.DeploymentMode = KataDeployMode
	generation, err := r.dispatcherDesiredGeneration()
	if err != nil {
		t.Fatal(err)
	}
	if converged {
		r.kataConfig.Annotations = map[string]string{annObservedGeneration: generation}
	}
	return r, generation
}

func TestKataDeployStatusAfterJobExpiry(t *testing.T) {
	nodes := []runtime.Object{statusTestNode("worker-c", "succeeded"), statusTestNode("worker-a", "succeeded"), statusTestNode("worker-b", "succeeded")}
	master := statusTestNode("master", "succeeded")
	delete(master.Labels, "node-role.kubernetes.io/worker")
	delete(master.Labels, kataRuntimeLabel)
	nodes = append(nodes, master)
	r, _ := statusTestReconciler(t, true, nodes...)
	if err := r.updateKataDeployStatus(); err != nil {
		t.Fatal(err)
	}
	want := kataconfigurationv1.KataNodesStatus{NodeCount: 3, ReadyNodeCount: 3, Installed: []string{"worker-a", "worker-b", "worker-c"}}
	if !reflect.DeepEqual(r.kataConfig.Status.KataNodes, want) {
		t.Fatalf("status: got %+v, want %+v", r.kataConfig.Status.KataNodes, want)
	}
	if r.getInProgressConditionValue() != corev1.ConditionFalse {
		t.Fatal("completed rollout must not be InProgress")
	}
	transition := r.findInProgressCondition().LastTransitionTime
	if err := r.updateKataDeployStatus(); err != nil {
		t.Fatal(err)
	}
	if !transition.Equal(&r.findInProgressCondition().LastTransitionTime) {
		t.Fatal("unchanged status must preserve LastTransitionTime")
	}

	if err := r.Client.Create(context.Background(), r.kataConfig); err != nil {
		t.Fatal(err)
	}
	registry := prometheus.NewRegistry()
	registry.MustRegister(&OscMetricsCollector{client: r.Client})
	metrics, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, metric := range metrics {
		if metric.GetName() == "kata_config_installation_success" {
			if metric.Metric[0].GetGauge().GetValue() != 1 {
				t.Fatal("completed rollout must produce installation success metric 1")
			}
			return
		}
	}
	t.Fatal("installation success metric missing")
}

func TestKataDeployStatusPartialRollout(t *testing.T) {
	installed := statusTestNode("worker-a", "succeeded")
	installing := statusTestNode("worker-b", "")
	installing.Labels[kataRuntimeLabel] = "false"
	waiting := statusTestNode("worker-c", "")
	delete(waiting.Labels, kataRuntimeLabel)
	r, generation := statusTestReconciler(t, false, installed, installing, waiting)
	prefix := "kata-" + generation[:12]
	rollout := statusTestJob(r.dispatcherJobName(generation), "", "", "")
	rollout.OwnerReferences = []metav1.OwnerReference{{UID: r.kataConfig.UID}}
	active := statusTestJob("install-b", prefix, installing.Name, "")
	for _, job := range []*batchv1.Job{rollout, active} {
		if err := r.Client.Create(context.Background(), job); err != nil {
			t.Fatal(err)
		}
	}
	// worker-a's Job has expired, but its result belongs to the current rollout.
	if err := r.updateKataDeployStatus(); err != nil {
		t.Fatal(err)
	}
	want := kataconfigurationv1.KataNodesStatus{
		NodeCount: 3, ReadyNodeCount: 1, Installed: []string{"worker-a"},
		Installing: []string{"worker-b"}, WaitingToInstall: []string{"worker-c"},
	}
	if !reflect.DeepEqual(r.kataConfig.Status.KataNodes, want) {
		t.Fatalf("status: got %+v, want %+v", r.kataConfig.Status.KataNodes, want)
	}
	if r.getInProgressConditionValue() != corev1.ConditionTrue {
		t.Fatal("partial rollout must be InProgress")
	}
}

func TestKataDeployStatusRejectsOldResults(t *testing.T) {
	for _, result := range []string{"succeeded", "failed"} {
		t.Run(result, func(t *testing.T) {
			node := statusTestNode("worker-a", result)
			r, generation := statusTestReconciler(t, false, node)
			rollout := statusTestJob(r.dispatcherJobName(generation), "", "", "")
			rollout.CreationTimestamp = metav1.NewTime(statusTestFinished.Add(time.Minute))
			rollout.OwnerReferences = []metav1.OwnerReference{{UID: r.kataConfig.UID}}
			if err := r.Client.Create(context.Background(), rollout); err != nil {
				t.Fatal(err)
			}
			if err := r.updateKataDeployStatus(); err != nil {
				t.Fatal(err)
			}
			status := r.kataConfig.Status.KataNodes
			if status.ReadyNodeCount != 0 || len(status.FailedToInstall) != 0 || len(status.WaitingToInstall) != 1 {
				t.Fatalf("old result was reused: %+v", status)
			}
		})
	}
}

func TestKataDeployStatusNeedsReadyNodeAndHandler(t *testing.T) {
	for _, change := range []string{"not ready", "missing handler", "pending label", "missing result timestamp"} {
		t.Run(change, func(t *testing.T) {
			node := statusTestNode("worker-a", "succeeded")
			switch change {
			case "not ready":
				node.Status.Conditions[0].Status = corev1.ConditionFalse
			case "missing handler":
				node.Status.RuntimeHandlers = nil
			case "pending label":
				node.Labels[kataRuntimeLabel] = "false"
			case "missing result timestamp":
				node.Annotations = nil
			}
			r, _ := statusTestReconciler(t, true, node)
			if err := r.updateKataDeployStatus(); err != nil {
				t.Fatal(err)
			}
			if r.kataConfig.Status.KataNodes.ReadyNodeCount != 0 || r.getInProgressConditionValue() != corev1.ConditionTrue {
				t.Fatal("node cannot count as completed")
			}
		})
	}
}

func TestKataDeployStatusFailureAndRetry(t *testing.T) {
	node := statusTestNode("worker-a", "failed")
	node.Labels[kataRuntimeLabel] = "false"
	node.Annotations[dispatcherErrorAnnotation] = "load-kernel-modules exited 1: no usable virtualization backend"
	r, generation := statusTestReconciler(t, true, node)
	if err := r.updateKataDeployStatus(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(r.kataConfig.Status.KataNodes.FailedToInstall, []string{node.Name}) ||
		r.findInProgressCondition().Reason != "Failed" || !strings.Contains(r.findInProgressCondition().Message, "no usable virtualization backend") {
		t.Fatalf("failure was not reported: %+v", r.kataConfig.Status)
	}
	// A new attempt must not inherit an older failure marker.
	job := statusTestJob("retry", "kata-"+generation[:12], node.Name, "")
	job.CreationTimestamp = metav1.NewTime(statusTestFinished.Add(time.Minute))
	if err := r.Client.Create(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if err := r.updateKataDeployStatus(); err != nil {
		t.Fatal(err)
	}
	if len(r.kataConfig.Status.KataNodes.FailedToInstall) != 0 || len(r.kataConfig.Status.KataNodes.Installing) != 1 {
		t.Fatalf("retry did not replace old failure: %+v", r.kataConfig.Status)
	}
}

func TestKataDeployStatusSelectorAndCleanup(t *testing.T) {
	selected := statusTestNode("worker-a", "succeeded")
	selected.Labels["kata-test"] = "yes"
	removed := statusTestNode("worker-b", "succeeded")
	r, _ := statusTestReconciler(t, false, selected, removed)
	r.kataConfig.Spec.KataConfigPoolSelector = &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{
		Key: "kata-test", Operator: metav1.LabelSelectorOpIn, Values: []string{"yes"},
	}}}
	generation, err := r.dispatcherDesiredGeneration()
	if err != nil {
		t.Fatal(err)
	}
	r.kataConfig.Annotations = map[string]string{annObservedGeneration: generation}
	if err := r.updateKataDeployStatus(); err != nil {
		t.Fatal(err)
	}
	status := r.kataConfig.Status.KataNodes
	if status.NodeCount != 1 || status.ReadyNodeCount != 1 || !reflect.DeepEqual(status.WaitingToUninstall, []string{removed.Name}) {
		t.Fatalf("selector change status: %+v", status)
	}
	if r.getInProgressConditionValue() != corev1.ConditionTrue {
		t.Fatal("pending cleanup must remain InProgress")
	}
}

func TestKataDeployStatusUninstall(t *testing.T) {
	active := statusTestNode("worker-a", "succeeded")
	failed := statusTestNode("worker-b", "failed")
	failed.Annotations[dispatcherErrorAnnotation] = "revert-cri exited 1"
	done := statusTestNode("worker-c", "")
	delete(done.Labels, kataRuntimeLabel)
	r, _ := statusTestReconciler(t, true, active, failed, done)
	r.kataConfig.DeletionTimestamp = &metav1.Time{Time: statusTestFinished}
	prefix := "kata-cleanup-" + string(r.kataConfig.UID)[:8]
	for _, job := range []*batchv1.Job{
		statusTestJob("cleanup-a", prefix, active.Name, ""),
		statusTestJob("cleanup-b", prefix, failed.Name, batchv1.JobFailed),
		statusTestJob("cleanup-c", prefix, done.Name, batchv1.JobComplete),
	} {
		if err := r.Client.Create(context.Background(), job); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.updateKataDeployStatus(); err != nil {
		t.Fatal(err)
	}
	want := kataconfigurationv1.KataNodesStatus{Uninstalling: []string{active.Name}, FailedToUninstall: []string{failed.Name}}
	if !reflect.DeepEqual(r.kataConfig.Status.KataNodes, want) || !strings.Contains(r.findInProgressCondition().Message, "revert-cri exited 1") {
		t.Fatalf("uninstall status: %+v", r.kataConfig.Status)
	}
}

func TestKataDeployStatusNoEligibleNodes(t *testing.T) {
	r, _ := statusTestReconciler(t, true)
	if err := r.updateKataDeployStatus(); err != nil {
		t.Fatal(err)
	}
	if r.kataConfig.Status.KataNodes.NodeCount != 0 || r.findInProgressCondition().Reason != "NoEligibleNodes" {
		t.Fatalf("empty selection status: %+v", r.kataConfig.Status)
	}
}

func TestKataDeployStatusUninstallFailureAfterJobExpiry(t *testing.T) {
	node := statusTestNode("worker-a", "failed")
	node.Annotations[dispatcherErrorAnnotation] = "remove-artifacts exited 1"
	r, _ := statusTestReconciler(t, true, node)
	r.kataConfig.DeletionTimestamp = &metav1.Time{Time: statusTestFinished.Add(-time.Minute)}
	if err := r.updateKataDeployStatus(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(r.kataConfig.Status.KataNodes.FailedToUninstall, []string{node.Name}) ||
		!strings.Contains(r.findInProgressCondition().Message, "remove-artifacts exited 1") {
		t.Fatalf("expired cleanup Job lost the failure: %+v", r.kataConfig.Status)
	}
}

func TestKataDeployStatusJobFailureBeforeNodeResult(t *testing.T) {
	node := statusTestNode("worker-a", "succeeded")
	r, generation := statusTestReconciler(t, true, node)
	job := statusTestJob("failed-install", "kata-"+generation[:12], node.Name, batchv1.JobFailed)
	job.CreationTimestamp = metav1.NewTime(statusTestFinished.Add(time.Minute))
	if err := r.Client.Create(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if err := r.updateKataDeployStatus(); err != nil {
		t.Fatal(err)
	}
	if r.kataConfig.Status.KataNodes.ReadyNodeCount != 0 || len(r.kataConfig.Status.KataNodes.FailedToInstall) != 1 ||
		!strings.Contains(r.findInProgressCondition().Message, "BackoffLimitExceeded") {
		t.Fatalf("Job failure was hidden by the old success marker: %+v", r.kataConfig.Status)
	}
}

func TestKataDeployStatusRejectsPreviousNodeAndConfig(t *testing.T) {
	for _, changed := range []string{"node", "config"} {
		t.Run(changed, func(t *testing.T) {
			node := statusTestNode("worker-a", "succeeded")
			if changed == "node" {
				node.CreationTimestamp = metav1.NewTime(statusTestFinished.Add(time.Minute))
			}
			r, generation := statusTestReconciler(t, true, node)
			if changed == "config" {
				r.kataConfig.CreationTimestamp = metav1.NewTime(statusTestFinished.Add(time.Minute))
			}
			oldJob := statusTestJob("old-failed-job", "kata-"+generation[:12], node.Name, batchv1.JobFailed)
			if err := r.Client.Create(context.Background(), oldJob); err != nil {
				t.Fatal(err)
			}
			if err := r.updateKataDeployStatus(); err != nil {
				t.Fatal(err)
			}
			if r.kataConfig.Status.KataNodes.ReadyNodeCount != 0 || len(r.kataConfig.Status.KataNodes.FailedToInstall) != 0 {
				t.Fatal("a result from a previous node or KataConfig must not count")
			}
		})
	}
}

func TestKataDeployNodeEvents(t *testing.T) {
	for _, change := range []string{"ready", "handler", "result", "error", "finished", "heartbeat"} {
		t.Run(change, func(t *testing.T) {
			old := statusTestNode("worker-a", "succeeded")
			new := old.DeepCopy()
			switch change {
			case "ready":
				new.Status.Conditions[0].Status = corev1.ConditionFalse
			case "handler":
				new.Status.RuntimeHandlers = nil
			case "result":
				new.Labels[dispatcherResultLabel] = "failed"
			case "error":
				new.Annotations[dispatcherErrorAnnotation] = "host-check failed"
			case "finished":
				new.Annotations[dispatcherFinishedAnnotation] = statusTestFinished.Add(time.Minute).Format(time.RFC3339)
			case "heartbeat":
				new.Status.Conditions[0].LastHeartbeatTime = metav1.Now()
			}
			r, _ := statusTestReconciler(t, true)
			queue := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[reconcile.Request]())
			defer queue.ShutDown()
			(&NodeEventHandler{reconciler: r}).Update(context.Background(), event.UpdateEvent{ObjectOld: old, ObjectNew: new}, queue)
			want := 1
			if change == "heartbeat" {
				want = 0
			}
			if queue.Len() != want {
				t.Fatalf("queued %d requests, want %d", queue.Len(), want)
			}
		})
	}
}

func TestKataDeployGuestFailureReportsTerminationAndZeroSuccess(t *testing.T) {
	node := statusTestNode("worker-a", "succeeded")
	r, generation := statusTestReconciler(t, true, node)
	job := statusTestJob("guest-install", "kata-"+generation[:12], node.Name, batchv1.JobFailed)
	job.UID = types.UID("current-job")
	controller := true
	message := "FATAL: guest kata-agent mismatch: installed=abc123 guest=def456"
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "guest-install-pod", Namespace: OperatorNamespace,
			Labels:          map[string]string{"batch.kubernetes.io/job-name": job.Name},
			OwnerReferences: []metav1.OwnerReference{{Kind: "Job", UID: job.UID, Controller: &controller}},
		},
		Status: corev1.PodStatus{InitContainerStatuses: []corev1.ContainerStatus{{
			Name: "guest-prep", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				ExitCode: 1, Reason: "Error", Message: message,
			}},
		}}},
	}
	for _, object := range []client.Object{job, pod} {
		if err := r.Client.Create(context.Background(), object); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.updateKataDeployStatus(); err != nil {
		t.Fatal(err)
	}
	status := r.kataConfig.Status.KataNodes
	if status.ReadyNodeCount != 0 || !reflect.DeepEqual(status.FailedToInstall, []string{node.Name}) ||
		r.findInProgressCondition().Reason != "Failed" || !strings.Contains(r.findInProgressCondition().Message, message) {
		t.Fatalf("guest failure hidden behind previous success: %+v", r.kataConfig.Status)
	}
	if err := r.Client.Create(context.Background(), r.kataConfig); err != nil {
		t.Fatal(err)
	}
	registry := prometheus.NewRegistry()
	registry.MustRegister(&OscMetricsCollector{client: r.Client})
	metrics, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, metric := range metrics {
		if metric.GetName() == "kata_config_installation_success" {
			found = true
			if metric.Metric[0].GetGauge().GetValue() != 0 {
				t.Fatal("failed guest verification reported success")
			}
		}
	}
	if !found {
		t.Fatal("installation success metric missing")
	}
	// A stale Pod from a previous Job with the same name must not explain this failure.
	pod.OwnerReferences[0].UID = types.UID("previous-job")
	if err := r.Client.Update(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
	if err := r.updateKataDeployStatus(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(r.findInProgressCondition().Message, "guest kata-agent mismatch") {
		t.Fatal("termination from a different Job was reused")
	}
}
