package controllers

import (
	"testing"

	kataconfigurationv1 "github.com/openshift/sandboxed-containers-operator/api/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/go-logr/logr"
	mcfgv1 "github.com/openshift/api/machineconfiguration/v1"
	nodeapi "k8s.io/api/node/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

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
