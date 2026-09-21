package controllers

import (
	"context"
	"os"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const (
	kataDeployFinalizer = "kataconfiguration.openshift.io/kata-deploy-finalizer"
	kataRuntimeLabel    = "katacontainers.io/kata-runtime"
	annManagedBy        = "kata.openshift.io/managed-by"
)

func (r *KataConfigOpenShiftReconciler) getKataDeployImage() string {
	return os.Getenv("RELATED_IMAGE_KATA_DEPLOY")
}

func (r *KataConfigOpenShiftReconciler) getHelperImage() string {
	return os.Getenv("RELATED_IMAGE_OSC_HELPER")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func (r *KataConfigOpenShiftReconciler) finalizeKataDeployDeletion() (ctrl.Result, error) {
	r.resetInProgressCondition()

	if err := r.deleteDaemonsetForMonitor(); err != nil {
		return ctrl.Result{Requeue: true, RequeueAfter: 15 * time.Second}, err
	}

	if r.kataConfig.Spec.EnablePeerPods {
		if err := r.disablePeerPodsMiscConfigs(); err != nil {
			return ctrl.Result{Requeue: true, RequeueAfter: 15 * time.Second}, err
		}
	}

	if err := r.deleteScc(); err != nil {
		return ctrl.Result{Requeue: true, RequeueAfter: 15 * time.Second}, err
	}

	controllerutil.RemoveFinalizer(r.kataConfig, kataDeployFinalizer)
	if err := r.Client.Update(context.TODO(), r.kataConfig); err != nil {
		return ctrl.Result{}, err
	}

	r.Log.Info("kata-deploy: deletion complete")
	return ctrl.Result{}, nil
}

func int32Ptr(i int32) *int32 { return &i }
func int64Ptr(i int64) *int64 { return &i }
func boolPtr(b bool) *bool    { return &b }
