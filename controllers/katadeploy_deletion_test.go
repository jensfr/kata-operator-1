package controllers

import (
	"context"
	"errors"
	"strings"
	"testing"

	secv1 "github.com/openshift/api/security/v1"
	kataconfigurationv1 "github.com/openshift/sandboxed-containers-operator/api/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	nodeapi "k8s.io/api/node/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestKataDeployDeletionBlockedByWorkloads(t *testing.T) {
	for _, phase := range []corev1.PodPhase{corev1.PodPending, corev1.PodRunning, corev1.PodSucceeded} {
		t.Run(string(phase), func(t *testing.T) {
			r := newDispatcherTestReconciler()
			r.DeploymentMode = KataDeployMode
			r.kataConfig.Finalizers = []string{kataDeployFinalizer}
			r.kataConfig.DeletionTimestamp = &metav1.Time{Time: statusTestFinished}
			className := kataRuntimeClassName
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "kata-test", Namespace: "workloads"},
				Spec:       corev1.PodSpec{RuntimeClassName: &className},
				Status:     corev1.PodStatus{Phase: phase},
			}
			// RuntimeClasses is empty, as it can be during a partial installation.
			r.Client = fake.NewClientBuilder().WithScheme(r.Scheme).
				WithStatusSubresource(&kataconfigurationv1.KataConfig{}).
				WithRuntimeObjects(r.kataConfig.DeepCopy(), pod).Build()
			if err := r.Client.Get(context.Background(), client.ObjectKeyFromObject(r.kataConfig), r.kataConfig); err != nil {
				t.Fatal(err)
			}
			result, err := r.processKataConfigDeleteRequestDispatcher()
			if err == nil || !strings.Contains(err.Error(), "workloads/kata-test") || result.RequeueAfter == 0 {
				t.Fatalf("workload did not block deletion: result=%+v err=%v", result, err)
			}
			jobs := &batchv1.JobList{}
			if err := r.Client.List(context.Background(), jobs); err != nil {
				t.Fatal(err)
			}
			if len(jobs.Items) != 0 {
				t.Fatal("cleanup must not create Jobs while Kata workloads exist")
			}
			stored := &kataconfigurationv1.KataConfig{}
			if err := r.Client.Get(context.Background(), client.ObjectKeyFromObject(r.kataConfig), stored); err != nil {
				t.Fatal(err)
			}
			if len(stored.Finalizers) != 1 || stored.Finalizers[0] != kataDeployFinalizer ||
				len(stored.Status.Conditions) != 1 || stored.Status.Conditions[0].Reason != "BlockedByExistingKataPods" {
				t.Fatalf("blocked deletion must retain finalizer and explain why: %+v", stored)
			}
			if err := r.Client.Delete(context.Background(), pod); err != nil {
				t.Fatal(err)
			}
			if _, err := r.checkDeletionEligibility(); err != nil {
				t.Fatalf("deletion should proceed after workload removal: %v", err)
			}
		})
	}
}

func TestKataDeployDeletionAllowsOperatorPods(t *testing.T) {
	r := newDispatcherTestReconciler(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "kata-cover-test", Namespace: OperatorNamespace},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	})
	r.DeploymentMode = KataDeployMode
	r.kataConfig.Finalizers = []string{kataDeployFinalizer}
	if _, err := r.checkDeletionEligibility(); err != nil {
		t.Fatalf("operator pods without a Kata RuntimeClass must not block uninstall: %v", err)
	}
}

func TestKataDeployDeletionProtectsRuntimeClassAliases(t *testing.T) {
	className := "custom-kata"
	r := newDispatcherTestReconciler(
		&nodeapi.RuntimeClass{ObjectMeta: metav1.ObjectMeta{Name: className}, Handler: defaultKataDeployRuntime.Handler},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "kata-test", Namespace: "workloads"}, Spec: corev1.PodSpec{RuntimeClassName: &className}},
	)
	r.DeploymentMode = KataDeployMode
	if err := r.listKataPods(); err == nil || !strings.Contains(err.Error(), "workloads/kata-test (RuntimeClass custom-kata)") {
		t.Fatalf("alias of Kata's handler must block cleanup: %v", err)
	}
}

func TestKataDeploySelectorRemovalBlockedByWorkloads(t *testing.T) {
	className := kataRuntimeClassName
	for _, nodeName := range []string{"worker-a", "worker-b"} {
		t.Run(nodeName, func(t *testing.T) {
			kept := statusTestNode("worker-a", "succeeded")
			kept.Labels["kata-test"] = "yes"
			removed := statusTestNode("worker-b", "succeeded")
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "kata-test", Namespace: "workloads"},
				Spec:       corev1.PodSpec{RuntimeClassName: &className, NodeName: nodeName},
				Status:     corev1.PodStatus{Phase: corev1.PodRunning},
			}
			r := newDispatcherTestReconciler(kept, removed, pod)
			r.DeploymentMode = KataDeployMode
			r.kataConfig.Spec.KataConfigPoolSelector = &metav1.LabelSelector{MatchLabels: map[string]string{"kata-test": "yes"}}
			err := r.checkKataDeployNodeRemoval()
			if nodeName == kept.Name {
				if err != nil {
					t.Fatalf("workload on retained node should not block rollout: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), "workloads/kata-test on node worker-b") {
				t.Fatalf("workload on removed node must block cleanup: %v", err)
			}
			if err := secv1.AddToScheme(r.Scheme); err != nil {
				t.Fatal(err)
			}
			if err := r.Client.Create(context.Background(), r.kataConfig); err != nil {
				t.Fatal(err)
			}
			if _, err := r.processKataConfigInstallRequestDispatcher(); err == nil {
				t.Fatal("selector cleanup must not start while workload exists")
			}
			jobs := &batchv1.JobList{}
			if err := r.Client.List(context.Background(), jobs); err != nil {
				t.Fatal(err)
			}
			if len(jobs.Items) != 0 || r.findInProgressCondition().Reason != "BlockedByExistingKataPods" {
				t.Fatal("blocked selector change must report its reason and create no Jobs")
			}
		})
	}
}

func TestKataDeployBlockedWorkloadEvent(t *testing.T) {
	r := newDispatcherTestReconciler()
	r.DeploymentMode = KataDeployMode
	r.setKataDeployProgress(corev1.ConditionFalse, "BlockedByExistingKataPods", "blocked")
	className := kataRuntimeClassName
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "kata-test", Namespace: "workloads"}, Spec: corev1.PodSpec{RuntimeClassName: &className}}
	if requests := r.kataPodReconcileRequests(context.Background(), pod); len(requests) != 1 || requests[0].Name != r.kataConfig.Name {
		t.Fatalf("removing a blocking workload should wake reconciliation: %+v", requests)
	}
	pod.Spec.RuntimeClassName = nil
	if requests := r.kataPodReconcileRequests(context.Background(), pod); len(requests) != 0 {
		t.Fatal("ordinary pods should not wake the blocked controller")
	}
}

type kataPodListFailureClient struct{ client.Client }

func (c kataPodListFailureClient) List(ctx context.Context, list client.ObjectList, options ...client.ListOption) error {
	if _, pods := list.(*corev1.PodList); pods {
		return errors.New("pod API unavailable")
	}
	return c.Client.List(ctx, list, options...)
}

func TestKataDeployDeletionFailsClosedOnPodListError(t *testing.T) {
	r := newDispatcherTestReconciler()
	r.DeploymentMode = KataDeployMode
	r.kataConfig.Finalizers = []string{kataDeployFinalizer}
	r.Client = fake.NewClientBuilder().WithScheme(r.Scheme).
		WithStatusSubresource(&kataconfigurationv1.KataConfig{}).
		WithRuntimeObjects(r.kataConfig.DeepCopy()).Build()
	if err := r.Client.Get(context.Background(), client.ObjectKeyFromObject(r.kataConfig), r.kataConfig); err != nil {
		t.Fatal(err)
	}
	r.Client = kataPodListFailureClient{Client: r.Client}
	if _, err := r.processKataConfigDeleteRequestDispatcher(); err == nil || !strings.Contains(err.Error(), "pod API unavailable") {
		t.Fatalf("unavailable pod API must block cleanup: %v", err)
	}
	jobs := &batchv1.JobList{}
	if err := r.Client.List(context.Background(), jobs); err != nil {
		t.Fatal(err)
	}
	if len(jobs.Items) != 0 {
		t.Fatal("cleanup must not start without verifying workloads")
	}
}
