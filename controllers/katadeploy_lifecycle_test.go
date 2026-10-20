package controllers

import (
	"context"
	"strings"
	"testing"

	secv1 "github.com/openshift/api/security/v1"
	kataconfigurationv1 "github.com/openshift/sandboxed-containers-operator/api/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestKataDeployDeletingConfigDoesNotRestartInstall(t *testing.T) {
	for _, reason := range []string{"Installing", "Updating"} {
		for _, workload := range []bool{false, true} {
			name := reason + "/without-workload"
			if workload {
				name = reason + "/with-workload"
			}
			t.Run(name, func(t *testing.T) {
				r := newDispatcherTestReconciler()
				if err := secv1.AddToScheme(r.Scheme); err != nil {
					t.Fatal(err)
				}
				r.DeploymentMode = KataDeployMode
				r.kataConfig.Finalizers = []string{kataDeployFinalizer}
				r.kataConfig.DeletionTimestamp = &metav1.Time{Time: statusTestFinished}
				r.setKataDeployProgress(corev1.ConditionTrue, reason, "previous operation")
				objects := []runtime.Object{
					r.kataConfig.DeepCopy(),
					&corev1.ConfigMap{
						ObjectMeta: metav1.ObjectMeta{Name: FgConfigMapName, Namespace: OperatorNamespace},
						Data:       map[string]string{DeploymentModeConfig: "KataDeploy"},
					},
				}
				if workload {
					className := kataRuntimeClassName
					objects = append(objects, &corev1.Pod{
						ObjectMeta: metav1.ObjectMeta{Name: "kata-test", Namespace: "workloads"},
						Spec:       corev1.PodSpec{RuntimeClassName: &className},
						Status:     corev1.PodStatus{Phase: corev1.PodRunning},
					})
				}
				r.Client = fake.NewClientBuilder().WithScheme(r.Scheme).
					WithStatusSubresource(&kataconfigurationv1.KataConfig{}).
					WithRuntimeObjects(objects...).Build()
				request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(r.kataConfig)}
				result, err := r.Reconcile(context.Background(), request)
				if result.RequeueAfter == 0 {
					t.Fatalf("deletion should wait for cleanup or workload removal: %+v", result)
				}
				jobs := &batchv1.JobList{}
				if listErr := r.Client.List(context.Background(), jobs); listErr != nil {
					t.Fatal(listErr)
				}
				stored := &kataconfigurationv1.KataConfig{}
				if getErr := r.Client.Get(context.Background(), request.NamespacedName, stored); getErr != nil {
					t.Fatal(getErr)
				}
				if len(stored.Finalizers) != 1 || stored.Finalizers[0] != kataDeployFinalizer {
					t.Fatal("deletion must retain its finalizer until cleanup finishes")
				}
				if workload {
					if err == nil || !strings.Contains(err.Error(), "workloads/kata-test") {
						t.Fatalf("deleting config must check workloads before starting Jobs: %v", err)
					}
					if len(jobs.Items) != 0 || len(stored.Status.Conditions) != 1 ||
						stored.Status.Conditions[0].Reason != "BlockedByExistingKataPods" {
						t.Fatalf("blocked deletion created Jobs or lost its explanation: jobs=%v status=%+v", jobs.Items, stored.Status)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				if len(jobs.Items) != 1 || jobs.Items[0].Labels[labelFleetOperation] != opCleanup {
					t.Fatalf("deleting config must start cleanup, never a rollout: %+v", jobs.Items)
				}
			})
		}
	}
}

func TestKataDeployNewGenerationWaitsForPreviousNodeWork(t *testing.T) {
	for _, operation := range []string{opRollout, opCoverage, opCleanup, "node-install"} {
		t.Run(operation, func(t *testing.T) {
			r := newDispatcherTestReconciler()
			if err := secv1.AddToScheme(r.Scheme); err != nil {
				t.Fatal(err)
			}
			r.DeploymentMode = KataDeployMode
			previousGeneration, err := r.dispatcherDesiredGeneration()
			if err != nil {
				t.Fatal(err)
			}
			previous := statusTestJob("previous-"+operation, "kata-"+previousGeneration[:12], "worker-a", "")
			if operation != "node-install" {
				previous.Labels = map[string]string{labelFleetOperation: operation}
			}
			r.kataConfig.Spec.KataConfigPoolSelector = &metav1.LabelSelector{
				MatchLabels: map[string]string{"kata-test": "yes"},
			}
			desired, err := r.dispatcherDesiredGeneration()
			if err != nil {
				t.Fatal(err)
			}
			if desired == previousGeneration {
				t.Fatal("test requires a new desired generation")
			}
			r.Client = fake.NewClientBuilder().WithScheme(r.Scheme).
				WithStatusSubresource(&kataconfigurationv1.KataConfig{}, &batchv1.Job{}).
				WithRuntimeObjects(r.kataConfig.DeepCopy(), previous).Build()
			if err := r.Client.Get(context.Background(), client.ObjectKeyFromObject(r.kataConfig), r.kataConfig); err != nil {
				t.Fatal(err)
			}
			result, err := r.processKataConfigInstallRequestDispatcher()
			if err != nil || result.RequeueAfter == 0 {
				t.Fatalf("new rollout must wait for previous work: result=%+v err=%v", result, err)
			}
			rolloutKey := client.ObjectKey{Name: r.dispatcherJobName(desired), Namespace: OperatorNamespace}
			if err := r.Client.Get(context.Background(), rolloutKey, &batchv1.Job{}); !k8serrors.IsNotFound(err) {
				t.Fatalf("new generation started while %s was active: %v", operation, err)
			}
			if err := r.Client.Get(context.Background(), client.ObjectKeyFromObject(previous), previous); err != nil {
				t.Fatalf("previous work must remain intact: %v", err)
			}
			previous.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
			if err := r.Client.Status().Update(context.Background(), previous); err != nil {
				t.Fatal(err)
			}
			if _, err := r.processKataConfigInstallRequestDispatcher(); err != nil {
				t.Fatal(err)
			}
			rollout := &batchv1.Job{}
			if err := r.Client.Get(context.Background(), rolloutKey, rollout); err != nil {
				t.Fatalf("new rollout did not start after previous work finished: %v", err)
			}
			if rollout.Labels[labelFleetOperation] != opRollout {
				t.Fatalf("expected a new rollout Job: %+v", rollout.Labels)
			}
		})
	}
}
