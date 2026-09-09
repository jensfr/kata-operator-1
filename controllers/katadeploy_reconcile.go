package controllers

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	_ "embed"
	"fmt"
	"os"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/yaml"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const (
	kataDeployFinalizer = "kataconfiguration.openshift.io/kata-deploy-finalizer"
	kataRuntimeLabel    = "katacontainers.io/kata-runtime"

	annInstallGeneration = "kata.openshift.io/install-generation"
	annInstallNodeUID    = "kata.openshift.io/install-node-uid"
	annInstallAttemptID  = "kata.openshift.io/install-attempt"
	annCleanupAttemptID  = "kata.openshift.io/cleanup-attempt"
	annManagedBy         = "kata.openshift.io/managed-by"

	labelApp        = "kata.openshift.io/app"
	labelTargetNode = "kata.openshift.io/target-node"
	labelGeneration = "kata.openshift.io/generation"
	labelNodeUID    = "kata.openshift.io/node-uid"

	appInstall = "kata-install"
	appCleanup = "kata-cleanup"

	maxConcurrentNodeMutations = 1

	kataDeployContainerNames = "host-check,install-artifacts,configure-cri"
	helperContainerNames     = "selinux-install,guest-prep,apply-labels"
)

//go:embed manifests/kata-deploy-install-job.yaml
var installJobTemplate []byte

//go:embed manifests/osc_kata_deploy.pp
var selinuxPolicyModule []byte

type nodePhase string

const (
	phaseReady      nodePhase = "Ready"
	phaseCompleted  nodePhase = "Completed"
	phaseInstalling nodePhase = "Installing"
	phaseFailed     nodePhase = "Failed"
	phaseCleaning   nodePhase = "Cleaning"
	phaseNeeded     nodePhase = "Needed"
	phaseCleaned    nodePhase = "Cleaned"
)

type InstallRevision struct {
	KataDeployImage     string
	HelperImage         string
	SELinuxPolicyHash   string
	InstallTemplateHash string
}

func (rev InstallRevision) Hash() string {
	h := sha256.New()
	fmt.Fprintf(h, "kata-deploy:%s\n", rev.KataDeployImage)
	fmt.Fprintf(h, "helper:%s\n", rev.HelperImage)
	fmt.Fprintf(h, "selinux:%s\n", rev.SELinuxPolicyHash)
	fmt.Fprintf(h, "template:%s\n", rev.InstallTemplateHash)
	return fmt.Sprintf("%x", h.Sum(nil))
}

func (r *KataConfigOpenShiftReconciler) installRevision() InstallRevision {
	templateHash := sha256.Sum256(installJobTemplate)
	policyHash := sha256.Sum256(selinuxPolicyModule)
	return InstallRevision{
		KataDeployImage:     r.getKataDeployImage(),
		HelperImage:         r.getHelperImage(),
		SELinuxPolicyHash:   fmt.Sprintf("%x", policyHash[:8]),
		InstallTemplateHash: fmt.Sprintf("%x", templateHash[:8]),
	}
}

func (r *KataConfigOpenShiftReconciler) desiredGeneration() string {
	return r.installRevision().Hash()
}

func (r *KataConfigOpenShiftReconciler) getKataDeployImage() string {
	return os.Getenv("RELATED_IMAGE_KATA_DEPLOY")
}

func (r *KataConfigOpenShiftReconciler) getHelperImage() string {
	return os.Getenv("RELATED_IMAGE_OSC_HELPER")
}

func generateAttemptID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate attempt ID: %w", err)
	}
	return fmt.Sprintf("%x", b), nil
}

func (r *KataConfigOpenShiftReconciler) selectedNodes() (*corev1.NodeList, error) {
	nodeList := &corev1.NodeList{}
	opts := []client.ListOption{
		client.MatchingLabels{"node-role.kubernetes.io/worker": ""},
	}
	if r.kataConfig.Spec.KataConfigPoolSelector != nil {
		selector, err := metav1.LabelSelectorAsSelector(r.kataConfig.Spec.KataConfigPoolSelector)
		if err != nil {
			return nil, err
		}
		opts = []client.ListOption{
			client.MatchingLabelsSelector{Selector: selector},
		}
	}
	if err := r.Client.List(context.TODO(), nodeList, opts...); err != nil {
		return nil, err
	}
	return nodeList, nil
}

func (r *KataConfigOpenShiftReconciler) managedNodes() (*corev1.NodeList, error) {
	nodeList := &corev1.NodeList{}
	if err := r.Client.List(context.TODO(), nodeList); err != nil {
		return nil, err
	}
	kcUID := string(r.kataConfig.UID)
	managed := &corev1.NodeList{}
	for i := range nodeList.Items {
		if nodeList.Items[i].Annotations[annManagedBy] == kcUID {
			managed.Items = append(managed.Items, nodeList.Items[i])
		}
	}
	return managed, nil
}

func (r *KataConfigOpenShiftReconciler) countActiveMutations() (int, error) {
	jobList := &batchv1.JobList{}
	if err := r.Client.List(context.TODO(), jobList,
		client.InNamespace(OperatorNamespace),
		client.HasLabels{labelApp}); err != nil {
		return 0, fmt.Errorf("list mutation Jobs: %w", err)
	}
	count := 0
	for i := range jobList.Items {
		job := &jobList.Items[i]
		app := job.Labels[labelApp]
		if app != appInstall && app != appCleanup {
			continue
		}
		if isJobActive(job) {
			count++
		}
	}
	return count, nil
}

func isJobActive(job *batchv1.Job) bool {
	if job.Status.Active > 0 {
		return true
	}
	for _, c := range job.Status.Conditions {
		if (c.Type == batchv1.JobComplete || c.Type == batchv1.JobFailed) &&
			c.Status == corev1.ConditionTrue {
			return false
		}
	}
	// No conditions and no active pods: just created, treat as active.
	return true
}

func (r *KataConfigOpenShiftReconciler) processKataConfigInstallRequestKataDeploy() (ctrl.Result, error) {
	r.Log.Info("kata-deploy: reconciling install")

	if err := r.ensureKataInstallSCC(); err != nil {
		return ctrl.Result{Requeue: true, RequeueAfter: 15 * time.Second}, err
	}

	if !controllerutil.ContainsFinalizer(r.kataConfig, kataDeployFinalizer) {
		controllerutil.AddFinalizer(r.kataConfig, kataDeployFinalizer)
		if err := r.Client.Update(context.TODO(), r.kataConfig); err != nil {
			return ctrl.Result{}, err
		}
	}

	if err := r.ensureSELinuxConfigMap(); err != nil {
		return ctrl.Result{Requeue: true, RequeueAfter: 15 * time.Second}, err
	}

	desired := r.desiredGeneration()

	selected, err := r.selectedNodes()
	if err != nil {
		return ctrl.Result{Requeue: true, RequeueAfter: 15 * time.Second}, err
	}
	selectedSet := map[string]bool{}
	for i := range selected.Items {
		selectedSet[selected.Items[i].Name] = true
	}

	managed, err := r.managedNodes()
	if err != nil {
		return ctrl.Result{Requeue: true, RequeueAfter: 15 * time.Second}, err
	}

	activeMutations, err := r.countActiveMutations()
	if err != nil {
		return ctrl.Result{Requeue: true, RequeueAfter: 15 * time.Second}, err
	}

	var installing, installed, failed, pending []string
	var uninstalling, waitingToUninstall, failedToUninstall []string
	scheduledMutation := false

	// First pass: commit completed work and count active mutations.
	for i := range selected.Items {
		node := &selected.Items[i]

		// Node with pending cleanup must complete cleanup first.
		if node.Annotations[annCleanupAttemptID] != "" {
			waitingToUninstall = append(waitingToUninstall, node.Name)
			continue
		}

		phase := r.nodePhase(node, desired)

		switch phase {
		case phaseReady:
			installed = append(installed, node.Name)
		case phaseCompleted:
			if err := r.commitInstallSuccess(node, desired); err != nil {
				r.Log.Error(err, "kata-deploy: commit failed", "node", node.Name)
				failed = append(failed, node.Name)
			} else {
				installed = append(installed, node.Name)
			}
		case phaseInstalling:
			installing = append(installing, node.Name)
		case phaseFailed:
			failed = append(failed, node.Name)
		case phaseNeeded:
			pending = append(pending, node.Name)
		}
	}

	// Second pass: schedule at most one new mutation from failed/pending.
	if !scheduledMutation && activeMutations < maxConcurrentNodeMutations {
		// Retry failed nodes first (delete failed Job, next reconcile creates fresh).
		for _, name := range failed {
			if scheduledMutation {
				break
			}
			node := r.findNode(selected, name)
			if node == nil {
				continue
			}
			if err := r.deleteFailedInstallJob(node, desired); err != nil {
				r.Log.Error(err, "kata-deploy: failed Job cleanup", "node", name)
				continue
			}
			scheduledMutation = true
		}

		if !scheduledMutation {
			newPending := make([]string, 0, len(pending))
			for _, name := range pending {
				if scheduledMutation {
					newPending = append(newPending, name)
					continue
				}
				node := r.findNode(selected, name)
				if node == nil {
					continue
				}
				if err := r.markManaged(node); err != nil {
					r.Log.Error(err, "kata-deploy: managed annotation failed", "node", name)
					newPending = append(newPending, name)
					continue
				}
				if err := r.startInstall(node, desired); err != nil {
					r.Log.Error(err, "kata-deploy: install failed", "node", name)
					newPending = append(newPending, name)
					continue
				}
				installing = append(installing, name)
				scheduledMutation = true
			}
			pending = newPending
		}
	}

	// Cleanup managed nodes that have a cleanup intent or left the selected set.
	for i := range managed.Items {
		node := &managed.Items[i]
		hasCleanupIntent := node.Annotations[annCleanupAttemptID] != ""

		// A node with cleanup intent must complete cleanup even if reselected.
		if selectedSet[node.Name] && !hasCleanupIntent {
			continue
		}

		// First time seeing deselection: withdraw scheduling and record intent.
		if !hasCleanupIntent {
			if err := r.withdrawReadinessAndRecordCleanupIntent(node); err != nil {
				r.Log.Error(err, "kata-deploy: failed to initiate cleanup", "node", node.Name)
				waitingToUninstall = append(waitingToUninstall, node.Name)
				continue
			}
		}

		cPhase := r.cleanupPhase(node)
		switch cPhase {
		case phaseCleaned:
			if err := r.commitCleanupSuccess(node); err != nil {
				r.Log.Error(err, "kata-deploy: cleanup commit failed", "node", node.Name)
				uninstalling = append(uninstalling, node.Name)
			}
		case phaseCleaning:
			uninstalling = append(uninstalling, node.Name)
		case phaseFailed:
			if !scheduledMutation && activeMutations < maxConcurrentNodeMutations {
				r.deleteFailedCleanupJob(node)
				scheduledMutation = true
			}
			failedToUninstall = append(failedToUninstall, node.Name)
		case phaseNeeded:
			if !scheduledMutation && activeMutations < maxConcurrentNodeMutations {
				if err := r.startCleanup(node); err != nil {
					r.Log.Error(err, "kata-deploy: cleanup start failed", "node", node.Name)
					waitingToUninstall = append(waitingToUninstall, node.Name)
				} else {
					uninstalling = append(uninstalling, node.Name)
					scheduledMutation = true
				}
			} else {
				waitingToUninstall = append(waitingToUninstall, node.Name)
			}
		}
	}

	r.kataConfig.Status.KataNodes.Installed = installed
	r.kataConfig.Status.KataNodes.Installing = installing
	r.kataConfig.Status.KataNodes.WaitingToInstall = pending
	r.kataConfig.Status.KataNodes.FailedToInstall = failed
	r.kataConfig.Status.KataNodes.Uninstalling = uninstalling
	r.kataConfig.Status.KataNodes.WaitingToUninstall = waitingToUninstall
	r.kataConfig.Status.KataNodes.FailedToUninstall = failedToUninstall
	r.kataConfig.Status.KataNodes.ReadyNodeCount = len(installed)
	r.kataConfig.Status.KataNodes.NodeCount = len(selected.Items)

	if err := r.Client.Status().Update(context.TODO(), r.kataConfig); err != nil {
		r.Log.Error(err, "kata-deploy: failed to update status")
	}

	needsRequeue := len(installing) > 0 || len(pending) > 0 || len(failed) > 0 ||
		len(uninstalling) > 0 || len(waitingToUninstall) > 0 || len(failedToUninstall) > 0
	if needsRequeue {
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	if len(installed) > 0 {
		r.Log.Info("kata-deploy: all nodes ready")
		res, err := r.postKataInstallation()
		if res != nil {
			return *res, err
		}
	}

	return ctrl.Result{}, nil
}

func (r *KataConfigOpenShiftReconciler) findNode(list *corev1.NodeList, name string) *corev1.Node {
	for i := range list.Items {
		if list.Items[i].Name == name {
			return &list.Items[i]
		}
	}
	return nil
}

// nodePhase is side-effect-free.
func (r *KataConfigOpenShiftReconciler) nodePhase(node *corev1.Node, desired string) nodePhase {
	observed := node.Annotations[annInstallGeneration]
	storedUID := node.Annotations[annInstallNodeUID]
	currentUID := string(node.UID)

	if observed == desired && storedUID == currentUID {
		return phaseReady
	}

	attemptID := node.Annotations[annInstallAttemptID]
	if attemptID == "" {
		return phaseNeeded
	}

	expectedJobName := installJobName(node.Name, desired, currentUID, attemptID)

	job := &batchv1.Job{}
	err := r.Client.Get(context.TODO(), types.NamespacedName{
		Name:      expectedJobName,
		Namespace: OperatorNamespace,
	}, job)

	if err != nil {
		if k8serrors.IsNotFound(err) {
			return phaseNeeded
		}
		return phaseFailed
	}

	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobComplete && c.Status == corev1.ConditionTrue {
			return phaseCompleted
		}
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
			return phaseFailed
		}
	}

	return phaseInstalling
}

// commitInstallSuccess records observed generation, node UID, and kata label.
// Clears the attempt ID since this lifecycle is complete.
func (r *KataConfigOpenShiftReconciler) commitInstallSuccess(node *corev1.Node, generation string) error {
	if node.Annotations == nil {
		node.Annotations = make(map[string]string)
	}
	node.Annotations[annInstallGeneration] = generation
	node.Annotations[annInstallNodeUID] = string(node.UID)
	delete(node.Annotations, annInstallAttemptID)

	if node.Labels == nil {
		node.Labels = make(map[string]string)
	}
	node.Labels[kataRuntimeLabel] = "true"

	return r.Client.Update(context.TODO(), node)
}

func (r *KataConfigOpenShiftReconciler) markManaged(node *corev1.Node) error {
	kcUID := string(r.kataConfig.UID)
	if node.Annotations == nil {
		node.Annotations = make(map[string]string)
	}
	if node.Annotations[annManagedBy] == kcUID {
		return nil
	}
	node.Annotations[annManagedBy] = kcUID
	return r.Client.Update(context.TODO(), node)
}

const selinuxConfigMapName = "selinux-module"

func (r *KataConfigOpenShiftReconciler) ensureSELinuxConfigMap() error {
	cm := &corev1.ConfigMap{}
	err := r.Client.Get(context.TODO(), types.NamespacedName{
		Name:      selinuxConfigMapName,
		Namespace: OperatorNamespace,
	}, cm)

	if err == nil {
		if bytes.Equal(cm.BinaryData["osc_kata_deploy.pp"], selinuxPolicyModule) {
			return nil
		}
		// Contents differ or key missing: update.
		if cm.BinaryData == nil {
			cm.BinaryData = make(map[string][]byte)
		}
		cm.BinaryData["osc_kata_deploy.pp"] = selinuxPolicyModule
		if err := controllerutil.SetControllerReference(r.kataConfig, cm, r.Scheme); err != nil {
			return err
		}
		r.Log.Info("kata-deploy: updating SELinux policy ConfigMap")
		return r.Client.Update(context.TODO(), cm)
	}

	if !k8serrors.IsNotFound(err) {
		return err
	}

	cm = &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      selinuxConfigMapName,
			Namespace: OperatorNamespace,
		},
		BinaryData: map[string][]byte{
			"osc_kata_deploy.pp": selinuxPolicyModule,
		},
	}

	if err := controllerutil.SetControllerReference(r.kataConfig, cm, r.Scheme); err != nil {
		return err
	}

	r.Log.Info("kata-deploy: creating SELinux policy ConfigMap")
	return r.Client.Create(context.TODO(), cm)
}

// startInstall generates a fresh attempt ID, writes it to the node, and
// creates the install Job. The attempt ID ensures that a future lifecycle
// (after cleanup and reselection) always gets a distinct Job name.
func (r *KataConfigOpenShiftReconciler) startInstall(node *corev1.Node, generation string) error {
	kataDeployImage := r.getKataDeployImage()
	if kataDeployImage == "" {
		return fmt.Errorf("RELATED_IMAGE_KATA_DEPLOY not set")
	}
	helperImage := r.getHelperImage()
	if helperImage == "" {
		return fmt.Errorf("RELATED_IMAGE_OSC_HELPER not set")
	}

	attemptID, err := generateAttemptID()
	if err != nil {
		return err
	}
	if node.Annotations == nil {
		node.Annotations = make(map[string]string)
	}
	node.Annotations[annInstallAttemptID] = attemptID
	if err := r.Client.Update(context.TODO(), node); err != nil {
		return fmt.Errorf("write attempt ID: %w", err)
	}

	nodeUID := string(node.UID)
	jobName := installJobName(node.Name, generation, nodeUID, attemptID)
	criVersion := node.Status.NodeInfo.ContainerRuntimeVersion

	job, err := r.renderInstallJob(node.Name, nodeUID, criVersion, generation, kataDeployImage, helperImage, attemptID)
	if err != nil {
		return err
	}

	if err := controllerutil.SetControllerReference(r.kataConfig, job, r.Scheme); err != nil {
		return err
	}

	r.Log.Info("kata-deploy: creating install Job",
		"job", jobName, "node", node.Name,
		"generation", truncate(generation, 12), "attempt", attemptID)
	return r.Client.Create(context.TODO(), job)
}

func (r *KataConfigOpenShiftReconciler) deleteFailedInstallJob(node *corev1.Node, desired string) error {
	attemptID := node.Annotations[annInstallAttemptID]
	if attemptID == "" {
		return nil
	}
	jobName := installJobName(node.Name, desired, string(node.UID), attemptID)

	// Clear attempt ID so next reconcile creates a fresh one.
	delete(node.Annotations, annInstallAttemptID)
	if err := r.Client.Update(context.TODO(), node); err != nil {
		return err
	}

	return r.deleteJobByName(jobName)
}

func (r *KataConfigOpenShiftReconciler) renderInstallJob(nodeName, nodeUID, criVersion, generation, kataDeployImage, helperImage, attemptID string) (*batchv1.Job, error) {
	job := &batchv1.Job{}
	if err := yaml.NewYAMLOrJSONDecoder(
		strings.NewReader(string(installJobTemplate)), len(installJobTemplate),
	).Decode(job); err != nil {
		return nil, fmt.Errorf("failed to decode install Job template: %w", err)
	}

	jobName := installJobName(nodeName, generation, nodeUID, attemptID)

	job.Name = jobName
	job.Namespace = OperatorNamespace

	if job.Labels == nil {
		job.Labels = make(map[string]string)
	}
	delete(job.Labels, "kata.io/target-node")
	job.Labels[labelApp] = appInstall
	job.Labels[labelTargetNode] = nodeName
	job.Labels[labelGeneration] = truncate(generation, 12)
	job.Labels[labelNodeUID] = shortUID(nodeUID)

	job.Spec.Template.Spec.NodeName = nodeName
	if job.Spec.Template.Labels == nil {
		job.Spec.Template.Labels = make(map[string]string)
	}
	delete(job.Spec.Template.Labels, "kata.io/target-node")
	job.Spec.Template.Labels[labelApp] = appInstall
	job.Spec.Template.Labels[labelTargetNode] = nodeName

	kataDeployNames := toSet(kataDeployContainerNames)
	helperNames := toSet(helperContainerNames)
	setContainerImages(job.Spec.Template.Spec.InitContainers, kataDeployNames, kataDeployImage)
	setContainerImages(job.Spec.Template.Spec.InitContainers, helperNames, helperImage)
	setContainerImages(job.Spec.Template.Spec.Containers, kataDeployNames, kataDeployImage)
	setContainerImages(job.Spec.Template.Spec.Containers, helperNames, helperImage)

	setContainerEnv(job.Spec.Template.Spec.InitContainers, "NODE_NAME", nodeName)
	setContainerEnv(job.Spec.Template.Spec.InitContainers, "CONTAINER_RUNTIME_VERSION", criVersion)
	setContainerEnv(job.Spec.Template.Spec.InitContainers, "SHIMS_X86_64", defaultKataDeployRuntime.Shim)
	setContainerEnv(job.Spec.Template.Spec.InitContainers, "DEFAULT_SHIM_X86_64", defaultKataDeployRuntime.Shim)
	setContainerEnv(job.Spec.Template.Spec.Containers, "NODE_NAME", nodeName)
	setContainerEnv(job.Spec.Template.Spec.Containers, "CONTAINER_RUNTIME_VERSION", criVersion)
	setContainerEnv(job.Spec.Template.Spec.Containers, "SHIMS_X86_64", defaultKataDeployRuntime.Shim)
	setContainerEnv(job.Spec.Template.Spec.Containers, "DEFAULT_SHIM_X86_64", defaultKataDeployRuntime.Shim)

	return job, nil
}

func setContainerImages(containers []corev1.Container, names map[string]bool, image string) {
	for i := range containers {
		if names[containers[i].Name] {
			containers[i].Image = image
		}
	}
}

func setContainerEnv(containers []corev1.Container, name, value string) {
	for i := range containers {
		for j := range containers[i].Env {
			if containers[i].Env[j].Name == name {
				containers[i].Env[j].Value = value
				containers[i].Env[j].ValueFrom = nil
			}
		}
	}
}

func toSet(csv string) map[string]bool {
	m := map[string]bool{}
	for _, s := range strings.Split(csv, ",") {
		m[s] = true
	}
	return m
}

func installJobName(nodeName, generation, nodeUID, attemptID string) string {
	h := sha256.Sum256([]byte(nodeName + ":" + generation + ":" + nodeUID + ":" + attemptID))
	short := fmt.Sprintf("%x", h[:6])
	return fmt.Sprintf("kata-install-%s", short)
}

func shortUID(uid string) string {
	return truncate(uid, 8)
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func (r *KataConfigOpenShiftReconciler) deleteJobByName(name string) error {
	job := &batchv1.Job{}
	if err := r.Client.Get(context.TODO(), types.NamespacedName{
		Name: name, Namespace: OperatorNamespace,
	}, job); err != nil {
		if k8serrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	bg := metav1.DeletePropagationBackground
	return r.Client.Delete(context.TODO(), job, &client.DeleteOptions{
		PropagationPolicy: &bg,
	})
}

// --- Delete / Cleanup ---

func (r *KataConfigOpenShiftReconciler) processKataConfigDeleteRequestKataDeploy() (ctrl.Result, error) {
	r.Log.Info("kata-deploy: processing delete request")

	res, err := r.checkDeletionEligibility()
	if err != nil {
		return res, err
	}

	r.setInProgressConditionToUninstalling()

	nodes, err := r.managedNodes()
	if err != nil {
		return ctrl.Result{Requeue: true, RequeueAfter: 15 * time.Second}, err
	}

	if len(nodes.Items) == 0 {
		if err := r.deleteAllKataDeployJobs(); err != nil {
			r.Log.Error(err, "kata-deploy: failed to delete remaining Jobs")
		}
		return r.finalizeKataDeployDeletion()
	}

	activeMutations, err := r.countActiveMutations()
	if err != nil {
		return ctrl.Result{Requeue: true, RequeueAfter: 15 * time.Second}, err
	}
	allDone := true
	scheduledMutation := false

	for i := range nodes.Items {
		node := &nodes.Items[i]
		phase := r.cleanupPhase(node)

		switch phase {
		case phaseCleaned:
			if err := r.commitCleanupSuccess(node); err != nil {
				r.Log.Error(err, "kata-deploy: cleanup commit failed", "node", node.Name)
				allDone = false
			}
		case phaseCleaning:
			allDone = false
		case phaseFailed:
			if !scheduledMutation && activeMutations < maxConcurrentNodeMutations {
				r.deleteFailedCleanupJob(node)
				scheduledMutation = true
			}
			allDone = false
		case phaseNeeded:
			if !scheduledMutation && activeMutations < maxConcurrentNodeMutations {
				if err := r.startCleanup(node); err != nil {
					r.Log.Error(err, "kata-deploy: cleanup start failed", "node", node.Name)
				} else {
					scheduledMutation = true
				}
			}
			allDone = false
		}
	}

	if !allDone {
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}

	if err := r.deleteAllKataDeployJobs(); err != nil {
		r.Log.Error(err, "kata-deploy: failed to delete remaining Jobs")
	}

	return r.finalizeKataDeployDeletion()
}

// cleanupPhase is side-effect-free.
func (r *KataConfigOpenShiftReconciler) cleanupPhase(node *corev1.Node) nodePhase {
	attemptID := node.Annotations[annCleanupAttemptID]
	if attemptID == "" {
		return phaseNeeded
	}

	expectedJobName := cleanupJobName(node.Name, string(node.UID), attemptID)

	job := &batchv1.Job{}
	err := r.Client.Get(context.TODO(), types.NamespacedName{
		Name:      expectedJobName,
		Namespace: OperatorNamespace,
	}, job)

	if err != nil {
		if k8serrors.IsNotFound(err) {
			return phaseNeeded
		}
		return phaseFailed
	}

	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobComplete && c.Status == corev1.ConditionTrue {
			return phaseCleaned
		}
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
			return phaseFailed
		}
	}

	return phaseCleaning
}

// commitCleanupSuccess removes all install/ownership state from the node.
func (r *KataConfigOpenShiftReconciler) commitCleanupSuccess(node *corev1.Node) error {
	delete(node.Annotations, annInstallGeneration)
	delete(node.Annotations, annInstallNodeUID)
	delete(node.Annotations, annInstallAttemptID)
	delete(node.Annotations, annCleanupAttemptID)
	delete(node.Annotations, annManagedBy)
	delete(node.Labels, kataRuntimeLabel)

	return r.Client.Update(context.TODO(), node)
}

// withdrawReadinessAndRecordCleanupIntent atomically removes the
// kata-runtime label and records a cleanup attempt ID on the node.
// This ensures the scheduler stops sending kata pods before any
// destructive cleanup begins.
func (r *KataConfigOpenShiftReconciler) withdrawReadinessAndRecordCleanupIntent(node *corev1.Node) error {
	attemptID, err := generateAttemptID()
	if err != nil {
		return err
	}
	if node.Annotations == nil {
		node.Annotations = make(map[string]string)
	}
	node.Annotations[annCleanupAttemptID] = attemptID
	delete(node.Labels, kataRuntimeLabel)

	if err := r.Client.Update(context.TODO(), node); err != nil {
		return fmt.Errorf("withdraw readiness: %w", err)
	}
	r.Log.Info("kata-deploy: withdrew readiness and recorded cleanup intent",
		"node", node.Name, "attempt", truncate(attemptID, 8))
	return nil
}

func (r *KataConfigOpenShiftReconciler) startCleanup(node *corev1.Node) error {
	kataDeployImage := r.getKataDeployImage()
	if kataDeployImage == "" {
		return fmt.Errorf("RELATED_IMAGE_KATA_DEPLOY not set")
	}
	helperImage := r.getHelperImage()
	if helperImage == "" {
		return fmt.Errorf("RELATED_IMAGE_OSC_HELPER not set")
	}

	// Reuse the cleanup attempt ID from withdrawReadinessAndRecordCleanupIntent.
	attemptID := node.Annotations[annCleanupAttemptID]
	if attemptID == "" {
		return fmt.Errorf("cleanup attempt ID missing on node %s", node.Name)
	}

	nodeUID := string(node.UID)
	job := r.renderCleanupJob(node.Name, nodeUID, kataDeployImage, helperImage, attemptID)

	if err := controllerutil.SetControllerReference(r.kataConfig, job, r.Scheme); err != nil {
		return err
	}

	r.Log.Info("kata-deploy: creating cleanup Job",
		"job", job.Name, "node", node.Name, "attempt", attemptID)
	return r.Client.Create(context.TODO(), job)
}

func (r *KataConfigOpenShiftReconciler) deleteFailedCleanupJob(node *corev1.Node) {
	attemptID := node.Annotations[annCleanupAttemptID]
	if attemptID == "" {
		return
	}
	jobName := cleanupJobName(node.Name, string(node.UID), attemptID)

	delete(node.Annotations, annCleanupAttemptID)
	_ = r.Client.Update(context.TODO(), node)

	_ = r.deleteJobByName(jobName)
}

func (r *KataConfigOpenShiftReconciler) renderCleanupJob(nodeName, nodeUID, kataDeployImage, helperImage, attemptID string) *batchv1.Job {
	jobName := cleanupJobName(nodeName, nodeUID, attemptID)
	priv := true

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: OperatorNamespace,
			Labels: map[string]string{
				labelApp:        appCleanup,
				labelTargetNode: nodeName,
				labelNodeUID:    shortUID(nodeUID),
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: int32Ptr(3),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						labelApp:        appCleanup,
						labelTargetNode: nodeName,
					},
				},
				Spec: corev1.PodSpec{
					NodeName:                      nodeName,
					RestartPolicy:                 corev1.RestartPolicyNever,
					ServiceAccountName:            "kata-install",
					AutomountServiceAccountToken:  boolPtr(false),
					TerminationGracePeriodSeconds: int64Ptr(60),
					InitContainers: []corev1.Container{
						{
							Name:    "cleanup-cri",
							Image:   kataDeployImage,
							Command: []string{"/usr/bin/kata-deploy", "cleanup-stage-revert-cri"},
							Env: []corev1.EnvVar{
								{Name: "NODE_NAME", Value: nodeName},
								{Name: "SHIMS_X86_64", Value: defaultKataDeployRuntime.Shim},
								{Name: "DEFAULT_SHIM_X86_64", Value: defaultKataDeployRuntime.Shim},
								{Name: "K8S_DISTRIBUTION", Value: "k8s"},
							},
							SecurityContext: &corev1.SecurityContext{Privileged: &priv},
							VolumeMounts:    cleanupVolumeMounts(),
						},
						{
							Name:  "cleanup-osc",
							Image: helperImage,
							Command: []string{"/bin/sh", "-c", `set -eu
chroot /host semodule -r osc_kata_deploy 2>/dev/null || true
rm -f /host/etc/systemd/system/osc-kata-guest-prep.service
rm -f /host/etc/systemd/system/kubelet.service.wants/osc-kata-guest-prep.service
rm -rf /host/var/cache/kata-containers/osbuilder-images
echo "OSC host state cleaned"`},
							SecurityContext: &corev1.SecurityContext{Privileged: &priv},
							VolumeMounts: []corev1.VolumeMount{
								{Name: "host-root", MountPath: "/host"},
							},
						},
					},
					Containers: []corev1.Container{
						{
							Name:    "cleanup-artifacts",
							Image:   kataDeployImage,
							Command: []string{"/usr/bin/kata-deploy", "cleanup-stage-remove-artifacts"},
							Env: []corev1.EnvVar{
								{Name: "NODE_NAME", Value: nodeName},
								{Name: "SHIMS_X86_64", Value: defaultKataDeployRuntime.Shim},
								{Name: "DEFAULT_SHIM_X86_64", Value: defaultKataDeployRuntime.Shim},
								{Name: "K8S_DISTRIBUTION", Value: "k8s"},
							},
							SecurityContext: &corev1.SecurityContext{Privileged: &priv},
							VolumeMounts:    cleanupVolumeMounts(),
						},
					},
					Volumes: cleanupVolumes(),
				},
			},
		},
	}
}

func cleanupJobName(nodeName, nodeUID, attemptID string) string {
	h := sha256.Sum256([]byte(nodeName + ":cleanup:" + nodeUID + ":" + attemptID))
	short := fmt.Sprintf("%x", h[:6])
	return fmt.Sprintf("kata-cleanup-%s", short)
}

func cleanupVolumeMounts() []corev1.VolumeMount {
	return []corev1.VolumeMount{
		{Name: "kata-install", MountPath: "/opt/kata"},
		{Name: "crio-conf", MountPath: "/etc/crio/"},
		{Name: "containerd-conf", MountPath: "/etc/containerd/"},
		{Name: "systemd-system", MountPath: "/etc/systemd/system"},
		{Name: "systemd-private", MountPath: "/run/systemd/private"},
		{Name: "host-run-lock", MountPath: "/host-run-lock"},
		{Name: "host-machine-id", MountPath: "/host-machine-id", ReadOnly: true},
		{Name: "host-usr-bin", MountPath: "/host-usr/bin", ReadOnly: true},
		{Name: "host-usr-sbin", MountPath: "/host-usr/sbin", ReadOnly: true},
	}
}

func cleanupVolumes() []corev1.Volume {
	dirOrCreate := corev1.HostPathDirectoryOrCreate
	socket := corev1.HostPathSocket
	file := corev1.HostPathFile
	return []corev1.Volume{
		{Name: "host-root", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/"}}},
		{Name: "kata-install", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/opt/kata", Type: &dirOrCreate}}},
		{Name: "crio-conf", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/etc/crio/"}}},
		{Name: "containerd-conf", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/etc/containerd/"}}},
		{Name: "systemd-system", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/etc/systemd/system", Type: &dirOrCreate}}},
		{Name: "systemd-private", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/run/systemd/private", Type: &socket}}},
		{Name: "host-run-lock", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/run/lock", Type: &dirOrCreate}}},
		{Name: "host-machine-id", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/etc/machine-id", Type: &file}}},
		{Name: "host-usr-bin", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/usr/bin"}}},
		{Name: "host-usr-sbin", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/usr/sbin"}}},
	}
}

func (r *KataConfigOpenShiftReconciler) deleteAllKataDeployJobs() error {
	jobList := &batchv1.JobList{}
	if err := r.Client.List(context.TODO(), jobList,
		client.InNamespace(OperatorNamespace),
		client.HasLabels{labelApp}); err != nil {
		return err
	}

	bg := metav1.DeletePropagationBackground
	for i := range jobList.Items {
		job := &jobList.Items[i]
		app := job.Labels[labelApp]
		if app == appInstall || app == appCleanup {
			if err := r.Client.Delete(context.TODO(), job, &client.DeleteOptions{
				PropagationPolicy: &bg,
			}); err != nil && !k8serrors.IsNotFound(err) {
				return err
			}
		}
	}
	return nil
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
