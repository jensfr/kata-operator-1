package controllers

import (
	"context"
	"time"

	"github.com/go-logr/logr"
	kataconfigurationv1 "github.com/openshift/sandboxed-containers-operator/api/v1"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

// KataNodeCoverageReconciler watches Node creates and label/taint changes
// to trigger upstream-style coverage dispatchers. It does not evaluate
// Kata selectors or readiness; the dispatcher's --skip-satisfied-nodes
// determines which selected nodes actually need installation.
//
// This replaces upstream's periodic CronJob trigger with an event-driven
// operator trigger. Everything below the trigger is upstream.
type KataNodeCoverageReconciler struct {
	client.Client
	Log    logr.Logger
	Scheme *runtime.Scheme
}

func (r *KataNodeCoverageReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues("node", req.Name)

	// Get current KataConfig
	kataList := &kataconfigurationv1.KataConfigList{}
	if err := r.Client.List(ctx, kataList); err != nil {
		return ctrl.Result{}, err
	}
	if len(kataList.Items) == 0 {
		return ctrl.Result{}, nil
	}

	kc := &kataList.Items[0]

	if !kc.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	// Resolve deployment mode through the same logic as the main controller.
	// KataDeployFallback on a cluster with MCO resolves to MachineConfig, not KataDeploy.
	fgCM := &corev1.ConfigMap{}
	if err := r.Client.Get(ctx, types.NamespacedName{
		Name: FgConfigMapName, Namespace: OperatorNamespace,
	}, fgCM); err != nil {
		return ctrl.Result{}, nil
	}
	mode, err := ParseDeploymentModeOption(fgCM.Data[DeploymentModeConfig])
	if err != nil {
		return ctrl.Result{}, nil
	}

	reconciler := &KataConfigOpenShiftReconciler{
		Client:     r.Client,
		Log:        r.Log,
		Scheme:     r.Scheme,
		kataConfig: kc,
	}

	if err := reconciler.handleDeploymentModeFeature(mode); err != nil {
		return ctrl.Result{}, nil
	}
	if reconciler.DeploymentMode != KataDeployMode {
		return ctrl.Result{}, nil
	}

	// Only if rollout has converged: observed must equal desired.
	observed := kc.Annotations[annObservedGeneration]
	desired, err := reconciler.dispatcherDesiredGeneration()
	if err != nil {
		return ctrl.Result{RequeueAfter: 30 * time.Second}, err
	}
	if observed != desired {
		log.Info("kata-coverage: generation not converged, requeueing",
			"observed", truncate(observed, 12), "desired", truncate(desired, 12))
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	active, err := reconciler.hasActiveFleetOperation()
	if err != nil {
		return ctrl.Result{RequeueAfter: 15 * time.Second}, err
	}
	if active {
		log.Info("kata-coverage: fleet operation active, requeueing")
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}

	// Create coverage dispatcher
	log.Info("kata-coverage: triggering coverage run")
	if err := reconciler.createCoverageDispatcher(observed); err != nil {
		log.Error(err, "kata-coverage: failed to create coverage dispatcher")
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	return ctrl.Result{}, nil
}

func (r *KataNodeCoverageReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("kata-node-coverage").
		WatchesRawSource(
			source.Kind(mgr.GetCache(), &corev1.Node{},
				handler.TypedEnqueueRequestsFromMapFunc(
					func(ctx context.Context, node *corev1.Node) []reconcile.Request {
						return []reconcile.Request{{
							NamespacedName: types.NamespacedName{Name: "kata-node-coverage"},
						}}
					},
				),
				predicate.TypedFuncs[*corev1.Node]{
					CreateFunc: func(e event.TypedCreateEvent[*corev1.Node]) bool {
						return true
					},
					UpdateFunc: func(e event.TypedUpdateEvent[*corev1.Node]) bool {
						// Only fire on label or taint changes
						oldLabels := e.ObjectOld.GetLabels()
						newLabels := e.ObjectNew.GetLabels()
						if len(oldLabels) != len(newLabels) {
							return true
						}
						for k, v := range oldLabels {
							if newLabels[k] != v {
								return true
							}
						}
						if !apiequality.Semantic.DeepEqual(
							e.ObjectOld.Spec.Taints,
							e.ObjectNew.Spec.Taints,
						) {
							return true
						}
						return false
					},
					DeleteFunc: func(e event.TypedDeleteEvent[*corev1.Node]) bool {
						return false
					},
				},
			),
		).
		Complete(r)
}
