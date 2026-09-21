package controllers

import (
	"context"
	"maps"
	"testing"

	nodeapi "k8s.io/api/node/v1"

	"github.com/go-logr/logr"
	kataconfigurationv1 "github.com/openshift/sandboxed-containers-operator/api/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func newTestReconciler(objs ...runtime.Object) *KataConfigOpenShiftReconciler {
	s := runtime.NewScheme()
	_ = corev1.AddToScheme(s)
	_ = batchv1.AddToScheme(s)
	_ = nodeapi.AddToScheme(s)
	_ = kataconfigurationv1.AddToScheme(s)

	return &KataConfigOpenShiftReconciler{
		Client: fake.NewClientBuilder().WithScheme(s).WithRuntimeObjects(objs...).Build(),
		Log:    logr.Discard(),
		Scheme: s,
		kataConfig: &kataconfigurationv1.KataConfig{
			ObjectMeta: metav1.ObjectMeta{
				Name: "example-kataconfig",
				UID:  types.UID("kc-uid-1"),
			},
			Spec: kataconfigurationv1.KataConfigSpec{},
		},
	}
}

func TestEnsureRuntimeClassLifecycle(t *testing.T) {
	t.Run("wrong handler deletes and returns pending", func(t *testing.T) {
		existingRC := &nodeapi.RuntimeClass{
			ObjectMeta: metav1.ObjectMeta{
				Name:       "kata",
				Finalizers: []string{runtimeClassFinalizerName},
			},
			Handler: "kata",
		}
		r := newTestReconciler(existingRC)
		r.DeploymentMode = KataDeployMode

		pending, err := r.ensureRuntimeClass(
			"kata", "0.25", "350Mi", "kata-qemu",
			map[string]string{kataRuntimeLabel: "true"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !pending {
			t.Error("expected pending=true for handler mismatch")
		}

		found := &nodeapi.RuntimeClass{}
		getErr := r.Client.Get(context.TODO(), types.NamespacedName{Name: "kata"}, found)
		if getErr == nil && found.DeletionTimestamp == nil {
			t.Error("expected old RuntimeClass to be deleted or terminating")
		}
	})

	t.Run("terminating RC only waits", func(t *testing.T) {
		now := metav1.Now()
		existingRC := &nodeapi.RuntimeClass{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "kata",
				DeletionTimestamp: &now,
				Finalizers:        []string{"test-finalizer"},
			},
			Handler: "kata",
		}
		r := newTestReconciler(existingRC)
		r.DeploymentMode = KataDeployMode

		pending, err := r.ensureRuntimeClass(
			"kata", "0.25", "350Mi", "kata-qemu",
			map[string]string{kataRuntimeLabel: "true"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !pending {
			t.Error("expected pending=true for terminating RC")
		}
	})

	t.Run("absent RC is created with correct handler", func(t *testing.T) {
		r := newTestReconciler()
		r.DeploymentMode = KataDeployMode

		pending, err := r.ensureRuntimeClass(
			"kata", "0.25", "350Mi", "kata-qemu",
			map[string]string{kataRuntimeLabel: "true"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if pending {
			t.Error("expected pending=false for fresh creation")
		}

		found := &nodeapi.RuntimeClass{}
		if err := r.Client.Get(context.TODO(), types.NamespacedName{Name: "kata"}, found); err != nil {
			t.Fatalf("RuntimeClass not found after creation: %v", err)
		}
		if found.Handler != "kata-qemu" {
			t.Errorf("expected handler kata-qemu, got %q", found.Handler)
		}
	})

	t.Run("correct RC restores missing status entry", func(t *testing.T) {
		existingRC := &nodeapi.RuntimeClass{
			ObjectMeta: metav1.ObjectMeta{Name: "kata"},
			Handler:    "kata-qemu",
		}
		r := newTestReconciler(existingRC)
		r.DeploymentMode = KataDeployMode
		r.kataConfig.Status.RuntimeClasses = []string{}

		pending, err := r.ensureRuntimeClass(
			"kata", "0.25", "350Mi", "kata-qemu",
			map[string]string{kataRuntimeLabel: "true"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if pending {
			t.Error("expected pending=false when handler matches")
		}
		if !contains(r.kataConfig.Status.RuntimeClasses, "kata") {
			t.Error("expected kata in status.runtimeClasses after reconciliation")
		}
	})

	t.Run("correct handler but stale scheduling is updated in place", func(t *testing.T) {
		existingRC := &nodeapi.RuntimeClass{
			ObjectMeta: metav1.ObjectMeta{Name: "kata"},
			Handler:    "kata-qemu",
			Scheduling: &nodeapi.Scheduling{
				NodeSelector: map[string]string{
					"node-role.kubernetes.io/master": "",
				},
			},
		}
		r := newTestReconciler(existingRC)
		r.DeploymentMode = KataDeployMode

		pending, err := r.ensureRuntimeClass(
			"kata", "0.25", "350Mi", "kata-qemu",
			map[string]string{kataRuntimeLabel: "true"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if pending {
			t.Error("expected pending=false, scheduling update should not require deletion")
		}

		found := &nodeapi.RuntimeClass{}
		if err := r.Client.Get(context.TODO(), types.NamespacedName{Name: "kata"}, found); err != nil {
			t.Fatalf("RuntimeClass not found: %v", err)
		}
		if found.Handler != "kata-qemu" {
			t.Errorf("handler should remain kata-qemu, got %q", found.Handler)
		}
		wantSelector := map[string]string{kataRuntimeLabel: "true"}
		if found.Scheduling == nil || !maps.Equal(found.Scheduling.NodeSelector, wantSelector) {
			var got map[string]string
			if found.Scheduling != nil {
				got = found.Scheduling.NodeSelector
			}
			t.Errorf("selector = %v, want %v", got, wantSelector)
		}
	})
}

func TestEnsureRuntimeClassAbsent(t *testing.T) {
	t.Run("stale GPU RC removed in KataDeployMode", func(t *testing.T) {
		existingRC := &nodeapi.RuntimeClass{
			ObjectMeta: metav1.ObjectMeta{
				Name:       "kata-nvidia-gpu",
				Finalizers: []string{runtimeClassFinalizerName},
			},
			Handler: "kata-nvidia-gpu",
		}
		r := newTestReconciler(existingRC)
		r.kataConfig.Status.RuntimeClasses = []string{"kata", "kata-nvidia-gpu"}

		pending, err := r.ensureRuntimeClassAbsent("kata-nvidia-gpu")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !pending {
			t.Error("expected pending=true for deletion of stale RC")
		}
		if contains(r.kataConfig.Status.RuntimeClasses, "kata-nvidia-gpu") {
			t.Error("status should not contain kata-nvidia-gpu after deletion request")
		}
	})

	t.Run("already absent is a no-op", func(t *testing.T) {
		r := newTestReconciler()
		pending, err := r.ensureRuntimeClassAbsent("kata-nvidia-gpu")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if pending {
			t.Error("expected pending=false when RC is already absent")
		}
	})
}

func TestDeploymentModeKataDeploy(t *testing.T) {
	t.Parallel()

	t.Run("ParseDeploymentModeOption accepts KataDeploy", func(t *testing.T) {
		t.Parallel()
		mode, err := ParseDeploymentModeOption("KataDeploy")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if mode != KataDeployOption {
			t.Errorf("expected KataDeployOption, got %v", mode)
		}
	})

	t.Run("ParseDeploymentModeOption accepts KataDeployFallback", func(t *testing.T) {
		t.Parallel()
		mode, err := ParseDeploymentModeOption("KataDeployFallback")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if mode != KataDeployFallbackOption {
			t.Errorf("expected KataDeployFallbackOption, got %v", mode)
		}
	})
}
