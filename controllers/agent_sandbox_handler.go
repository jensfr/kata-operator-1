package controllers

import (
	"context"
	"fmt"

	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

const (
	agentSandboxSubscriptionName    = "agent-sandbox"
	agentSandboxPackageName         = "agent-sandbox"
	agentSandboxCatalogSource       = "redhat-operators"
	agentSandboxCatalogSourceNS     = "openshift-marketplace"
	agentSandboxChannel             = "tech-preview"
	agentSandboxInstallPlanApproval = "Automatic"
)

// handleAgentSandboxFeature manages the OLM Subscription for the agent-sandbox operator.
// When enabled, it creates an OLM Subscription to install the agent-sandbox operator.
// When disabled, it deletes the Subscription so OLM can clean up.
func (r *KataConfigOpenShiftReconciler) handleAgentSandboxFeature(state FeatureGateState) error {
	if state == Enabled {
		return r.ensureAgentSandboxSubscription()
	}
	return r.removeAgentSandboxSubscription()
}

// ensureAgentSandboxSubscription creates the OLM Subscription for agent-sandbox if it doesn't exist.
func (r *KataConfigOpenShiftReconciler) ensureAgentSandboxSubscription() error {
	sub := newAgentSandboxSubscription()

	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(subscriptionGVK())
	err := r.Client.Get(context.TODO(), types.NamespacedName{
		Name:      agentSandboxSubscriptionName,
		Namespace: OperatorNamespace,
	}, existing)

	if err == nil {
		r.Log.Info("Agent sandbox OLM Subscription already exists")
		return nil
	}

	if !k8serrors.IsNotFound(err) {
		return fmt.Errorf("failed to check for agent-sandbox Subscription: %w", err)
	}

	r.Log.Info("Creating agent-sandbox OLM Subscription")
	if err := r.Client.Create(context.TODO(), sub); err != nil {
		return fmt.Errorf("failed to create agent-sandbox Subscription: %w", err)
	}

	r.Log.Info("Agent sandbox OLM Subscription created successfully")
	return nil
}

// removeAgentSandboxSubscription deletes the OLM Subscription for agent-sandbox if it exists.
func (r *KataConfigOpenShiftReconciler) removeAgentSandboxSubscription() error {
	sub := &unstructured.Unstructured{}
	sub.SetGroupVersionKind(subscriptionGVK())
	err := r.Client.Get(context.TODO(), types.NamespacedName{
		Name:      agentSandboxSubscriptionName,
		Namespace: OperatorNamespace,
	}, sub)

	if k8serrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("failed to check for agent-sandbox Subscription: %w", err)
	}

	r.Log.Info("Deleting agent-sandbox OLM Subscription")
	if err := r.Client.Delete(context.TODO(), sub); err != nil {
		if k8serrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("failed to delete agent-sandbox Subscription: %w", err)
	}

	r.Log.Info("Agent sandbox OLM Subscription deleted successfully")
	return nil
}

// newAgentSandboxSubscription builds the unstructured OLM Subscription object.
func newAgentSandboxSubscription() *unstructured.Unstructured {
	sub := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "operators.coreos.com/v1alpha1",
			"kind":       "Subscription",
			"metadata": map[string]interface{}{
				"name":      agentSandboxSubscriptionName,
				"namespace": OperatorNamespace,
				"labels": map[string]interface{}{
					"app.kubernetes.io/managed-by": "sandboxed-containers-operator",
					"app.kubernetes.io/part-of":    "agent-sandbox",
				},
			},
			"spec": map[string]interface{}{
				"channel":             agentSandboxChannel,
				"installPlanApproval": agentSandboxInstallPlanApproval,
				"name":               agentSandboxPackageName,
				"source":             agentSandboxCatalogSource,
				"sourceNamespace":    agentSandboxCatalogSourceNS,
			},
		},
	}
	return sub
}

func subscriptionGVK() schema.GroupVersionKind {
	return schema.GroupVersionKind{
		Group:   "operators.coreos.com",
		Version: "v1alpha1",
		Kind:    "Subscription",
	}
}
