package controllers

import (
	"context"
	"encoding/json"
	"maps"
	"os"
	"strings"
	"testing"

	secv1 "github.com/openshift/api/security/v1"
	nodeapi "k8s.io/api/node/v1"
	"k8s.io/apimachinery/pkg/util/validation"

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

func TestInstallRevision(t *testing.T) {
	t.Run("deterministic from inputs", func(t *testing.T) {
		t.Setenv("RELATED_IMAGE_KATA_DEPLOY", "quay.io/test/kata-deploy@sha256:abc")
		t.Setenv("RELATED_IMAGE_OSC_HELPER", "quay.io/test/helper@sha256:def")
		t.Setenv("OSC_SELINUX_POLICY_HASH", "policy1")
		r := newTestReconciler()

		g1 := r.desiredGeneration()
		g2 := r.desiredGeneration()
		if g1 != g2 {
			t.Errorf("expected stable generation, got %q and %q", g1, g2)
		}
	})

	t.Run("changes when kata-deploy image changes", func(t *testing.T) {
		t.Setenv("RELATED_IMAGE_KATA_DEPLOY", "quay.io/test/kata-deploy:v1")
		t.Setenv("RELATED_IMAGE_OSC_HELPER", "quay.io/test/helper:v1")
		r := newTestReconciler()
		g1 := r.desiredGeneration()

		os.Setenv("RELATED_IMAGE_KATA_DEPLOY", "quay.io/test/kata-deploy:v2")
		g2 := r.desiredGeneration()

		if g1 == g2 {
			t.Error("expected different generation for different kata-deploy image")
		}
	})

	t.Run("changes when helper image changes", func(t *testing.T) {
		t.Setenv("RELATED_IMAGE_KATA_DEPLOY", "quay.io/test/kata-deploy:v1")
		t.Setenv("RELATED_IMAGE_OSC_HELPER", "quay.io/test/helper:v1")
		r := newTestReconciler()
		g1 := r.desiredGeneration()

		os.Setenv("RELATED_IMAGE_OSC_HELPER", "quay.io/test/helper:v2")
		g2 := r.desiredGeneration()

		if g1 == g2 {
			t.Error("expected different generation for different helper image")
		}
	})

	t.Run("SELinux policy hash is derived from embedded bytes", func(t *testing.T) {
		t.Setenv("RELATED_IMAGE_KATA_DEPLOY", "quay.io/test/kata-deploy:v1")
		t.Setenv("RELATED_IMAGE_OSC_HELPER", "quay.io/test/helper:v1")
		r := newTestReconciler()
		rev := r.installRevision()
		if rev.SELinuxPolicyHash == "" {
			t.Error("expected non-empty SELinux policy hash from embedded .pp")
		}
	})
}

func TestNodePhase(t *testing.T) {
	t.Parallel()

	t.Run("new node needs install", func(t *testing.T) {
		t.Parallel()
		node := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "worker-1",
				UID:  types.UID("uid-1"),
			},
		}
		r := newTestReconciler(node)
		if r.nodePhase(node, "gen1") != phaseNeeded {
			t.Error("expected Needed")
		}
	})

	t.Run("installed node is Ready", func(t *testing.T) {
		t.Parallel()
		node := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "worker-1",
				UID:  types.UID("uid-1"),
				Annotations: map[string]string{
					annInstallGeneration: "gen1",
					annInstallNodeUID:    "uid-1",
				},
			},
		}
		r := newTestReconciler(node)
		if r.nodePhase(node, "gen1") != phaseReady {
			t.Error("expected Ready")
		}
	})

	t.Run("generation mismatch with no attempt ID needs install", func(t *testing.T) {
		t.Parallel()
		node := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "worker-1",
				UID:  types.UID("uid-1"),
				Annotations: map[string]string{
					annInstallGeneration: "old-gen",
					annInstallNodeUID:    "uid-1",
				},
			},
		}
		r := newTestReconciler(node)
		if r.nodePhase(node, "new-gen") != phaseNeeded {
			t.Error("expected Needed")
		}
	})

	t.Run("active Job means Installing", func(t *testing.T) {
		t.Parallel()
		attemptID := "att1"
		jobName := installJobName("worker-1", "gen1", "uid-1", attemptID)
		job := &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{Name: jobName, Namespace: OperatorNamespace},
			Status:     batchv1.JobStatus{Active: 1},
		}
		node := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "worker-1",
				UID:  types.UID("uid-1"),
				Annotations: map[string]string{
					annInstallAttemptID: attemptID,
				},
			},
		}
		r := newTestReconciler(node, job)
		if r.nodePhase(node, "gen1") != phaseInstalling {
			t.Error("expected Installing")
		}
	})

	t.Run("failed Job means Failed", func(t *testing.T) {
		t.Parallel()
		attemptID := "att1"
		jobName := installJobName("worker-1", "gen1", "uid-1", attemptID)
		job := &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{Name: jobName, Namespace: OperatorNamespace},
			Status: batchv1.JobStatus{
				Conditions: []batchv1.JobCondition{
					{Type: batchv1.JobFailed, Status: corev1.ConditionTrue},
				},
			},
		}
		node := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "worker-1",
				UID:  types.UID("uid-1"),
				Annotations: map[string]string{
					annInstallAttemptID: attemptID,
				},
			},
		}
		r := newTestReconciler(node, job)
		if r.nodePhase(node, "gen1") != phaseFailed {
			t.Error("expected Failed")
		}
	})

	t.Run("completed Job means Completed", func(t *testing.T) {
		t.Parallel()
		attemptID := "att1"
		jobName := installJobName("worker-1", "gen1", "uid-1", attemptID)
		job := &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{Name: jobName, Namespace: OperatorNamespace},
			Status: batchv1.JobStatus{
				Conditions: []batchv1.JobCondition{
					{Type: batchv1.JobComplete, Status: corev1.ConditionTrue},
				},
			},
		}
		node := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "worker-1",
				UID:  types.UID("uid-1"),
				Annotations: map[string]string{
					annInstallAttemptID: attemptID,
				},
			},
		}
		r := newTestReconciler(node, job)
		if r.nodePhase(node, "gen1") != phaseCompleted {
			t.Error("expected Completed")
		}
	})
}

func TestLifecycleStaleness(t *testing.T) {
	t.Run("stale completed install Job after cleanup requires fresh install", func(t *testing.T) {
		gen := "gen1"
		uid := "uid-A"
		oldAttempt := "old-attempt"

		// Old completed install Job still exists.
		oldJobName := installJobName("worker-1", gen, uid, oldAttempt)
		oldJob := &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{Name: oldJobName, Namespace: OperatorNamespace},
			Status: batchv1.JobStatus{
				Conditions: []batchv1.JobCondition{
					{Type: batchv1.JobComplete, Status: corev1.ConditionTrue},
				},
			},
		}

		// Node was cleaned: no attempt ID, no generation annotations.
		node := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "worker-1",
				UID:  types.UID(uid),
			},
		}

		r := newTestReconciler(node, oldJob)

		phase := r.nodePhase(node, gen)
		// No attempt ID on node => phaseNeeded, regardless of the old Job.
		if phase != phaseNeeded {
			t.Errorf("expected Needed after cleanup/reselect, got %q", phase)
		}
	})

	t.Run("old cleanup Job must not prove cleanup for replacement node", func(t *testing.T) {
		oldAttempt := "old-cleanup-att"
		oldJobName := cleanupJobName("worker-1", "uid-A", oldAttempt)
		oldJob := &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{
				Name:      oldJobName,
				Namespace: OperatorNamespace,
				Labels:    map[string]string{labelApp: appCleanup, labelTargetNode: "worker-1"},
			},
			Status: batchv1.JobStatus{
				Conditions: []batchv1.JobCondition{
					{Type: batchv1.JobComplete, Status: corev1.ConditionTrue},
				},
			},
		}

		// Replacement node uid-B, no cleanup attempt ID.
		node := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "worker-1",
				UID:         types.UID("uid-B"),
				Annotations: map[string]string{annManagedBy: "kc-uid"},
			},
		}

		r := newTestReconciler(node, oldJob)

		phase := r.cleanupPhase(node)
		if phase == phaseCleaned {
			t.Error("old cleanup Job for uid-A must not prove cleanup for uid-B")
		}
		if phase != phaseNeeded {
			t.Errorf("expected Needed, got %q", phase)
		}
	})

	t.Run("active cleanup blocks new install in same reconcile", func(t *testing.T) {
		cleanupJob := &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "kata-cleanup-abc",
				Namespace: OperatorNamespace,
				Labels:    map[string]string{labelApp: appCleanup},
			},
			Status: batchv1.JobStatus{Active: 1},
		}
		r := newTestReconciler(cleanupJob)

		count, err := r.countActiveMutations()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if count < maxConcurrentNodeMutations {
			t.Errorf("expected active mutations >= %d, got %d", maxConcurrentNodeMutations, count)
		}
	})

	t.Run("active install blocks new cleanup", func(t *testing.T) {
		installJob := &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "kata-install-abc",
				Namespace: OperatorNamespace,
				Labels:    map[string]string{labelApp: appInstall},
			},
			Status: batchv1.JobStatus{Active: 1},
		}
		r := newTestReconciler(installJob)

		count, err := r.countActiveMutations()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if count < maxConcurrentNodeMutations {
			t.Errorf("expected active mutations >= %d, got %d", maxConcurrentNodeMutations, count)
		}
	})
}

func TestInstallJobName(t *testing.T) {
	t.Run("deterministic", func(t *testing.T) {
		n1 := installJobName("w1", "g1", "u1", "a1")
		n2 := installJobName("w1", "g1", "u1", "a1")
		if n1 != n2 {
			t.Errorf("expected stable name, got %q and %q", n1, n2)
		}
	})

	t.Run("different for different attempt IDs", func(t *testing.T) {
		n1 := installJobName("w1", "g1", "u1", "a1")
		n2 := installJobName("w1", "g1", "u1", "a2")
		if n1 == n2 {
			t.Error("expected different names for different attempt IDs")
		}
	})

	t.Run("different for different UIDs same attempt", func(t *testing.T) {
		n1 := installJobName("w1", "g1", "uid-A", "a1")
		n2 := installJobName("w1", "g1", "uid-B", "a1")
		if n1 == n2 {
			t.Error("expected different names for different UIDs")
		}
	})
}

func TestRenderInstallJob(t *testing.T) {
	t.Run("sets images by container name", func(t *testing.T) {
		r := newTestReconciler()
		kataImage := "quay.io/prod/kata-deploy@sha256:abc"
		helperImg := "quay.io/prod/helper@sha256:def"

		job, err := r.renderInstallJob("worker-1", "uid-1", "cri-o://1.35.6", "gen1",
			kataImage, helperImg, "att1")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		for _, c := range job.Spec.Template.Spec.InitContainers {
			switch c.Name {
			case "host-check", "install-artifacts":
				if c.Image != kataImage {
					t.Errorf("container %q: expected %q, got %q", c.Name, kataImage, c.Image)
				}
			case "selinux-install", "guest-prep", "apply-labels":
				if c.Image != helperImg {
					t.Errorf("container %q: expected %q, got %q", c.Name, helperImg, c.Image)
				}
			}
		}
		for _, c := range job.Spec.Template.Spec.Containers {
			if c.Name == "configure-cri" && c.Image != kataImage {
				t.Errorf("configure-cri: expected %q, got %q", kataImage, c.Image)
			}
		}
	})
}

func TestRenderInstallJobIsFullyResolved(t *testing.T) {
	r := newTestReconciler()

	nodeName := "test-worker-0"
	nodeUID := "uid-123"
	criVersion := "cri-o://1.35.6"
	generation := "gen1"
	kataImage := "quay.io/test/kata-deploy@sha256:abc"
	helperImage := "quay.io/test/helper@sha256:def"
	attemptID := "att1"

	job, err := r.renderInstallJob(
		nodeName, nodeUID, criVersion, generation,
		kataImage, helperImage, attemptID,
	)
	if err != nil {
		t.Fatal(err)
	}

	if job.Namespace != OperatorNamespace {
		t.Errorf("expected namespace %q, got %q", OperatorNamespace, job.Namespace)
	}
	if job.Spec.Template.Spec.NodeName != nodeName {
		t.Errorf("expected nodeName %q, got %q", nodeName, job.Spec.Template.Spec.NodeName)
	}

	b, err := json.Marshal(job)
	if err != nil {
		t.Fatal(err)
	}
	rendered := string(b)

	for _, forbidden := range []string{
		"__TARGET_NODE__",
		"__CONTAINER_RUNTIME_VERSION__",
		"placeholder",
		"quay.io/jensfr/",
	} {
		if strings.Contains(rendered, forbidden) {
			t.Errorf("rendered Job contains unresolved/prototype value %q", forbidden)
		}
	}

	for k, v := range job.Labels {
		if errs := validation.IsValidLabelValue(v); len(errs) > 0 {
			t.Errorf("invalid Job label %q=%q: %v", k, v, errs)
		}
	}
	for k, v := range job.Spec.Template.Labels {
		if errs := validation.IsValidLabelValue(v); len(errs) > 0 {
			t.Errorf("invalid Pod label %q=%q: %v", k, v, errs)
		}
	}

	// Verify shim env vars are structurally set from runtime definition.
	kataDeployNames := toSet(kataDeployContainerNames)
	for _, c := range append(job.Spec.Template.Spec.InitContainers, job.Spec.Template.Spec.Containers...) {
		if !kataDeployNames[c.Name] {
			continue
		}
		shimFound := false
		defaultFound := false
		for _, e := range c.Env {
			if e.Name == "SHIMS_X86_64" {
				shimFound = true
				if e.Value != defaultKataDeployRuntime.Shim {
					t.Errorf("container %q SHIMS_X86_64=%q, want %q",
						c.Name, e.Value, defaultKataDeployRuntime.Shim)
				}
			}
			if e.Name == "DEFAULT_SHIM_X86_64" {
				defaultFound = true
				if e.Value != defaultKataDeployRuntime.Shim {
					t.Errorf("container %q DEFAULT_SHIM_X86_64=%q, want %q",
						c.Name, e.Value, defaultKataDeployRuntime.Shim)
				}
			}
		}
		if !shimFound {
			t.Errorf("container %q missing SHIMS_X86_64 env var", c.Name)
		}
		if !defaultFound {
			t.Errorf("container %q missing DEFAULT_SHIM_X86_64 env var", c.Name)
		}
	}
}

func TestJobsUseCanonicalServiceAccount(t *testing.T) {
	r := newTestReconciler()

	installJob, err := r.renderInstallJob(
		"worker-0", "uid-1", "cri-o://1.35.6", "gen1",
		"quay.io/test/kata:v1", "quay.io/test/helper:v1", "att1")
	if err != nil {
		t.Fatal(err)
	}
	if installJob.Spec.Template.Spec.ServiceAccountName != "kata-install" {
		t.Errorf("install Job SA: expected kata-install, got %q",
			installJob.Spec.Template.Spec.ServiceAccountName)
	}

	cleanupJob := r.renderCleanupJob("worker-0", "uid-1",
		"quay.io/test/kata:v1", "quay.io/test/helper:v1", "att1")
	if cleanupJob.Spec.Template.Spec.ServiceAccountName != "kata-install" {
		t.Errorf("cleanup Job SA: expected kata-install, got %q",
			cleanupJob.Spec.Template.Spec.ServiceAccountName)
	}
}

func TestCleanupJobContainsOSCState(t *testing.T) {
	r := newTestReconciler()
	job := r.renderCleanupJob("worker-1", "uid-1",
		"quay.io/test/kata-deploy:v1", "quay.io/test/helper:v1", "att1")

	found := false
	for _, c := range job.Spec.Template.Spec.InitContainers {
		if c.Name == "cleanup-osc" {
			found = true
			cmd := strings.Join(c.Command, " ")
			if !strings.Contains(cmd, "semodule -r osc_kata_deploy") {
				t.Error("should remove SELinux module")
			}
			if !strings.Contains(cmd, "osc-kata-guest-prep.service") {
				t.Error("should remove boot service")
			}
			if !strings.Contains(cmd, "osbuilder-images") {
				t.Error("should remove guest artifacts")
			}
		}
	}
	if !found {
		t.Error("expected cleanup-osc init container")
	}
}

func TestGenerateAttemptID(t *testing.T) {
	id1, err := generateAttemptID()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	id2, err := generateAttemptID()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id1 == id2 {
		t.Error("expected unique attempt IDs")
	}
	if len(id1) != 32 {
		t.Errorf("expected 32-char hex (128 bits), got %q (len=%d)", id1, len(id1))
	}
}

func TestSCCCoversJobVolumeTypes(t *testing.T) {
	r := newTestReconciler()

	installJob, err := r.renderInstallJob(
		"worker-0", "uid-1", "cri-o://1.35.6", "gen1",
		"quay.io/test/kata:v1", "quay.io/test/helper:v1", "att1")
	if err != nil {
		t.Fatal(err)
	}

	cleanupJob := r.renderCleanupJob("worker-0", "uid-1",
		"quay.io/test/kata:v1", "quay.io/test/helper:v1", "att1")

	scc := desiredKataInstallSCC()
	allowed := map[secv1.FSType]bool{}
	for _, v := range scc.Volumes {
		allowed[v] = true
	}

	checkVolumes := func(t *testing.T, volumes []corev1.Volume) {
		t.Helper()
		for _, vol := range volumes {
			var required secv1.FSType
			switch {
			case vol.HostPath != nil:
				required = secv1.FSTypeHostPath
			case vol.ConfigMap != nil:
				required = secv1.FSTypeConfigMap
			case vol.EmptyDir != nil:
				required = secv1.FSTypeEmptyDir
			case vol.Secret != nil:
				required = secv1.FSTypeSecret
			default:
				t.Fatalf("volume %q uses unhandled source type", vol.Name)
			}
			if !allowed[required] {
				t.Errorf("volume %q requires %q, not in kata-install-scc", vol.Name, required)
			}
		}
	}

	checkVolumes(t, installJob.Spec.Template.Spec.Volumes)
	checkVolumes(t, cleanupJob.Spec.Template.Spec.Volumes)
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

		// Verify the old RC was deleted (has deletionTimestamp or is gone)
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
