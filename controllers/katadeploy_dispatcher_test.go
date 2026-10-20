package controllers

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	kataconfigurationv1 "github.com/openshift/sandboxed-containers-operator/api/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/go-logr/logr"
	mcfgv1 "github.com/openshift/api/machineconfiguration/v1"
	secv1 "github.com/openshift/api/security/v1"
	nodeapi "k8s.io/api/node/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestDispatcherSELinuxCMRepairsMissingMonitorPolicy(t *testing.T) {
	for _, existing := range []map[string][]byte{nil, {
		"osc_kata_deploy.pp": dispatcherSELinuxPolicy,
		"unrelated":          []byte("preserve"),
	}} {
		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: selinuxCMName, Namespace: OperatorNamespace},
			BinaryData: existing,
		}
		r := newDispatcherTestReconciler(cm)
		if err := r.ensureDispatcherSELinuxCM(); err != nil {
			t.Fatal(err)
		}
		if err := r.Client.Get(context.Background(), client.ObjectKeyFromObject(cm), cm); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(cm.BinaryData["osc_monitor.cil"], dispatcherMonitorSELinuxPolicy) {
			t.Fatal("existing file-context policy must not hide a missing monitor policy")
		}
		if !bytes.Contains(cm.BinaryData["osc_monitor.cil"], []byte("(block osc_monitor")) {
			t.Fatal("monitor policy must define the osc_monitor.process type used by the DaemonSet")
		}
		if existing != nil && string(cm.BinaryData["unrelated"]) != "preserve" {
			t.Fatal("policy repair removed unrelated ConfigMap data")
		}
	}
}

func newDispatcherTestReconciler(objs ...runtime.Object) *KataConfigOpenShiftReconciler {
	s := runtime.NewScheme()
	_ = corev1.AddToScheme(s)
	_ = batchv1.AddToScheme(s)
	_ = nodeapi.AddToScheme(s)
	_ = kataconfigurationv1.AddToScheme(s)
	_ = mcfgv1.Install(s)

	return &KataConfigOpenShiftReconciler{
		Client: fake.NewClientBuilder().WithScheme(s).WithRuntimeObjects(objs...).Build(),
		Log:    logr.Discard(),
		Scheme: s,
		kataConfig: &kataconfigurationv1.KataConfig{
			ObjectMeta: metav1.ObjectMeta{
				Name: "example-kataconfig",
				UID:  types.UID("kc-uid-test"),
			},
			Spec: kataconfigurationv1.KataConfigSpec{},
		},
	}
}

func TestDispatcherNodeSelector(t *testing.T) {
	t.Run("nil selector returns worker", func(t *testing.T) {
		r := newDispatcherTestReconciler()
		sel, err := r.dispatcherNodeSelector()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if sel != "node-role.kubernetes.io/worker=" {
			t.Errorf("expected worker selector, got %q", sel)
		}
	})

	t.Run("single matchLabel returns worker AND label", func(t *testing.T) {
		r := newDispatcherTestReconciler()
		r.kataConfig.Spec.KataConfigPoolSelector = &metav1.LabelSelector{
			MatchLabels: map[string]string{"gpu": "true"},
		}
		sel, err := r.dispatcherNodeSelector()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if sel != "node-role.kubernetes.io/worker=,gpu=true" {
			t.Errorf("expected worker,gpu=true, got %q", sel)
		}
	})

	t.Run("two matchLabels are both included", func(t *testing.T) {
		r := newDispatcherTestReconciler()
		r.kataConfig.Spec.KataConfigPoolSelector = &metav1.LabelSelector{
			MatchLabels: map[string]string{"a": "1", "b": "2"},
		}
		sel, err := r.dispatcherNodeSelector()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		// LabelSelectorAsSelector produces canonical ordering (alphabetical)
		expected1 := "node-role.kubernetes.io/worker=,a=1,b=2"
		expected2 := "node-role.kubernetes.io/worker=,b=2,a=1"
		if sel != expected1 && sel != expected2 {
			t.Errorf("expected both labels, got %q", sel)
		}
	})

	t.Run("matchExpressions are serialized", func(t *testing.T) {
		r := newDispatcherTestReconciler()
		r.kataConfig.Spec.KataConfigPoolSelector = &metav1.LabelSelector{
			MatchExpressions: []metav1.LabelSelectorRequirement{{
				Key:      "zone",
				Operator: metav1.LabelSelectorOpIn,
				Values:   []string{"us-east", "us-west"},
			}},
		}
		sel, err := r.dispatcherNodeSelector()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if sel == "node-role.kubernetes.io/worker=" {
			t.Error("matchExpression was ignored")
		}
		t.Logf("selector: %s", sel)
	})

	t.Run("CheckNodeEligibility adds runtime.kata requirement", func(t *testing.T) {
		r := newDispatcherTestReconciler()
		r.kataConfig.Spec.CheckNodeEligibility = true
		sel, err := r.dispatcherNodeSelector()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if sel != "node-role.kubernetes.io/worker=,feature.node.kubernetes.io/runtime.kata=true" {
			t.Errorf("expected eligibility requirement, got %q", sel)
		}
	})

	t.Run("CheckNodeEligibility combined with pool selector", func(t *testing.T) {
		r := newDispatcherTestReconciler()
		r.kataConfig.Spec.CheckNodeEligibility = true
		r.kataConfig.Spec.KataConfigPoolSelector = &metav1.LabelSelector{
			MatchLabels: map[string]string{"gpu": "true"},
		}
		sel, err := r.dispatcherNodeSelector()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		// Both gpu=true AND runtime.kata=true should be present
		if sel == "node-role.kubernetes.io/worker=" {
			t.Error("pool selector and eligibility were both ignored")
		}
		t.Logf("selector: %s", sel)
	})

	t.Run("converged cluster still uses worker selector", func(t *testing.T) {
		// KataDeploy does not check MCPs. On compact clusters,
		// nodes carry both master and worker roles, so worker= matches.
		masterMCP := &mcfgv1.MachineConfigPool{
			ObjectMeta: metav1.ObjectMeta{Name: "master"},
			Status:     mcfgv1.MachineConfigPoolStatus{MachineCount: 3},
		}
		workerMCP := &mcfgv1.MachineConfigPool{
			ObjectMeta: metav1.ObjectMeta{Name: "worker"},
			Status:     mcfgv1.MachineConfigPoolStatus{MachineCount: 0},
		}
		r := newDispatcherTestReconciler(masterMCP, workerMCP)

		sel, err := r.dispatcherNodeSelector()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if sel != "node-role.kubernetes.io/worker=" {
			t.Errorf("expected worker selector (MCP-independent), got %q", sel)
		}
	})
}

func TestIsTerminalJob(t *testing.T) {
	t.Run("no conditions is not terminal", func(t *testing.T) {
		j := &batchv1.Job{}
		if isTerminalJob(j) {
			t.Error("empty Job should not be terminal")
		}
	})

	t.Run("active job is not terminal", func(t *testing.T) {
		j := &batchv1.Job{Status: batchv1.JobStatus{Active: 1}}
		if isTerminalJob(j) {
			t.Error("active Job should not be terminal")
		}
	})

	t.Run("complete job is terminal", func(t *testing.T) {
		j := &batchv1.Job{
			Status: batchv1.JobStatus{
				Conditions: []batchv1.JobCondition{{
					Type:   batchv1.JobComplete,
					Status: corev1.ConditionTrue,
				}},
			},
		}
		if !isTerminalJob(j) {
			t.Error("completed Job should be terminal")
		}
	})

	t.Run("failed job is terminal", func(t *testing.T) {
		j := &batchv1.Job{
			Status: batchv1.JobStatus{
				Conditions: []batchv1.JobCondition{{
					Type:   batchv1.JobFailed,
					Status: corev1.ConditionTrue,
				}},
			},
		}
		if !isTerminalJob(j) {
			t.Error("failed Job should be terminal")
		}
	})

	t.Run("condition false is not terminal", func(t *testing.T) {
		j := &batchv1.Job{
			Status: batchv1.JobStatus{
				Conditions: []batchv1.JobCondition{{
					Type:   batchv1.JobComplete,
					Status: corev1.ConditionFalse,
				}},
			},
		}
		if isTerminalJob(j) {
			t.Error("ConditionFalse should not be terminal")
		}
	})
}

func TestHasActiveFleetOperation(t *testing.T) {
	t.Run("no jobs means no active operation", func(t *testing.T) {
		r := newDispatcherTestReconciler()
		active, err := r.hasActiveFleetOperation()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if active {
			t.Error("no jobs should mean no active operation")
		}
	})

	t.Run("terminal job is not active", func(t *testing.T) {
		j := &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "kata-rollout-abc",
				Namespace: OperatorNamespace,
				Labels:    map[string]string{labelFleetOperation: opRollout},
			},
			Status: batchv1.JobStatus{
				Conditions: []batchv1.JobCondition{{
					Type:   batchv1.JobComplete,
					Status: corev1.ConditionTrue,
				}},
			},
		}
		r := newDispatcherTestReconciler(j)
		active, err := r.hasActiveFleetOperation()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if active {
			t.Error("completed job should not count as active")
		}
	})

	t.Run("pending job is active", func(t *testing.T) {
		j := &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "kata-rollout-abc",
				Namespace: OperatorNamespace,
				Labels:    map[string]string{labelFleetOperation: opRollout},
			},
			Status: batchv1.JobStatus{},
		}
		r := newDispatcherTestReconciler(j)
		active, err := r.hasActiveFleetOperation()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !active {
			t.Error("pending job (no conditions, active=0) should count as active")
		}
	})
}

func TestDispatcherGenerationStability(t *testing.T) {
	t.Run("same selector with different map insertion is stable", func(t *testing.T) {
		t.Setenv("RELATED_IMAGE_KATA_DEPLOY", "quay.io/test/kata:v1")
		t.Setenv("RELATED_IMAGE_OSC_HELPER", "quay.io/test/helper:v1")

		r1 := newDispatcherTestReconciler()
		r1.kataConfig.Spec.KataConfigPoolSelector = &metav1.LabelSelector{
			MatchLabels: map[string]string{"a": "1", "b": "2", "c": "3"},
		}

		r2 := newDispatcherTestReconciler()
		r2.kataConfig.Spec.KataConfigPoolSelector = &metav1.LabelSelector{
			MatchLabels: map[string]string{"c": "3", "a": "1", "b": "2"},
		}

		g1, err := r1.dispatcherDesiredGeneration()
		if err != nil {
			t.Fatalf("r1: %v", err)
		}
		g2, err := r2.dispatcherDesiredGeneration()
		if err != nil {
			t.Fatalf("r2: %v", err)
		}
		if g1 != g2 {
			t.Errorf("generation should be stable across map orderings, got %q vs %q", g1, g2)
		}
	})
}

func TestDispatcherGenerationAfterSelectorRevert(t *testing.T) {
	r := newDispatcherTestReconciler()
	generation := func() string {
		t.Helper()
		value, err := r.dispatcherDesiredGeneration()
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	r.kataConfig.Generation = 1
	original := generation()
	r.kataConfig.Generation = 2
	r.kataConfig.Spec.KataConfigPoolSelector = &metav1.LabelSelector{
		MatchLabels: map[string]string{"selected": "true"},
	}
	changed := generation()
	r.kataConfig.Generation = 3
	r.kataConfig.Spec.KataConfigPoolSelector = nil
	restored := generation()
	if restored == original || restored == changed || r.dispatcherJobName(restored) == r.dispatcherJobName(original) {
		t.Fatal("restoring a selector must create new work instead of reusing its completed Jobs")
	}

	// Coverage must still handle node addition without changing the rollout generation.
	if err := r.Client.Create(context.Background(), &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "new-worker", Labels: map[string]string{"node-role.kubernetes.io/worker": ""}},
	}); err != nil {
		t.Fatal(err)
	}
	if generation() != restored {
		t.Fatal("node addition must not change the rollout generation")
	}
}

type generationPatchRecorder struct {
	client.Client
	data []byte
}

func (c *generationPatchRecorder) Patch(_ context.Context, obj client.Object, patch client.Patch, _ ...client.PatchOption) error {
	var err error
	c.data, err = patch.Data(obj)
	return err
}

func TestDispatcherAcknowledgementDoesNotWriteSpec(t *testing.T) {
	r := newDispatcherTestReconciler()
	r.kataConfig.ResourceVersion = "17"
	recorder := &generationPatchRecorder{Client: r.Client}
	r.Client = recorder
	if err := r.recordDispatcherGeneration("completed"); err != nil {
		t.Fatal(err)
	}
	var patch map[string]json.RawMessage
	if err := json.Unmarshal(recorder.data, &patch); err != nil {
		t.Fatal(err)
	}
	if _, ok := patch["spec"]; ok {
		t.Fatal("acknowledgement must not normalize absent nullable spec fields and increment generation")
	}
	var metadata struct {
		ResourceVersion string            `json:"resourceVersion"`
		Annotations     map[string]string `json:"annotations"`
	}
	if err := json.Unmarshal(patch["metadata"], &metadata); err != nil {
		t.Fatal(err)
	}
	if metadata.ResourceVersion != "17" || metadata.Annotations[annObservedGeneration] != "completed" {
		t.Fatalf("acknowledgement must record the completed generation with optimistic locking: %s", recorder.data)
	}
}

func TestDispatcherNodeWorkFlags(t *testing.T) {
	flags := dispatcherNodeWorkFlags()
	expected := map[string]bool{
		"--tracking-label-prefix=kata-deploy-job-dispatcher":    false,
		"--node-label-key=katacontainers.io/kata-runtime":       false,
		"--instance-label-prefix=kata-deploy.katacontainers.io": false,
		"--require-node-runtime-version":                        false,
		"--require-node-machine-id":                             false,
	}
	for _, f := range flags {
		if _, ok := expected[f]; ok {
			expected[f] = true
		}
	}
	for k, found := range expected {
		if !found {
			t.Errorf("missing upstream flag: %s", k)
		}
	}
}

func TestDispatcherRolloutHasRunOwner(t *testing.T) {
	r := newDispatcherTestReconciler()
	generation, err := r.dispatcherDesiredGeneration()
	if err != nil {
		t.Fatal(err)
	}
	if err := r.createDispatcherJob(generation); err != nil {
		t.Fatal(err)
	}
	job := &batchv1.Job{}
	name := r.dispatcherJobName(generation)
	if err := r.Client.Get(context.Background(), client.ObjectKey{Name: name, Namespace: OperatorNamespace}, job); err != nil {
		t.Fatal(err)
	}
	for _, arg := range job.Spec.Template.Spec.Containers[0].Args {
		if arg == "--owner-job-name="+name {
			return
		}
	}
	t.Fatal("per-node Jobs must belong to this rollout so retries replace stale Jobs")
}

func TestActiveNodeWorkBlocksCleanup(t *testing.T) {
	for _, condition := range []batchv1.JobConditionType{"", batchv1.JobComplete, batchv1.JobFailed} {
		t.Run(string(condition), func(t *testing.T) {
			job := statusTestJob("orphan-install", "kata-old-generation", "worker-a", condition)
			r := newDispatcherTestReconciler(job)
			active, err := r.hasActiveFleetOperation()
			if err != nil {
				t.Fatal(err)
			}
			if active != (condition == "") {
				t.Fatalf("active=%v for per-node condition %q", active, condition)
			}
		})
	}
}

func TestDispatcherRejectsPreviousKataConfigRollout(t *testing.T) {
	r := newDispatcherTestReconciler()
	if err := secv1.AddToScheme(r.Scheme); err != nil {
		t.Fatal(err)
	}
	if err := r.Client.Create(context.Background(), r.kataConfig); err != nil {
		t.Fatal(err)
	}
	generation, err := r.dispatcherDesiredGeneration()
	if err != nil {
		t.Fatal(err)
	}
	oldJob := statusTestJob(r.dispatcherJobName(generation), "", "", batchv1.JobComplete)
	oldJob.OwnerReferences = []metav1.OwnerReference{{UID: types.UID("previous-kataconfig"), Controller: boolPtr(true)}}
	if err := r.Client.Create(context.Background(), oldJob); err != nil {
		t.Fatal(err)
	}
	result, err := r.processKataConfigInstallRequestDispatcher()
	if err != nil || result.RequeueAfter == 0 {
		t.Fatalf("old rollout must be removed before installing: result=%+v err=%v", result, err)
	}
	if r.kataConfig.Annotations[annObservedGeneration] != "" {
		t.Fatal("old KataConfig's completed rollout must not mark this installation complete")
	}
}
