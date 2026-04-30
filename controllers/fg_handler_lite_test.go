package controllers

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
)

var _ = Describe("Lite Feature Gate", func() {

	var reconciler *KataConfigOpenShiftReconciler

	BeforeEach(func() {
		reconciler = &KataConfigOpenShiftReconciler{
			Client: k8sClient,
			Log:    ctrl.Log.WithName("test").WithName("lite"),
			Scheme: k8sClient.Scheme(),
		}
	})

	Context("when the feature-gates ConfigMap does not exist", func() {
		It("should default enableLite to false", func() {
			fgStatus, err := reconciler.NewFeatureGateStatus()
			Expect(err).ToNot(HaveOccurred())
			Expect(fgStatus.EnableLite).To(BeFalse())
		})
	})

	Context("when the feature-gates ConfigMap has enableLite=true", func() {
		BeforeEach(func() {
			// Ensure the operator namespace exists
			ns := &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: OperatorNamespace,
				},
			}
			_ = k8sClient.Create(context.TODO(), ns)

			cm := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      FgConfigMapName,
					Namespace: OperatorNamespace,
				},
				Data: map[string]string{
					EnableLiteFeatureGate: "true",
				},
			}
			err := k8sClient.Create(context.TODO(), cm)
			Expect(err).ToNot(HaveOccurred())
		})

		AfterEach(func() {
			cm := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      FgConfigMapName,
					Namespace: OperatorNamespace,
				},
			}
			_ = k8sClient.Delete(context.TODO(), cm)
		})

		It("should return enableLite as true", func() {
			fgStatus, err := reconciler.NewFeatureGateStatus()
			Expect(err).ToNot(HaveOccurred())
			Expect(fgStatus.EnableLite).To(BeTrue())
		})

		It("should be enabled via IsEnabled", func() {
			fgStatus, err := reconciler.NewFeatureGateStatus()
			Expect(err).ToNot(HaveOccurred())
			Expect(fgStatus.IsEnabled(EnableLiteFeatureGate)).To(BeTrue())
		})
	})

	Context("when the feature-gates ConfigMap has enableLite=false", func() {
		BeforeEach(func() {
			ns := &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: OperatorNamespace,
				},
			}
			_ = k8sClient.Create(context.TODO(), ns)

			cm := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      FgConfigMapName,
					Namespace: OperatorNamespace,
				},
				Data: map[string]string{
					EnableLiteFeatureGate: "false",
				},
			}
			err := k8sClient.Create(context.TODO(), cm)
			Expect(err).ToNot(HaveOccurred())
		})

		AfterEach(func() {
			cm := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      FgConfigMapName,
					Namespace: OperatorNamespace,
				},
			}
			_ = k8sClient.Delete(context.TODO(), cm)
		})

		It("should return enableLite as false", func() {
			fgStatus, err := reconciler.NewFeatureGateStatus()
			Expect(err).ToNot(HaveOccurred())
			Expect(fgStatus.EnableLite).To(BeFalse())
		})
	})
})
