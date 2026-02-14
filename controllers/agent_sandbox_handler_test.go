package controllers

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
)

var _ = Describe("Agent Sandbox Handler", func() {

	var reconciler *KataConfigOpenShiftReconciler

	BeforeEach(func() {
		reconciler = &KataConfigOpenShiftReconciler{
			Client: k8sClient,
			Log:    ctrl.Log.WithName("test").WithName("agent-sandbox"),
			Scheme: k8sManager.GetScheme(),
		}

		// Ensure the operator namespace exists
		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: OperatorNamespace,
			},
		}
		err := k8sClient.Create(context.TODO(), ns)
		if err != nil && !k8serrors.IsAlreadyExists(err) {
			Expect(err).ToNot(HaveOccurred())
		}
	})

	AfterEach(func() {
		// Clean up any Subscription left over
		sub := &unstructured.Unstructured{}
		sub.SetGroupVersionKind(subscriptionGVK())
		err := k8sClient.Get(context.TODO(), types.NamespacedName{
			Name:      agentSandboxSubscriptionName,
			Namespace: OperatorNamespace,
		}, sub)
		if err == nil {
			_ = k8sClient.Delete(context.TODO(), sub)
		}
	})

	Context("when feature gate is enabled", func() {
		It("should create an OLM Subscription", func() {
			err := reconciler.handleAgentSandboxFeature(Enabled)
			Expect(err).ToNot(HaveOccurred())

			sub := &unstructured.Unstructured{}
			sub.SetGroupVersionKind(subscriptionGVK())
			err = k8sClient.Get(context.TODO(), types.NamespacedName{
				Name:      agentSandboxSubscriptionName,
				Namespace: OperatorNamespace,
			}, sub)
			Expect(err).ToNot(HaveOccurred())

			spec, found, err := unstructured.NestedMap(sub.Object, "spec")
			Expect(err).ToNot(HaveOccurred())
			Expect(found).To(BeTrue())
			Expect(spec["channel"]).To(Equal(agentSandboxChannel))
			Expect(spec["name"]).To(Equal(agentSandboxPackageName))
			Expect(spec["source"]).To(Equal(agentSandboxCatalogSource))
			Expect(spec["sourceNamespace"]).To(Equal(agentSandboxCatalogSourceNS))
			Expect(spec["installPlanApproval"]).To(Equal(agentSandboxInstallPlanApproval))
		})
	})

	Context("when feature gate is disabled", func() {
		It("should delete the OLM Subscription if it exists", func() {
			// First enable to create the subscription
			err := reconciler.handleAgentSandboxFeature(Enabled)
			Expect(err).ToNot(HaveOccurred())

			// Verify it exists
			sub := &unstructured.Unstructured{}
			sub.SetGroupVersionKind(subscriptionGVK())
			err = k8sClient.Get(context.TODO(), types.NamespacedName{
				Name:      agentSandboxSubscriptionName,
				Namespace: OperatorNamespace,
			}, sub)
			Expect(err).ToNot(HaveOccurred())

			// Now disable
			err = reconciler.handleAgentSandboxFeature(Disabled)
			Expect(err).ToNot(HaveOccurred())

			// Verify it's gone
			sub = &unstructured.Unstructured{}
			sub.SetGroupVersionKind(subscriptionGVK())
			err = k8sClient.Get(context.TODO(), types.NamespacedName{
				Name:      agentSandboxSubscriptionName,
				Namespace: OperatorNamespace,
			}, sub)
			Expect(k8serrors.IsNotFound(err)).To(BeTrue())
		})

		It("should not error if no Subscription exists", func() {
			err := reconciler.handleAgentSandboxFeature(Disabled)
			Expect(err).ToNot(HaveOccurred())
		})
	})

	Context("when feature gate is not set (default off)", func() {
		It("should not create a Subscription", func() {
			// Disabled is the default behavior when the feature gate is not set
			err := reconciler.handleAgentSandboxFeature(Disabled)
			Expect(err).ToNot(HaveOccurred())

			sub := &unstructured.Unstructured{}
			sub.SetGroupVersionKind(subscriptionGVK())
			err = k8sClient.Get(context.TODO(), types.NamespacedName{
				Name:      agentSandboxSubscriptionName,
				Namespace: OperatorNamespace,
			}, sub)
			Expect(k8serrors.IsNotFound(err)).To(BeTrue())
		})
	})

	Context("idempotency", func() {
		It("should handle enable being called twice without error", func() {
			err := reconciler.handleAgentSandboxFeature(Enabled)
			Expect(err).ToNot(HaveOccurred())

			err = reconciler.handleAgentSandboxFeature(Enabled)
			Expect(err).ToNot(HaveOccurred())

			// Still only one Subscription
			sub := &unstructured.Unstructured{}
			sub.SetGroupVersionKind(subscriptionGVK())
			err = k8sClient.Get(context.TODO(), types.NamespacedName{
				Name:      agentSandboxSubscriptionName,
				Namespace: OperatorNamespace,
			}, sub)
			Expect(err).ToNot(HaveOccurred())
		})

		It("should handle disable being called twice without error", func() {
			err := reconciler.handleAgentSandboxFeature(Disabled)
			Expect(err).ToNot(HaveOccurred())

			err = reconciler.handleAgentSandboxFeature(Disabled)
			Expect(err).ToNot(HaveOccurred())
		})
	})

	Context("toggle lifecycle", func() {
		It("should support enable → disable → enable cycle", func() {
			// Enable
			err := reconciler.handleAgentSandboxFeature(Enabled)
			Expect(err).ToNot(HaveOccurred())

			sub := &unstructured.Unstructured{}
			sub.SetGroupVersionKind(subscriptionGVK())
			err = k8sClient.Get(context.TODO(), types.NamespacedName{
				Name:      agentSandboxSubscriptionName,
				Namespace: OperatorNamespace,
			}, sub)
			Expect(err).ToNot(HaveOccurred())

			// Disable
			err = reconciler.handleAgentSandboxFeature(Disabled)
			Expect(err).ToNot(HaveOccurred())

			sub = &unstructured.Unstructured{}
			sub.SetGroupVersionKind(subscriptionGVK())
			err = k8sClient.Get(context.TODO(), types.NamespacedName{
				Name:      agentSandboxSubscriptionName,
				Namespace: OperatorNamespace,
			}, sub)
			Expect(k8serrors.IsNotFound(err)).To(BeTrue())

			// Re-enable
			err = reconciler.handleAgentSandboxFeature(Enabled)
			Expect(err).ToNot(HaveOccurred())

			sub = &unstructured.Unstructured{}
			sub.SetGroupVersionKind(subscriptionGVK())
			err = k8sClient.Get(context.TODO(), types.NamespacedName{
				Name:      agentSandboxSubscriptionName,
				Namespace: OperatorNamespace,
			}, sub)
			Expect(err).ToNot(HaveOccurred())
		})
	})

	Context("newAgentSandboxSubscription", func() {
		It("should create a well-formed Subscription object", func() {
			sub := newAgentSandboxSubscription()
			Expect(sub.GetKind()).To(Equal("Subscription"))
			Expect(sub.GetAPIVersion()).To(Equal("operators.coreos.com/v1alpha1"))
			Expect(sub.GetName()).To(Equal(agentSandboxSubscriptionName))
			Expect(sub.GetNamespace()).To(Equal(OperatorNamespace))

			labels := sub.GetLabels()
			Expect(labels["app.kubernetes.io/managed-by"]).To(Equal("sandboxed-containers-operator"))
			Expect(labels["app.kubernetes.io/part-of"]).To(Equal("agent-sandbox"))
		})
	})

	Context("FeatureGateStatus integration", func() {
		It("should have AgentSandbox defaulting to false", func() {
			Expect(DefaultFeatureGates.AgentSandbox).To(BeFalse())
		})

		It("should report AgentSandbox status via IsEnabled", func() {
			fgStatus := &FeatureGateStatus{AgentSandbox: true}
			Expect(fgStatus.IsEnabled(AgentSandboxFeatureGate)).To(BeTrue())

			fgStatus = &FeatureGateStatus{AgentSandbox: false}
			Expect(fgStatus.IsEnabled(AgentSandboxFeatureGate)).To(BeFalse())
		})
	})
})
