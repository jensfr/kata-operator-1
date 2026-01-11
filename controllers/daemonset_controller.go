/*
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

package controllers

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	kataconfigurationv1 "github.com/openshift/sandboxed-containers-operator/api/v1"
)

const (
	kataDeployDaemonSetName      = "kata-deploy"
	kataDeployNamespace          = "openshift-sandboxed-containers-operator"
	kataDeployServiceAccountName = "controller-manager"
	kataDeployFinalizer          = "kataconfiguration.openshift.io/daemonset-finalizer"

	// Labels used for tracking kata runtime installation and cleanup
	// kata-deploy sets kataRuntimeLabel to:
	// - "true" during installation (node can run kata workloads)
	// - "cleanup" during cleanup (node is being cleaned up)
	// The controller uses the "cleanup" value to detect cleanup completion.
	kataRuntimeLabel = "katacontainers.io/kata-runtime"
)

// DaemonSetReconciler reconciles KataConfig using DaemonSet deployment mode.
// This is used for HCP clusters (ROSA, ARO, IBM Cloud ROKS) where MCO is not available.
type DaemonSetReconciler struct {
	client.Client
	Log    logr.Logger
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=kataconfiguration.openshift.io,resources=kataconfigs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kataconfiguration.openshift.io,resources=kataconfigs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps,resources=daemonsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=node.k8s.io,resources=runtimeclasses,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch;patch;update
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch

// Reconcile handles the DaemonSet-based deployment of kata containers
func (r *DaemonSetReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues("kataconfig", req.NamespacedName)

	// 1. Fetch the KataConfig
	kataConfig := &kataconfigurationv1.KataConfig{}
	if err := r.Get(ctx, req.NamespacedName, kataConfig); err != nil {
		if k8serrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// 2. Check if we should handle this (DaemonSet mode)
	shouldReconcile, err := r.shouldReconcile(kataConfig)
	if err != nil {
		log.Error(err, "Failed to determine deployment mode")
		return ctrl.Result{}, err
	}
	if !shouldReconcile {
		log.V(1).Info("Skipping reconcile - not in DaemonSet mode")
		return ctrl.Result{}, nil
	}

	// 3. Add finalizer if not present
	if !controllerutil.ContainsFinalizer(kataConfig, kataDeployFinalizer) {
		controllerutil.AddFinalizer(kataConfig, kataDeployFinalizer)
		if err := r.Update(ctx, kataConfig); err != nil {
			return ctrl.Result{}, err
		}
	}

	// 4. Handle deletion
	if !kataConfig.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, kataConfig)
	}

	// 5. Ensure DaemonSet exists and is up to date
	if err := r.reconcileDaemonSet(ctx, kataConfig); err != nil {
		log.Error(err, "Failed to reconcile DaemonSet")
		return ctrl.Result{Requeue: true, RequeueAfter: 15 * time.Second}, err
	}

	// 6. Ensure RuntimeClasses exist
	if err := r.reconcileRuntimeClasses(ctx, kataConfig); err != nil {
		log.Error(err, "Failed to reconcile RuntimeClasses")
		return ctrl.Result{Requeue: true, RequeueAfter: 15 * time.Second}, err
	}

	// 7. Update status
	if err := r.updateStatus(ctx, kataConfig); err != nil {
		log.Error(err, "Failed to update status")
		return ctrl.Result{Requeue: true, RequeueAfter: 15 * time.Second}, err
	}

	return ctrl.Result{}, nil
}

// shouldReconcile determines if this controller should handle the KataConfig
func (r *DaemonSetReconciler) shouldReconcile(kataConfig *kataconfigurationv1.KataConfig) (bool, error) {
	mode := kataConfig.Spec.DeploymentMode

	// Explicit DaemonSet mode in spec
	if mode == kataconfigurationv1.DeploymentModeDaemonSet {
		r.Log.Info("DaemonSet mode explicitly set in KataConfig spec")
		return true, nil
	}

	// Check the feature gate ConfigMap for deploymentMode setting
	cfgMap := &corev1.ConfigMap{}
	err := r.Client.Get(context.Background(), types.NamespacedName{
		Name:      "osc-feature-gates",
		Namespace: kataDeployNamespace,
	}, cfgMap)
	if err == nil {
		if cfgMode, ok := cfgMap.Data["deploymentMode"]; ok && cfgMode == "DaemonSet" {
			r.Log.Info("DaemonSet mode set via osc-feature-gates ConfigMap")
			return true, nil
		}
	} else if !k8serrors.IsNotFound(err) {
		r.Log.Error(err, "Failed to get osc-feature-gates ConfigMap")
	}

	// Auto mode - check if MCO is available
	if mode == kataconfigurationv1.DeploymentModeAuto || mode == "" {
		hasMCO, err := r.isMachineConfigAvailable()
		if err != nil {
			return false, err
		}
		// If MCO is NOT available, we should use DaemonSet mode
		if !hasMCO {
			r.Log.Info("MCO not available, using DaemonSet mode")
			return true, nil
		}
	}

	// MachineConfig mode - don't handle
	r.Log.V(1).Info("Skipping - not in DaemonSet mode")
	return false, nil
}

// reconcileDaemonSet creates/updates the kata-deploy DaemonSet
func (r *DaemonSetReconciler) reconcileDaemonSet(ctx context.Context, kataConfig *kataconfigurationv1.KataConfig) error {
	ds := r.buildDaemonSet(kataConfig)

	// Set KataConfig as the owner
	if err := controllerutil.SetControllerReference(kataConfig, ds, r.Scheme); err != nil {
		r.Log.Error(err, "Failed to set controller reference on kata-deploy DaemonSet")
		return err
	}

	// Get or create
	foundDs := &appsv1.DaemonSet{}
	err := r.Client.Get(ctx, types.NamespacedName{Name: ds.Name, Namespace: ds.Namespace}, foundDs)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			r.Log.Info("Creating kata-deploy DaemonSet", "namespace", ds.Namespace, "name", ds.Name)
			return r.Client.Create(ctx, ds)
		}
		return err
	}

	// Update if needed
	r.Log.Info("Updating kata-deploy DaemonSet", "namespace", ds.Namespace, "name", ds.Name)
	foundDs.Spec = ds.Spec
	return r.Client.Update(ctx, foundDs)
}

// buildDaemonSet creates the kata-deploy DaemonSet spec
func (r *DaemonSetReconciler) buildDaemonSet(kataConfig *kataconfigurationv1.KataConfig) *appsv1.DaemonSet {
	labels := map[string]string{
		"app":                          "kata-deploy",
		"app.kubernetes.io/name":       "kata-deploy",
		"app.kubernetes.io/managed-by": "sandboxed-containers-operator",
	}

	// Build environment variables from KataConfig
	env := r.buildEnvVars(kataConfig)

	// Privileged container is required for host access
	privileged := true

	// Max unavailable for rolling update
	maxUnavailable := intstr.FromString("10%")

	return &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      kataDeployDaemonSetName,
			Namespace: kataDeployNamespace,
			Labels:    labels,
		},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: labels,
			},
			UpdateStrategy: appsv1.DaemonSetUpdateStrategy{
				Type: appsv1.RollingUpdateDaemonSetStrategyType,
				RollingUpdate: &appsv1.RollingUpdateDaemonSet{
					MaxUnavailable: &maxUnavailable,
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
				},
				Spec: corev1.PodSpec{
					HostNetwork:        true,
					HostPID:            true,
					ServiceAccountName: kataDeployServiceAccountName,
					PriorityClassName:  "system-node-critical",
					NodeSelector: map[string]string{
						"node-role.kubernetes.io/worker": "",
					},
					Tolerations: []corev1.Toleration{
						{
							Operator: corev1.TolerationOpExists,
						},
					},
					Containers: []corev1.Container{
						{
							Name:            "kata-deploy",
							Image:           r.getKataDeployImage(),
							ImagePullPolicy: corev1.PullAlways,
							Command: []string{
								"/usr/bin/kata-deploy",
								"install",
							},
							Env: env,
							SecurityContext: &corev1.SecurityContext{
								Privileged: &privileged,
							},
							VolumeMounts: r.buildVolumeMounts(),
							// preStop hook removes the kata-runtime label to prevent scheduling
							// kata workloads during DaemonSet rolling updates
							Lifecycle: &corev1.Lifecycle{
								PreStop: &corev1.LifecycleHandler{
									Exec: &corev1.ExecAction{
										Command: []string{"/usr/bin/kata-deploy", "remove-label"},
									},
								},
							},
						},
					},
					Volumes: r.buildVolumes(),
				},
			},
		},
	}
}

// getKataDeployImage returns the kata-deploy container image
func (r *DaemonSetReconciler) getKataDeployImage() string {
	// RELATED_IMAGE_ prefix is required for disconnected/air-gapped installs
	image := os.Getenv("RELATED_IMAGE_KATA_DEPLOY")
	if image == "" {
		// Fallback - should be set by the operator deployment
		// Using Rust-based kata-deploy with /usr/bin/kata-deploy binary
		image = "quay.io/jensfr/kata-deploy-rust:daemonset-test"
		r.Log.Info("RELATED_IMAGE_KATA_DEPLOY not set, using default", "image", image)
	}
	return image
}

// buildEnvVars creates environment variables for the kata-deploy container
func (r *DaemonSetReconciler) buildEnvVars(kataConfig *kataconfigurationv1.KataConfig) []corev1.EnvVar {
	env := []corev1.EnvVar{
		{
			Name: "NODE_NAME",
			ValueFrom: &corev1.EnvVarSource{
				FieldRef: &corev1.ObjectFieldSelector{
					FieldPath: "spec.nodeName",
				},
			},
		},
		{
			// Use architecture-specific env var for the Rust kata-deploy binary
			Name:  "SHIMS_X86_64",
			Value: "qemu qemu-tdx qemu-snp qemu-se",
		},
	}

	// Add platform-specific configuration
	platform := kataConfig.Spec.Platform
	if platform == "" {
		platform = "rhcos" // Default for OpenShift
	}
	env = append(env, corev1.EnvVar{Name: "PLATFORM", Value: platform})

	// Add host component paths if specified
	if paths := kataConfig.Spec.HostComponentPaths; paths != nil {
		if paths.QemuPath != "" {
			env = append(env, corev1.EnvVar{Name: "QEMU_PATH", Value: paths.QemuPath})
		}
		if paths.VirtiofsdPath != "" {
			env = append(env, corev1.EnvVar{Name: "VIRTIOFSD_PATH", Value: paths.VirtiofsdPath})
		}
		if paths.KernelPath != "" {
			env = append(env, corev1.EnvVar{Name: "KERNEL_PATH", Value: paths.KernelPath})
		}
	}

	// Add initrd configuration
	if initrd := kataConfig.Spec.InitrdConfig; initrd != nil {
		if initrd.BuildAtRuntime != nil {
			env = append(env, corev1.EnvVar{
				Name:  "BUILD_INITRD",
				Value: fmt.Sprintf("%t", *initrd.BuildAtRuntime),
			})
		}
		if initrd.PrebuiltPath != "" {
			env = append(env, corev1.EnvVar{Name: "INITRD_PATH", Value: initrd.PrebuiltPath})
		}
	}

	// Add debug configuration
	if kataConfig.Spec.LogLevel == "debug" {
		env = append(env, corev1.EnvVar{Name: "DEBUG", Value: "true"})
	}

	// Add extensions image if specified or auto-detected
	extensionsImage := r.getExtensionsImage(kataConfig)
	if extensionsImage != "" {
		env = append(env, corev1.EnvVar{Name: "EXTENSIONS_IMAGE", Value: extensionsImage})
	}

	return env
}

// buildVolumeMounts creates the volume mounts for the kata-deploy container
// Matching upstream helm chart: only /host, /etc/crio, /etc/containerd
func (r *DaemonSetReconciler) buildVolumeMounts() []corev1.VolumeMount {
	return []corev1.VolumeMount{
		{
			Name:      "host",
			MountPath: "/host/",
		},
		{
			Name:      "crio-conf",
			MountPath: "/etc/crio/",
		},
		{
			Name:      "containerd-conf",
			MountPath: "/etc/containerd/",
		},
	}
}

// buildVolumes creates the volumes for the kata-deploy pod
// Matching upstream helm chart: only host root, crio-conf, containerd-conf
func (r *DaemonSetReconciler) buildVolumes() []corev1.Volume {
	return []corev1.Volume{
		{
			Name: "host",
			VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{
					Path: "/",
				},
			},
		},
		{
			Name: "crio-conf",
			VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{
					Path: "/etc/crio/",
				},
			},
		},
		{
			Name: "containerd-conf",
			VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{
					Path: "/etc/containerd/",
				},
			},
		},
	}
}

// getExtensionsImage returns the RHCOS extension image URL.
// Priority:
// 1. If ExtensionsImage is set in KataConfig spec, use that
// 2. Otherwise, try to auto-detect from ClusterVersion
// 3. If auto-detection fails, return empty string (kata-deploy uses baked RPMs)
func (r *DaemonSetReconciler) getExtensionsImage(kataConfig *kataconfigurationv1.KataConfig) string {
	// Use explicit value from spec if provided
	if kataConfig.Spec.ExtensionsImage != "" {
		r.Log.Info("Using ExtensionsImage from KataConfig spec", "image", kataConfig.Spec.ExtensionsImage)
		return kataConfig.Spec.ExtensionsImage
	}

	// Try to auto-detect from ClusterVersion
	extensionsImage, err := r.detectExtensionsImage()
	if err != nil {
		r.Log.Info("Could not auto-detect extensions image, kata-deploy will use baked RPMs",
			"error", err.Error())
		return ""
	}

	if extensionsImage != "" {
		r.Log.Info("Auto-detected extensions image from release", "image", extensionsImage)
	}
	return extensionsImage
}

// detectExtensionsImage attempts to find the rhel-coreos-extensions image from the cluster's release.
// This queries the ClusterVersion to get the release image, then looks up the extension image reference.
func (r *DaemonSetReconciler) detectExtensionsImage() (string, error) {
	// Get ClusterVersion to find the release image
	cv := &unstructured.Unstructured{}
	cv.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "config.openshift.io",
		Version: "v1",
		Kind:    "ClusterVersion",
	})

	err := r.Client.Get(context.Background(), types.NamespacedName{Name: "version"}, cv)
	if err != nil {
		return "", fmt.Errorf("failed to get ClusterVersion: %w", err)
	}

	// Extract release image from status.desired.image
	releaseImage, found, err := unstructured.NestedString(cv.Object, "status", "desired", "image")
	if err != nil || !found || releaseImage == "" {
		return "", fmt.Errorf("could not find release image in ClusterVersion")
	}

	r.Log.V(1).Info("Found release image from ClusterVersion", "releaseImage", releaseImage)

	// To get the extension image, we need to query the release image manifest.
	// This requires registry credentials and manifest parsing.
	// For now, we'll check if there's an ImageContentSourcePolicy or ImageDigestMirrorSet
	// that provides the mapping, or return empty to use the fallback.
	//
	// TODO: Implement full release image query using registry API
	// For now, log the release image so users can manually set ExtensionsImage
	r.Log.Info("Release image detected but auto-lookup of extension image not yet implemented",
		"releaseImage", releaseImage,
		"hint", "Set spec.extensionsImage in KataConfig, or run: oc adm release info "+releaseImage+" --image-for=rhel-coreos-extensions")

	return "", nil
}

// reconcileRuntimeClasses creates RuntimeClasses for each configured shim
// using the shared RuntimeClassHelper
func (r *DaemonSetReconciler) reconcileRuntimeClasses(ctx context.Context, kataConfig *kataconfigurationv1.KataConfig) error {
	helper := NewRuntimeClassHelper(r.Client, r.Scheme)

	// Node selector for DaemonSet mode uses the kata-deploy label
	nodeSelector := map[string]string{
		"katacontainers.io/kata-runtime": "true",
	}

	// Get the standard RuntimeClass configurations
	configs := KataRuntimeClassConfigs(nodeSelector)

	for _, config := range configs {
		if err := helper.CreateOrUpdateRuntimeClass(ctx, kataConfig, config); err != nil {
			return err
		}
		helper.UpdateKataConfigRuntimeClasses(kataConfig, config.Name)
	}
	return nil
}

// handleDeletion handles the cleanup when KataConfig is deleted.
// Cleanup detection uses pod exit status instead of node labels to avoid
// modifying upstream kata-deploy behavior:
// 1. Switch DaemonSet to cleanup mode
// 2. Wait for cleanup pods to exit with code 0 (successful cleanup)
// 3. Delete DaemonSet and clean up node labels
// 4. Remove finalizer to allow KataConfig deletion
func (r *DaemonSetReconciler) handleDeletion(ctx context.Context, kataConfig *kataconfigurationv1.KataConfig) (ctrl.Result, error) {
	r.Log.Info("Handling KataConfig deletion in DaemonSet mode")

	// 1. Get the DaemonSet first to determine how many nodes should run cleanup
	ds := &appsv1.DaemonSet{}
	err := r.Client.Get(ctx, types.NamespacedName{
		Name:      kataDeployDaemonSetName,
		Namespace: kataDeployNamespace,
	}, ds)

	if err != nil {
		if k8serrors.IsNotFound(err) {
			// DaemonSet already deleted - try to get node names from any remaining terminating pods
			// before falling back to label search
			r.Log.Info("DaemonSet already deleted, finalizing")
			kataNodeNames := r.getKataDeployNodeNames(ctx)
			return r.cleanupLabelsAndFinalize(ctx, kataConfig, kataNodeNames)
		}
		return ctrl.Result{}, err
	}

	// Get expected number of nodes from DaemonSet status
	expectedNodes := int(ds.Status.DesiredNumberScheduled)
	if expectedNodes == 0 {
		r.Log.Info("No nodes scheduled for DaemonSet, finalizing")
		if err := r.Client.Delete(ctx, ds); err != nil && !k8serrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		return r.cleanupLabelsAndFinalize(ctx, kataConfig, nil)
	}

	// 2. Check if already in cleanup mode
	isCleanupMode := false
	if len(ds.Spec.Template.Spec.Containers) > 0 {
		for _, arg := range ds.Spec.Template.Spec.Containers[0].Command {
			if arg == "cleanup" {
				isCleanupMode = true
				break
			}
		}
	}

	if !isCleanupMode {
		// Switch to cleanup mode
		r.Log.Info("Switching kata-deploy to cleanup mode", "expectedNodes", expectedNodes)
		ds.Spec.Template.Spec.Containers[0].Command = []string{
			"/usr/bin/kata-deploy",
			"cleanup",
		}
		if err := r.Client.Update(ctx, ds); err != nil {
			return ctrl.Result{}, err
		}
		// Requeue to wait for cleanup pods to start
		return ctrl.Result{Requeue: true, RequeueAfter: 10 * time.Second}, nil
	}

	// 3. Check cleanup completion by examining pod exit status
	// kata-deploy cleanup exits with code 0 on success. We detect this by checking
	// if pods have terminated successfully (either in current state or lastState).
	completedCount, err := r.countCleanupCompletedPods(ctx)
	if err != nil {
		r.Log.Error(err, "Failed to count cleanup completed pods")
		return ctrl.Result{Requeue: true, RequeueAfter: 10 * time.Second}, nil
	}

	r.Log.Info("Cleanup progress", "expectedNodes", expectedNodes, "cleanupComplete", completedCount)

	if completedCount < expectedNodes {
		// Not all nodes have completed cleanup, wait
		r.Log.Info("Waiting for cleanup to complete on all nodes",
			"completed", completedCount,
			"expected", expectedNodes)
		return ctrl.Result{Requeue: true, RequeueAfter: 15 * time.Second}, nil
	}

	// 4. Capture node names BEFORE deleting DaemonSet (pods will be gone after deletion)
	// This ensures we know exactly which nodes to clean up, avoiding cache timing issues.
	kataNodeNames := r.getKataDeployNodeNames(ctx)
	r.Log.Info("Captured kata-deploy node names for cleanup", "nodes", kataNodeNames)

	// 5. All nodes have completed cleanup, delete the DaemonSet
	r.Log.Info("All nodes completed cleanup, deleting kata-deploy DaemonSet")
	if err := r.Client.Delete(ctx, ds); err != nil && !k8serrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}

	// 6. Wait for kata-deploy pods to terminate before cleaning up labels.
	podList := &corev1.PodList{}
	if err := r.Client.List(ctx, podList,
		client.InNamespace(kataDeployNamespace),
		client.MatchingLabels{"app": "kata-deploy"}); err != nil {
		r.Log.Error(err, "Failed to list kata-deploy pods")
	} else if len(podList.Items) > 0 {
		r.Log.Info("Waiting for kata-deploy pods to terminate", "podCount", len(podList.Items))
		return ctrl.Result{Requeue: true, RequeueAfter: 5 * time.Second}, nil
	}

	// 7. Clean up node labels and finalize
	return r.cleanupLabelsAndFinalize(ctx, kataConfig, kataNodeNames)
}

// countCleanupCompletedPods counts kata-deploy pods that have successfully completed cleanup.
// A pod is considered to have completed cleanup if its container exited with code 0.
// This can be detected from either the current state (terminated) or lastState (if restarted).
func (r *DaemonSetReconciler) countCleanupCompletedPods(ctx context.Context) (int, error) {
	podList := &corev1.PodList{}
	if err := r.Client.List(ctx, podList,
		client.InNamespace(kataDeployNamespace),
		client.MatchingLabels{"app": "kata-deploy"}); err != nil {
		return 0, err
	}

	completedCount := 0
	for _, pod := range podList.Items {
		if r.isPodCleanupComplete(&pod) {
			completedCount++
		}
	}
	return completedCount, nil
}

// getKataDeployNodeNames returns the names of nodes where kata-deploy pods are/were running.
// This is used to precisely track which nodes need label cleanup, avoiding reliance on
// cached label state which can cause race conditions.
func (r *DaemonSetReconciler) getKataDeployNodeNames(ctx context.Context) []string {
	podList := &corev1.PodList{}
	if err := r.Client.List(ctx, podList,
		client.InNamespace(kataDeployNamespace),
		client.MatchingLabels{"app": "kata-deploy"}); err != nil {
		r.Log.Error(err, "Failed to list kata-deploy pods for node name extraction")
		return nil
	}

	// Use a map to deduplicate node names
	nodeSet := make(map[string]struct{})
	for _, pod := range podList.Items {
		if pod.Spec.NodeName != "" {
			nodeSet[pod.Spec.NodeName] = struct{}{}
		}
	}

	nodeNames := make([]string, 0, len(nodeSet))
	for nodeName := range nodeSet {
		nodeNames = append(nodeNames, nodeName)
	}
	return nodeNames
}

// isPodCleanupComplete checks if a kata-deploy pod has completed cleanup successfully.
// Returns true if the kata-deploy container exited with code 0.
func (r *DaemonSetReconciler) isPodCleanupComplete(pod *corev1.Pod) bool {
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name != "kata-deploy" {
			continue
		}
		// Check current state - container terminated with exit code 0
		if cs.State.Terminated != nil && cs.State.Terminated.ExitCode == 0 {
			return true
		}
		// Check last state - container previously terminated with exit code 0
		// This catches the case where the DaemonSet controller restarted the pod
		if cs.LastTerminationState.Terminated != nil && cs.LastTerminationState.Terminated.ExitCode == 0 {
			return true
		}
	}
	return false
}

// getNodesWithLabel returns all nodes that have the specified label with the given value
func (r *DaemonSetReconciler) getNodesWithLabel(ctx context.Context, labelKey, labelValue string) ([]corev1.Node, error) {
	nodeList := &corev1.NodeList{}
	if err := r.Client.List(ctx, nodeList, client.MatchingLabels{labelKey: labelValue}); err != nil {
		return nil, err
	}
	return nodeList.Items, nil
}

// cleanupLabelsAndFinalize removes the kata-runtime label from nodes and finalizes deletion.
// nodeNames contains the list of nodes that had kata-deploy pods running. If nil or empty,
// falls back to searching for nodes with the kata-runtime label (for edge cases like
// DaemonSet already deleted before we could capture node names).
func (r *DaemonSetReconciler) cleanupLabelsAndFinalize(ctx context.Context, kataConfig *kataconfigurationv1.KataConfig, nodeNames []string) (ctrl.Result, error) {
	// If we have explicit node names from kata-deploy pods, use those.
	// Otherwise fall back to searching by label (handles edge cases).
	if len(nodeNames) == 0 {
		r.Log.Info("No node names provided, falling back to label search")
		nodeList := &corev1.NodeList{}
		if err := r.Client.List(ctx, nodeList); err != nil {
			r.Log.Error(err, "Failed to list nodes")
			// Continue anyway - don't block deletion
		} else {
			for _, node := range nodeList.Items {
				if _, exists := node.Labels[kataRuntimeLabel]; exists {
					nodeNames = append(nodeNames, node.Name)
				}
			}
		}
	}

	r.Log.Info("Removing kata labels from nodes", "nodeCount", len(nodeNames), "nodes", nodeNames)

	// Remove the kata-runtime label from each node
	for _, nodeName := range nodeNames {
		if err := r.removeNodeLabel(ctx, nodeName, kataRuntimeLabel); err != nil {
			r.Log.Error(err, "Failed to remove kata-runtime label from node", "node", nodeName)
		}
	}

	// Second pass: kata-deploy might have set the label via an in-flight API call that
	// completed after our removal. Wait for cache to sync, then re-check and remove.
	// This handles the race where kata-deploy's label_node() completes after we removed the label.
	// 2 seconds is sufficient to allow in-flight K8s API calls to complete and cache to sync.
	time.Sleep(2 * time.Second)
	for _, nodeName := range nodeNames {
		node := &corev1.Node{}
		if err := r.Client.Get(ctx, types.NamespacedName{Name: nodeName}, node); err != nil {
			continue
		}
		if _, exists := node.Labels[kataRuntimeLabel]; exists {
			r.Log.Info("Second pass: removing kata label that was re-added", "node", nodeName)
			if err := r.removeNodeLabel(ctx, nodeName, kataRuntimeLabel); err != nil {
				r.Log.Error(err, "Failed to remove kata-runtime label on second pass", "node", nodeName)
			}
		}
	}

	return r.finalizeDeletion(ctx, kataConfig)
}

// finalizeDeletion removes the finalizer to allow KataConfig deletion
func (r *DaemonSetReconciler) finalizeDeletion(ctx context.Context, kataConfig *kataconfigurationv1.KataConfig) (ctrl.Result, error) {
	// RuntimeClasses are deleted automatically via owner references

	// Remove finalizer
	controllerutil.RemoveFinalizer(kataConfig, kataDeployFinalizer)
	if err := r.Update(ctx, kataConfig); err != nil {
		return ctrl.Result{}, err
	}

	r.Log.Info("KataConfig deletion complete")
	return ctrl.Result{}, nil
}

// removeNodeLabel removes a label from a node with retry logic for conflicts
func (r *DaemonSetReconciler) removeNodeLabel(ctx context.Context, nodeName, labelKey string) error {
	// Retry up to 3 times to handle optimistic locking conflicts
	for attempt := 0; attempt < 3; attempt++ {
		node := &corev1.Node{}
		if err := r.Client.Get(ctx, types.NamespacedName{Name: nodeName}, node); err != nil {
			if k8serrors.IsNotFound(err) {
				return nil // Node doesn't exist, nothing to do
			}
			return err
		}

		if node.Labels == nil {
			return nil
		}

		if _, exists := node.Labels[labelKey]; !exists {
			return nil // Label doesn't exist
		}

		delete(node.Labels, labelKey)
		err := r.Client.Update(ctx, node)
		if err == nil {
			return nil
		}

		// If conflict, retry with fresh data
		if k8serrors.IsConflict(err) {
			r.Log.V(1).Info("Conflict updating node, retrying", "node", nodeName, "attempt", attempt+1)
			time.Sleep(100 * time.Millisecond)
			continue
		}
		return err
	}
	return fmt.Errorf("failed to remove label %s from node %s after 3 attempts", labelKey, nodeName)
}

// updateStatus updates the KataConfig status with DaemonSet information
func (r *DaemonSetReconciler) updateStatus(ctx context.Context, kataConfig *kataconfigurationv1.KataConfig) error {
	// Get DaemonSet status
	ds := &appsv1.DaemonSet{}
	if err := r.Get(ctx, client.ObjectKey{
		Namespace: kataDeployNamespace,
		Name:      kataDeployDaemonSetName,
	}, ds); err != nil {
		if k8serrors.IsNotFound(err) {
			return nil // DaemonSet doesn't exist yet
		}
		return err
	}

	// Update KataConfig status
	kataConfig.Status.ActiveDeploymentMode = kataconfigurationv1.DeploymentModeDaemonSet
	kataConfig.Status.DaemonSetStatus = &kataconfigurationv1.DaemonSetDeploymentStatus{
		DesiredNumberScheduled: ds.Status.DesiredNumberScheduled,
		CurrentNumberScheduled: ds.Status.CurrentNumberScheduled,
		NumberReady:            ds.Status.NumberReady,
		UpdatedNumberScheduled: ds.Status.UpdatedNumberScheduled,
		ObservedGeneration:     ds.Status.ObservedGeneration,
	}

	// Update node counts
	kataConfig.Status.KataNodes.NodeCount = int(ds.Status.DesiredNumberScheduled)
	kataConfig.Status.KataNodes.ReadyNodeCount = int(ds.Status.NumberReady)

	return r.Status().Update(ctx, kataConfig)
}

// isMachineConfigAvailable checks if the MachineConfig CRD is available in the cluster.
// This is used to determine whether to use MachineConfig or DaemonSet mode.
func (r *DaemonSetReconciler) isMachineConfigAvailable() (bool, error) {
	machineConfig := &unstructured.Unstructured{}
	machineConfig.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "machineconfiguration.openshift.io",
		Version: "v1",
		Kind:    "MachineConfig",
	})

	// Attempt to GET any MachineConfig to verify if the GVK is known.
	// If the CRD isn't installed, it will return a NoMatchError.
	err := r.Client.Get(context.Background(), client.ObjectKey{Name: "dummy-machine-config"}, machineConfig)
	if err == nil || k8serrors.IsNotFound(err) {
		r.Log.Info("MachineConfig CRD is present")
		return true, nil
	}

	if meta.IsNoMatchError(err) {
		r.Log.Info("MachineConfig CRD not found")
		return false, nil
	}

	return false, err
}

// SetupWithManager sets up the controller with the Manager
func (r *DaemonSetReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("kataconfig-daemonset").
		For(&kataconfigurationv1.KataConfig{}).
		Owns(&appsv1.DaemonSet{}).
		Owns(&nodev1.RuntimeClass{}).
		Complete(r)
}
