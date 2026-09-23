package controllers

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"fmt"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

//go:embed manifests/dispatcher-install-template.yaml
var dispatcherInstallTemplate []byte

//go:embed manifests/dispatcher-cleanup-template.yaml
var dispatcherCleanupTemplate []byte

//go:embed manifests/osc_kata_deploy.pp
var dispatcherSELinuxPolicy []byte

const (
	dispatcherImage       = "ghcr.io/kata-containers/k8s-job-dispatcher:0.3.0"
	dispatcherSAName      = "kata-dispatcher"
	templateConfigMapName = "kata-deploy-job-templates"
	selinuxCMName         = "selinux-module"

	annObservedGeneration = "kata.openshift.io/observed-generation"
	labelFleetOperation   = "kata-deploy/operation"
	labelKataConfigUID    = "kata-deploy/kataconfig-uid"
	instanceLabelPrefix   = "kata-deploy.katacontainers.io"
	opRollout  = "rollout"
	opCoverage = "coverage"
	opCleanup  = "cleanup"
)

// dispatcherNodeWorkFlags returns the exact upstream common dispatcher
// contract. All dispatcher variants (rollout, coverage, cleanup) carry these.
func dispatcherNodeWorkFlags() []string {
	return []string{
		"--tracking-label-prefix=kata-deploy-job-dispatcher",
		"--node-label-key=katacontainers.io/kata-runtime",
		"--instance-label-prefix=" + instanceLabelPrefix,
		"--require-node-runtime-version",
		"--require-node-machine-id",
	}
}

func (r *KataConfigOpenShiftReconciler) dispatcherDesiredGeneration() (string, error) {
	combined := r.getKataDeployImage()
	helper := r.getHelperImage()
	installHash := sha256.Sum256(dispatcherInstallTemplate)
	cleanupHash := sha256.Sum256(dispatcherCleanupTemplate)
	policyHash := sha256.Sum256(dispatcherSELinuxPolicy)
	selector, err := r.dispatcherNodeSelector()
	if err != nil {
		return "", err
	}

	h := sha256.New()
	fmt.Fprintf(h, "combined:%s\n", combined)
	fmt.Fprintf(h, "helper:%s\n", helper)
	fmt.Fprintf(h, "install-template:%x\n", installHash[:8])
	fmt.Fprintf(h, "cleanup-template:%x\n", cleanupHash[:8])
	fmt.Fprintf(h, "policy:%x\n", policyHash[:8])
	fmt.Fprintf(h, "selector:%s\n", selector)
	fmt.Fprintf(h, "handler:%s\n", defaultKataDeployRuntime.Handler)
	fmt.Fprintf(h, "dispatcher:%s\n", dispatcherImage)
	fmt.Fprintf(h, "dispatcher-flags:%s\n",
		strings.Join(dispatcherNodeWorkFlags(), ","))
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

func (r *KataConfigOpenShiftReconciler) processKataConfigInstallRequestDispatcher() (ctrl.Result, error) {
	r.Log.Info("kata-deploy-dispatcher: reconciling install")

	if err := r.ensureKataInstallSCC(); err != nil {
		return ctrl.Result{Requeue: true, RequeueAfter: 15 * time.Second}, err
	}

	if !controllerutil.ContainsFinalizer(r.kataConfig, kataDeployFinalizer) {
		controllerutil.AddFinalizer(r.kataConfig, kataDeployFinalizer)
		if err := r.Client.Update(context.TODO(), r.kataConfig); err != nil {
			return ctrl.Result{}, err
		}
	}

	if err := r.ensureDispatcherSELinuxCM(); err != nil {
		return ctrl.Result{Requeue: true, RequeueAfter: 15 * time.Second}, err
	}

	if err := r.ensureDispatcherTemplateCM(); err != nil {
		return ctrl.Result{Requeue: true, RequeueAfter: 15 * time.Second}, err
	}

	desired, err := r.dispatcherDesiredGeneration()
	if err != nil {
		return ctrl.Result{Requeue: true, RequeueAfter: 15 * time.Second}, err
	}
	observed := r.kataConfig.Annotations[annObservedGeneration]

	if observed == desired {
		r.Log.Info("kata-deploy-dispatcher: all nodes at desired generation")
		res, err := r.postKataInstallation()
		if res != nil {
			return *res, err
		}
		return ctrl.Result{}, nil
	}

	// Check for active dispatcher Job
	dispatcherJobName := r.dispatcherJobName(desired)
	activeJob := &batchv1.Job{}
	err = r.Client.Get(context.TODO(), types.NamespacedName{
		Name:      dispatcherJobName,
		Namespace: OperatorNamespace,
	}, activeJob)

	if err == nil {
		for _, c := range activeJob.Status.Conditions {
			if c.Type == batchv1.JobComplete && c.Status == corev1.ConditionTrue {
				r.Log.Info("kata-deploy-dispatcher: rollout succeeded", "generation", desired[:12])
				if r.kataConfig.Annotations == nil {
					r.kataConfig.Annotations = make(map[string]string)
				}
				r.kataConfig.Annotations[annObservedGeneration] = desired
				if err := r.Client.Update(context.TODO(), r.kataConfig); err != nil {
					return ctrl.Result{Requeue: true, RequeueAfter: 5 * time.Second}, err
				}
				res, err := r.postKataInstallation()
				if res != nil {
					return *res, err
				}
				return ctrl.Result{}, nil
			}
			if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
				r.Log.Info("kata-deploy-dispatcher: rollout failed, will retry", "generation", desired[:12])
				bg := metav1.DeletePropagationBackground
				_ = r.Client.Delete(context.TODO(), activeJob, &client.DeleteOptions{
					PropagationPolicy: &bg,
				})
				return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
			}
		}
		r.Log.Info("kata-deploy-dispatcher: rollout in progress")
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}

	if !k8serrors.IsNotFound(err) {
		return ctrl.Result{Requeue: true, RequeueAfter: 15 * time.Second}, err
	}

	// Create dispatcher Job
	if err := r.createDispatcherJob(desired); err != nil {
		return ctrl.Result{Requeue: true, RequeueAfter: 15 * time.Second}, err
	}

	return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
}

func (r *KataConfigOpenShiftReconciler) dispatcherJobName(generation string) string {
	return fmt.Sprintf("kata-rollout-%.12s", generation)
}

func (r *KataConfigOpenShiftReconciler) createDispatcherJob(generation string) error {
	nodeSelector, err := r.dispatcherNodeSelector()
	if err != nil {
		return err
	}
	jobName := r.dispatcherJobName(generation)
	priv := false

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: OperatorNamespace,
			Labels: map[string]string{
				labelFleetOperation: opRollout,
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            int32Ptr(0),
			TTLSecondsAfterFinished: int32Ptr(600),
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					ServiceAccountName: dispatcherSAName,
					RestartPolicy:      corev1.RestartPolicyNever,
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: boolPtr(true),
						RunAsUser:    int64Ptr(65532),
						SeccompProfile: &corev1.SeccompProfile{
							Type: corev1.SeccompProfileTypeRuntimeDefault,
						},
					},
					Containers: []corev1.Container{{
						Name:  "dispatcher",
						Image: dispatcherImage,
						Args: append([]string{
							"--job-template=/etc/kata-job/install-job.yaml",
							"--cleanup-job-template=/etc/kata-job/cleanup-job.yaml",
							fmt.Sprintf("--name-prefix=kata-%.12s", generation),
							"--parallelism=1",
							fmt.Sprintf("--node-selector=%s", nodeSelector),
							"--node-label=true",
							"--claim-node-pending",
							"--wait-node-ready-secs=300",
							fmt.Sprintf("--require-node-handlers=%s", defaultKataDeployRuntime.Handler),
						}, dispatcherNodeWorkFlags()...),
						Env: []corev1.EnvVar{{
							Name: "POD_NAMESPACE",
							ValueFrom: &corev1.EnvVarSource{
								FieldRef: &corev1.ObjectFieldSelector{
									FieldPath: "metadata.namespace",
								},
							},
						}},
						VolumeMounts: []corev1.VolumeMount{{
							Name:      "job-templates",
							MountPath: "/etc/kata-job",
							ReadOnly:  true,
						}},
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: &priv,
							Capabilities: &corev1.Capabilities{
								Drop: []corev1.Capability{"ALL"},
							},
						},
					}},
					Volumes: []corev1.Volume{{
						Name: "job-templates",
						VolumeSource: corev1.VolumeSource{
							ConfigMap: &corev1.ConfigMapVolumeSource{
								LocalObjectReference: corev1.LocalObjectReference{
									Name: templateConfigMapName,
								},
							},
						},
					}},
				},
			},
		},
	}

	if err := controllerutil.SetControllerReference(r.kataConfig, job, r.Scheme); err != nil {
		return err
	}

	r.Log.Info("kata-deploy-dispatcher: creating rollout",
		"job", jobName, "generation", generation[:12])
	return r.Client.Create(context.TODO(), job)
}

// dispatcherNodeSelector builds the label selector for the dispatcher.
// KataDeploy does not use MachineConfigPools for topology. It selects
// nodes purely by Kubernetes labels: the KataConfig pool selector AND
// the worker role. Customer-owned MCPs remain untouched.
func (r *KataConfigOpenShiftReconciler) dispatcherNodeSelector() (string, error) {
	const worker = "node-role.kubernetes.io/worker="

	ls := &metav1.LabelSelector{}
	if r.kataConfig.Spec.KataConfigPoolSelector != nil {
		ls = r.kataConfig.Spec.KataConfigPoolSelector.DeepCopy()
	}

	if r.kataConfig.Spec.CheckNodeEligibility {
		ls = metav1.AddLabelToSelector(ls, "feature.node.kubernetes.io/runtime.kata", "true")
	}

	sel, err := metav1.LabelSelectorAsSelector(ls)
	if err != nil {
		return "", err
	}
	if sel.Empty() {
		return worker, nil
	}

	return worker + "," + sel.String(), nil
}

func isTerminalJob(job *batchv1.Job) bool {
	for _, c := range job.Status.Conditions {
		if c.Status != corev1.ConditionTrue {
			continue
		}
		if c.Type == batchv1.JobComplete || c.Type == batchv1.JobFailed {
			return true
		}
	}
	return false
}

// hasActiveFleetOperation checks if any OSC dispatcher (rollout, coverage, or
// cleanup) is non-terminal. This includes freshly created, pending, or
// unschedulable Jobs, not only those with active pods.
func (r *KataConfigOpenShiftReconciler) hasActiveFleetOperation() (bool, error) {
	jobList := &batchv1.JobList{}
	if err := r.Client.List(context.TODO(), jobList,
		client.InNamespace(OperatorNamespace),
		client.HasLabels{labelFleetOperation}); err != nil {
		return false, err
	}
	for i := range jobList.Items {
		if !isTerminalJob(&jobList.Items[i]) {
			return true, nil
		}
	}
	return false, nil
}

// createCoverageDispatcher creates an upstream-style coverage dispatcher Job.
// Called by the KataNodeCoverageReconciler when a Node event suggests the
// fleet may need reconciliation. The dispatcher determines which selected
// nodes actually need work via --skip-satisfied-nodes.
func (r *KataConfigOpenShiftReconciler) createCoverageDispatcher(generation string) error {
	nodeSelector, err := r.dispatcherNodeSelector()
	if err != nil {
		return err
	}
	priv := false
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: fmt.Sprintf("kata-cover-%.12s-", generation),
			Namespace:    OperatorNamespace,
			Labels: map[string]string{
				labelFleetOperation: opCoverage,
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            int32Ptr(0),
			TTLSecondsAfterFinished: int32Ptr(600),
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					ServiceAccountName: dispatcherSAName,
					RestartPolicy:      corev1.RestartPolicyNever,
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: boolPtr(true),
						RunAsUser:    int64Ptr(65532),
						SeccompProfile: &corev1.SeccompProfile{
							Type: corev1.SeccompProfileTypeRuntimeDefault,
						},
					},
					Containers: []corev1.Container{{
						Name:  "dispatcher",
						Image: dispatcherImage,
						Args: append([]string{
							"--job-template=/etc/kata-job/install-job.yaml",
							fmt.Sprintf("--name-prefix=kata-%.12s", generation),
							"--parallelism=1",
							fmt.Sprintf("--node-selector=%s", nodeSelector),
							"--node-label=true",
							"--skip-satisfied-nodes",
							"--yield-to-live-run",
							"--owner-job-from-pod=$(POD_NAME)",
							"--wait-node-ready-secs=300",
							fmt.Sprintf("--require-node-handlers=%s", defaultKataDeployRuntime.Handler),
						}, dispatcherNodeWorkFlags()...),
						Env: []corev1.EnvVar{
							{
								Name: "POD_NAMESPACE",
								ValueFrom: &corev1.EnvVarSource{
									FieldRef: &corev1.ObjectFieldSelector{
										FieldPath: "metadata.namespace",
									},
								},
							},
							{
								Name: "POD_NAME",
								ValueFrom: &corev1.EnvVarSource{
									FieldRef: &corev1.ObjectFieldSelector{
										FieldPath: "metadata.name",
									},
								},
							},
						},
						VolumeMounts: []corev1.VolumeMount{{
							Name:      "job-templates",
							MountPath: "/etc/kata-job",
							ReadOnly:  true,
						}},
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: &priv,
							Capabilities: &corev1.Capabilities{
								Drop: []corev1.Capability{"ALL"},
							},
						},
					}},
					Volumes: []corev1.Volume{{
						Name: "job-templates",
						VolumeSource: corev1.VolumeSource{
							ConfigMap: &corev1.ConfigMapVolumeSource{
								LocalObjectReference: corev1.LocalObjectReference{
									Name: templateConfigMapName,
								},
							},
						},
					}},
				},
			},
		},
	}

	if err := controllerutil.SetControllerReference(r.kataConfig, job, r.Scheme); err != nil {
		return err
	}

	r.Log.Info("kata-deploy-dispatcher: creating coverage run",
		"generation", generation[:12])
	return r.Client.Create(context.TODO(), job)
}

func (r *KataConfigOpenShiftReconciler) processKataConfigDeleteRequestDispatcher() (ctrl.Result, error) {
	r.Log.Info("kata-deploy-dispatcher: processing delete request")

	res, err := r.checkDeletionEligibility()
	if err != nil {
		return res, err
	}

	r.setInProgressConditionToUninstalling()

	if err := r.ensureDispatcherTemplateCM(); err != nil {
		return ctrl.Result{Requeue: true, RequeueAfter: 15 * time.Second}, err
	}

	// Use KataConfig UID for stable identity (works even if install never completed)
	kcUID := string(r.kataConfig.UID)
	cleanupJobName := fmt.Sprintf("kata-uninstall-%.12s", kcUID)

	activeJob := &batchv1.Job{}
	err = r.Client.Get(context.TODO(), types.NamespacedName{
		Name:      cleanupJobName,
		Namespace: OperatorNamespace,
	}, activeJob)

	if err == nil {
		for _, c := range activeJob.Status.Conditions {
			if c.Type == batchv1.JobComplete && c.Status == corev1.ConditionTrue {
				r.Log.Info("kata-deploy-dispatcher: uninstall complete")
				return r.finalizeKataDeployDeletion()
			}
			if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
				bg := metav1.DeletePropagationBackground
				_ = r.Client.Delete(context.TODO(), activeJob, &client.DeleteOptions{
					PropagationPolicy: &bg,
				})
				return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
			}
		}
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}
	if !k8serrors.IsNotFound(err) {
		return ctrl.Result{Requeue: true, RequeueAfter: 15 * time.Second}, err
	}

	// Cleanup must not race a running rollout or coverage dispatcher.
	active, err := r.hasActiveFleetOperation()
	if err != nil {
		return ctrl.Result{Requeue: true, RequeueAfter: 15 * time.Second}, err
	}
	if active {
		r.Log.Info("kata-deploy-dispatcher: fleet operation active, deferring cleanup")
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}

	// Upstream uninstall semantics: use cleanup-job.yaml as the primary
	// template, target nodes by the kata-runtime label (exists, any value)
	// rather than the install selector, and tolerate all taints.
	// No OwnerReference: the uninstall Job is part of finalization and must
	// survive independently until cleanup completes; foreground cascading
	// deletion would remove it before the finalizer can observe completion.
	priv := false

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cleanupJobName,
			Namespace: OperatorNamespace,
			Labels: map[string]string{
				labelFleetOperation: opCleanup,
				labelKataConfigUID:  kcUID,
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            int32Ptr(0),
			TTLSecondsAfterFinished: int32Ptr(600),
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					ServiceAccountName: dispatcherSAName,
					RestartPolicy:      corev1.RestartPolicyNever,
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: boolPtr(true),
						RunAsUser:    int64Ptr(65532),
						SeccompProfile: &corev1.SeccompProfile{
							Type: corev1.SeccompProfileTypeRuntimeDefault,
						},
					},
					Containers: []corev1.Container{{
						Name:  "dispatcher",
						Image: dispatcherImage,
						Args: append([]string{
							"--job-template=/etc/kata-job/cleanup-job.yaml",
							fmt.Sprintf("--name-prefix=kata-cleanup-%.8s", kcUID),
							"--owner-job-name=" + cleanupJobName,
							"--parallelism=1",
							"--node-selector=katacontainers.io/kata-runtime",
							"--remove-node-label",
							"--ignore-node-taints",
						}, dispatcherNodeWorkFlags()...),
						Env: []corev1.EnvVar{{
							Name: "POD_NAMESPACE",
							ValueFrom: &corev1.EnvVarSource{
								FieldRef: &corev1.ObjectFieldSelector{
									FieldPath: "metadata.namespace",
								},
							},
						}},
						VolumeMounts: []corev1.VolumeMount{{
							Name:      "job-templates",
							MountPath: "/etc/kata-job",
							ReadOnly:  true,
						}},
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: &priv,
							Capabilities: &corev1.Capabilities{
								Drop: []corev1.Capability{"ALL"},
							},
						},
					}},
					Volumes: []corev1.Volume{{
						Name: "job-templates",
						VolumeSource: corev1.VolumeSource{
							ConfigMap: &corev1.ConfigMapVolumeSource{
								LocalObjectReference: corev1.LocalObjectReference{
									Name: templateConfigMapName,
								},
							},
						},
					}},
				},
			},
		},
	}

	r.Log.Info("kata-deploy-dispatcher: creating uninstall dispatcher", "job", cleanupJobName)
	if err := r.Client.Create(context.TODO(), job); err != nil {
		return ctrl.Result{Requeue: true, RequeueAfter: 15 * time.Second}, err
	}

	return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
}

func (r *KataConfigOpenShiftReconciler) ensureDispatcherSELinuxCM() error {
	cm := &corev1.ConfigMap{}
	err := r.Client.Get(context.TODO(), types.NamespacedName{
		Name:      selinuxCMName,
		Namespace: OperatorNamespace,
	}, cm)

	if err == nil {
		if bytes.Equal(cm.BinaryData["osc_kata_deploy.pp"], dispatcherSELinuxPolicy) {
			return nil
		}
		if cm.BinaryData == nil {
			cm.BinaryData = make(map[string][]byte)
		}
		cm.BinaryData["osc_kata_deploy.pp"] = dispatcherSELinuxPolicy
		return r.Client.Update(context.TODO(), cm)
	}

	if !k8serrors.IsNotFound(err) {
		return err
	}

	cm = &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      selinuxCMName,
			Namespace: OperatorNamespace,
		},
		BinaryData: map[string][]byte{
			"osc_kata_deploy.pp": dispatcherSELinuxPolicy,
		},
	}
	if err := controllerutil.SetControllerReference(r.kataConfig, cm, r.Scheme); err != nil {
		return err
	}
	return r.Client.Create(context.TODO(), cm)
}

func (r *KataConfigOpenShiftReconciler) renderTemplate(tmpl []byte) string {
	combined := r.getKataDeployImage()
	helper := r.getHelperImage()
	rendered := strings.ReplaceAll(string(tmpl), "COMBINED_IMAGE", combined)
	rendered = strings.ReplaceAll(rendered, "HELPER_IMAGE", helper)
	return rendered
}

func (r *KataConfigOpenShiftReconciler) ensureDispatcherTemplateCM() error {
	desired := r.renderTemplate(dispatcherInstallTemplate)
	desiredCleanup := r.renderTemplate(dispatcherCleanupTemplate)

	cm := &corev1.ConfigMap{}
	err := r.Client.Get(context.TODO(), types.NamespacedName{
		Name:      templateConfigMapName,
		Namespace: OperatorNamespace,
	}, cm)

	if err == nil {
		if cm.Data["install-job.yaml"] == desired && cm.Data["cleanup-job.yaml"] == desiredCleanup {
			return nil
		}
		cm.Data["install-job.yaml"] = desired
		cm.Data["cleanup-job.yaml"] = desiredCleanup
		r.Log.Info("kata-deploy-dispatcher: updated job template ConfigMap")
		return r.Client.Update(context.TODO(), cm)
	}

	if !k8serrors.IsNotFound(err) {
		return err
	}

	cm = &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      templateConfigMapName,
			Namespace: OperatorNamespace,
		},
		Data: map[string]string{
			"install-job.yaml": desired,
			"cleanup-job.yaml": desiredCleanup,
		},
	}
	if err := controllerutil.SetControllerReference(r.kataConfig, cm, r.Scheme); err != nil {
		return err
	}
	r.Log.Info("kata-deploy-dispatcher: created job template ConfigMap")
	return r.Client.Create(context.TODO(), cm)
}
