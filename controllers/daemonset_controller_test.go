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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kataconfigurationv1 "github.com/openshift/sandboxed-containers-operator/api/v1"
)

// ============================================================
// FAILING TESTS - TDD Phase 1
// These tests define the expected behavior for the DaemonSet
// controller. They should fail initially until the controller
// is implemented.
// ============================================================

var _ = Describe("DaemonSetController", func() {
	const (
		timeout  = time.Second * 30
		interval = time.Millisecond * 250
	)

	// Track the KataConfig created in each test for cleanup
	var testKataConfig *kataconfigurationv1.KataConfig

	// Create the operator namespace before tests
	BeforeEach(func() {
		ctx := context.Background()
		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: "openshift-sandboxed-containers-operator",
			},
		}
		err := k8sClient.Create(ctx, ns)
		if err != nil && !errors.IsAlreadyExists(err) {
			Expect(err).ToNot(HaveOccurred())
		}
		testKataConfig = nil
	})

	// Clean up any KataConfig after each test and wait for it to be fully deleted
	AfterEach(func() {
		ctx := context.Background()

		// Helper to remove finalizers and delete a KataConfig
		cleanupKataConfig := func(name string) {
			kc := &kataconfigurationv1.KataConfig{}
			err := k8sClient.Get(ctx, types.NamespacedName{Name: name}, kc)
			if errors.IsNotFound(err) {
				return
			}
			if err != nil {
				GinkgoWriter.Printf("Warning: error getting KataConfig %s: %v\n", name, err)
				return
			}

			// Remove all finalizers to allow deletion (cleanup never completes in test env)
			if len(kc.Finalizers) > 0 {
				kc.Finalizers = nil
				if err := k8sClient.Update(ctx, kc); err != nil && !errors.IsNotFound(err) {
					GinkgoWriter.Printf("Warning: error removing finalizers from KataConfig %s: %v\n", name, err)
				}
			}

			// Delete the KataConfig
			if err := k8sClient.Delete(ctx, kc); err != nil && !errors.IsNotFound(err) {
				GinkgoWriter.Printf("Warning: error deleting KataConfig %s: %v\n", name, err)
			}
		}

		// Clean up DaemonSet first (it will be recreated by controller if KataConfig exists)
		ds := &appsv1.DaemonSet{}
		if err := k8sClient.Get(ctx, types.NamespacedName{
			Name:      "kata-deploy",
			Namespace: "openshift-sandboxed-containers-operator",
		}, ds); err == nil {
			// Remove owner references to prevent cascade issues
			ds.OwnerReferences = nil
			_ = k8sClient.Update(ctx, ds)
			_ = k8sClient.Delete(ctx, ds)
		}

		// Clean up RuntimeClasses
		runtimeClasses := []string{"kata-qemu", "kata-qemu-tdx", "kata-qemu-snp", "kata-qemu-se"}
		for _, rcName := range runtimeClasses {
			rc := &nodev1.RuntimeClass{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: rcName}, rc); err == nil {
				rc.OwnerReferences = nil
				rc.Finalizers = nil
				_ = k8sClient.Update(ctx, rc)
				_ = k8sClient.Delete(ctx, rc)
			}
		}

		// If we tracked a KataConfig, ensure it's deleted
		if testKataConfig != nil {
			cleanupKataConfig(testKataConfig.Name)

			// Wait for the KataConfig to be fully deleted
			Eventually(func() bool {
				kc := &kataconfigurationv1.KataConfig{}
				err := k8sClient.Get(ctx, types.NamespacedName{Name: testKataConfig.Name}, kc)
				return errors.IsNotFound(err)
			}, timeout, interval).Should(BeTrue(), "KataConfig should be fully deleted")
		}

		// Also clean up any leftover KataConfigs (belt and suspenders)
		kcList := &kataconfigurationv1.KataConfigList{}
		if err := k8sClient.List(ctx, kcList); err == nil {
			for _, kc := range kcList.Items {
				cleanupKataConfig(kc.Name)
			}
			// Wait for all to be deleted
			Eventually(func() bool {
				kcList := &kataconfigurationv1.KataConfigList{}
				if err := k8sClient.List(ctx, kcList); err != nil {
					return false
				}
				return len(kcList.Items) == 0
			}, timeout, interval).Should(BeTrue(), "All KataConfigs should be deleted")
		}
	})

	Context("Deployment Mode Detection", func() {
		It("should detect DaemonSet mode when DeploymentMode is explicitly set", func() {
			ctx := context.Background()

			testKataConfig = &kataconfigurationv1.KataConfig{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-kataconfig-daemonset-mode",
				},
				Spec: kataconfigurationv1.KataConfigSpec{
					DeploymentMode: kataconfigurationv1.DeploymentModeDaemonSet,
				},
			}

			Expect(k8sClient.Create(ctx, testKataConfig)).Should(Succeed())

			// Verify DaemonSet is created
			daemonSet := &appsv1.DaemonSet{}
			Eventually(func() bool {
				err := k8sClient.Get(ctx, types.NamespacedName{
					Name:      "kata-deploy",
					Namespace: "openshift-sandboxed-containers-operator",
				}, daemonSet)
				return err == nil
			}, timeout, interval).Should(BeTrue())

			// Cleanup handled by AfterEach
		})

		It("should detect MachineConfig mode when MCO CRD is available", func() {
			Skip("Requires deployment mode detection logic - TDD placeholder")

			// This test verifies that when MachineConfig CRD exists,
			// the controller uses MachineConfig mode by default
		})

		It("should detect DaemonSet mode when MCO CRD is not available (HCP)", func() {
			Skip("Requires deployment mode detection logic - TDD placeholder")

			// This test verifies that on HCP clusters (no MCO),
			// the controller automatically uses DaemonSet mode
		})
	})

	Context("DaemonSet Creation", func() {
		It("should create a DaemonSet with correct configuration", func() {
			ctx := context.Background()

			// Create KataConfig with DaemonSet mode
			testKataConfig = &kataconfigurationv1.KataConfig{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-kataconfig-ds-creation",
				},
				Spec: kataconfigurationv1.KataConfigSpec{
					DeploymentMode: kataconfigurationv1.DeploymentModeDaemonSet,
					Platform:       "rhcos",
				},
			}

			Expect(k8sClient.Create(ctx, testKataConfig)).Should(Succeed())

			// Verify DaemonSet is created with correct spec
			daemonSet := &appsv1.DaemonSet{}
			Eventually(func() bool {
				err := k8sClient.Get(ctx, types.NamespacedName{
					Name:      "kata-deploy",
					Namespace: "openshift-sandboxed-containers-operator",
				}, daemonSet)
				return err == nil
			}, timeout, interval).Should(BeTrue())

			// Verify DaemonSet spec
			Expect(daemonSet.Spec.Template.Spec.HostNetwork).To(BeTrue())
			Expect(daemonSet.Spec.Template.Spec.HostPID).To(BeTrue())
			Expect(daemonSet.Spec.Template.Spec.PriorityClassName).To(Equal("system-node-critical"))

			// Verify container configuration
			Expect(daemonSet.Spec.Template.Spec.Containers).To(HaveLen(1))
			container := daemonSet.Spec.Template.Spec.Containers[0]
			Expect(container.Name).To(Equal("kata-deploy"))
			Expect(container.Command).To(ContainElement("install"))

			// Cleanup handled by AfterEach
		})

		It("should set PLATFORM environment variable based on KataConfig spec", func() {
			ctx := context.Background()

			testKataConfig = &kataconfigurationv1.KataConfig{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-kataconfig-platform-env",
				},
				Spec: kataconfigurationv1.KataConfigSpec{
					DeploymentMode: kataconfigurationv1.DeploymentModeDaemonSet,
					Platform:       "rhcos",
				},
			}

			Expect(k8sClient.Create(ctx, testKataConfig)).Should(Succeed())

			// Verify DaemonSet has PLATFORM env var
			daemonSet := &appsv1.DaemonSet{}
			Eventually(func() bool {
				err := k8sClient.Get(ctx, types.NamespacedName{
					Name:      "kata-deploy",
					Namespace: "openshift-sandboxed-containers-operator",
				}, daemonSet)
				return err == nil
			}, timeout, interval).Should(BeTrue())

			// Find PLATFORM env var
			container := daemonSet.Spec.Template.Spec.Containers[0]
			var platformEnv *corev1.EnvVar
			for i := range container.Env {
				if container.Env[i].Name == "PLATFORM" {
					platformEnv = &container.Env[i]
					break
				}
			}

			Expect(platformEnv).NotTo(BeNil())
			Expect(platformEnv.Value).To(Equal("rhcos"))

			// Cleanup handled by AfterEach
		})

		It("should set host component paths from KataConfig spec", func() {
			ctx := context.Background()

			testKataConfig = &kataconfigurationv1.KataConfig{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-kataconfig-host-paths",
				},
				Spec: kataconfigurationv1.KataConfigSpec{
					DeploymentMode: kataconfigurationv1.DeploymentModeDaemonSet,
					HostComponentPaths: &kataconfigurationv1.HostComponentPaths{
						QemuPath:      "/usr/libexec/qemu-kvm",
						VirtiofsdPath: "/usr/libexec/virtiofsd",
						KernelPath:    "/boot/vmlinuz-kata",
					},
				},
			}

			Expect(k8sClient.Create(ctx, testKataConfig)).Should(Succeed())

			// Wait for DaemonSet to have the expected QEMU_PATH env var
			// (this ensures we wait for the reconciler to update the DaemonSet)
			var container corev1.Container
			Eventually(func() bool {
				daemonSet := &appsv1.DaemonSet{}
				err := k8sClient.Get(ctx, types.NamespacedName{
					Name:      "kata-deploy",
					Namespace: "openshift-sandboxed-containers-operator",
				}, daemonSet)
				if err != nil || len(daemonSet.Spec.Template.Spec.Containers) == 0 {
					return false
				}
				container = daemonSet.Spec.Template.Spec.Containers[0]
				qemuEnv := getEnvVar(container, "QEMU_PATH")
				return qemuEnv != nil && qemuEnv.Value == "/usr/libexec/qemu-kvm"
			}, timeout, interval).Should(BeTrue(), "QEMU_PATH should be set")

			// Check VIRTIOFSD_PATH
			virtiofsdEnv := getEnvVar(container, "VIRTIOFSD_PATH")
			Expect(virtiofsdEnv).NotTo(BeNil())
			Expect(virtiofsdEnv.Value).To(Equal("/usr/libexec/virtiofsd"))

			// Check KERNEL_PATH
			kernelEnv := getEnvVar(container, "KERNEL_PATH")
			Expect(kernelEnv).NotTo(BeNil())
			Expect(kernelEnv.Value).To(Equal("/boot/vmlinuz-kata"))

			// Cleanup handled by AfterEach
		})

		It("should set BUILD_INITRD based on InitrdConfig", func() {
			ctx := context.Background()

			buildAtRuntime := true
			testKataConfig = &kataconfigurationv1.KataConfig{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-kataconfig-initrd",
				},
				Spec: kataconfigurationv1.KataConfigSpec{
					DeploymentMode: kataconfigurationv1.DeploymentModeDaemonSet,
					InitrdConfig: &kataconfigurationv1.InitrdConfig{
						BuildAtRuntime: &buildAtRuntime,
						PrebuiltPath:   "/opt/kata/share/initrd.img",
					},
				},
			}

			Expect(k8sClient.Create(ctx, testKataConfig)).Should(Succeed())

			// Wait for DaemonSet to have the expected BUILD_INITRD env var
			// (this ensures we wait for the reconciler to update the DaemonSet)
			var container corev1.Container
			Eventually(func() bool {
				daemonSet := &appsv1.DaemonSet{}
				err := k8sClient.Get(ctx, types.NamespacedName{
					Name:      "kata-deploy",
					Namespace: "openshift-sandboxed-containers-operator",
				}, daemonSet)
				if err != nil || len(daemonSet.Spec.Template.Spec.Containers) == 0 {
					return false
				}
				container = daemonSet.Spec.Template.Spec.Containers[0]
				buildInitrdEnv := getEnvVar(container, "BUILD_INITRD")
				return buildInitrdEnv != nil && buildInitrdEnv.Value == "true"
			}, timeout, interval).Should(BeTrue(), "BUILD_INITRD should be set")

			// Check INITRD_PATH
			initrdPathEnv := getEnvVar(container, "INITRD_PATH")
			Expect(initrdPathEnv).NotTo(BeNil())
			Expect(initrdPathEnv.Value).To(Equal("/opt/kata/share/initrd.img"))

			// Cleanup handled by AfterEach
		})
	})

	Context("RuntimeClass Creation", func() {
		It("should create RuntimeClasses for each configured shim", func() {
			ctx := context.Background()

			testKataConfig = &kataconfigurationv1.KataConfig{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-kataconfig-runtimeclass",
				},
				Spec: kataconfigurationv1.KataConfigSpec{
					DeploymentMode: kataconfigurationv1.DeploymentModeDaemonSet,
				},
			}

			Expect(k8sClient.Create(ctx, testKataConfig)).Should(Succeed())

			// Verify RuntimeClasses are created
			expectedClasses := []string{"kata-qemu", "kata-qemu-tdx", "kata-qemu-snp", "kata-qemu-se"}
			for _, className := range expectedClasses {
				rc := &nodev1.RuntimeClass{}
				Eventually(func() bool {
					err := k8sClient.Get(ctx, types.NamespacedName{Name: className}, rc)
					return err == nil
				}, timeout, interval).Should(BeTrue(), "RuntimeClass %s should exist", className)
			}

			// Cleanup handled by AfterEach
		})

		It("should add NFD resources to TDX RuntimeClass overhead", func() {
			ctx := context.Background()

			testKataConfig = &kataconfigurationv1.KataConfig{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-kataconfig-nfd-tdx",
				},
				Spec: kataconfigurationv1.KataConfigSpec{
					DeploymentMode: kataconfigurationv1.DeploymentModeDaemonSet,
				},
			}

			Expect(k8sClient.Create(ctx, testKataConfig)).Should(Succeed())

			// Verify kata-qemu-tdx RuntimeClass has NFD resource
			rc := &nodev1.RuntimeClass{}
			Eventually(func() bool {
				err := k8sClient.Get(ctx, types.NamespacedName{Name: "kata-qemu-tdx"}, rc)
				return err == nil
			}, timeout, interval).Should(BeTrue())

			Expect(rc.Overhead).NotTo(BeNil())
			Expect(rc.Overhead.PodFixed).To(HaveKey(corev1.ResourceName("tdx.intel.com/keys")))

			// Cleanup handled by AfterEach
		})

		It("should add NFD resources to SNP RuntimeClass overhead", func() {
			ctx := context.Background()

			testKataConfig = &kataconfigurationv1.KataConfig{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-kataconfig-nfd-snp",
				},
				Spec: kataconfigurationv1.KataConfigSpec{
					DeploymentMode: kataconfigurationv1.DeploymentModeDaemonSet,
				},
			}

			Expect(k8sClient.Create(ctx, testKataConfig)).Should(Succeed())

			// Verify kata-qemu-snp RuntimeClass has NFD resource
			rc := &nodev1.RuntimeClass{}
			Eventually(func() bool {
				err := k8sClient.Get(ctx, types.NamespacedName{Name: "kata-qemu-snp"}, rc)
				return err == nil
			}, timeout, interval).Should(BeTrue())

			Expect(rc.Overhead).NotTo(BeNil())
			Expect(rc.Overhead.PodFixed).To(HaveKey(corev1.ResourceName("sev-snp.amd.com/esids")))

			// Cleanup handled by AfterEach
		})
	})

	Context("Status Updates", func() {
		It("should update KataConfig status with DaemonSet rollout progress", func() {
			ctx := context.Background()

			testKataConfig = &kataconfigurationv1.KataConfig{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-kataconfig-status-ds",
				},
				Spec: kataconfigurationv1.KataConfigSpec{
					DeploymentMode: kataconfigurationv1.DeploymentModeDaemonSet,
				},
			}

			Expect(k8sClient.Create(ctx, testKataConfig)).Should(Succeed())

			// Wait for status to be updated with DaemonSetStatus
			Eventually(func() bool {
				kc := &kataconfigurationv1.KataConfig{}
				err := k8sClient.Get(ctx, types.NamespacedName{Name: testKataConfig.Name}, kc)
				if err != nil {
					return false
				}
				return kc.Status.DaemonSetStatus != nil
			}, timeout, interval).Should(BeTrue())

			// Verify DaemonSetStatus fields are populated
			kc := &kataconfigurationv1.KataConfig{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: testKataConfig.Name}, kc)).Should(Succeed())
			Expect(kc.Status.DaemonSetStatus).NotTo(BeNil())

			// Cleanup handled by AfterEach
		})

		It("should set ActiveDeploymentMode in status", func() {
			ctx := context.Background()

			testKataConfig = &kataconfigurationv1.KataConfig{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-kataconfig-active-mode",
				},
				Spec: kataconfigurationv1.KataConfigSpec{
					DeploymentMode: kataconfigurationv1.DeploymentModeDaemonSet,
				},
			}

			Expect(k8sClient.Create(ctx, testKataConfig)).Should(Succeed())

			// Wait for status to be updated with ActiveDeploymentMode
			Eventually(func() bool {
				kc := &kataconfigurationv1.KataConfig{}
				err := k8sClient.Get(ctx, types.NamespacedName{Name: testKataConfig.Name}, kc)
				if err != nil {
					return false
				}
				return kc.Status.ActiveDeploymentMode == kataconfigurationv1.DeploymentModeDaemonSet
			}, timeout, interval).Should(BeTrue())

			// Cleanup handled by AfterEach
		})
	})

	Context("Cleanup and Deletion", func() {
		It("should run cleanup before deleting KataConfig", func() {
			Skip("Requires DaemonSet controller implementation - TDD placeholder")

			ctx := context.Background()

			// Create KataConfig
			kataConfig := &kataconfigurationv1.KataConfig{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-kataconfig-cleanup",
				},
			}

			Expect(k8sClient.Create(ctx, kataConfig)).Should(Succeed())

			// Wait for DaemonSet to be created
			daemonSet := &appsv1.DaemonSet{}
			Eventually(func() bool {
				err := k8sClient.Get(ctx, types.NamespacedName{
					Name:      "kata-deploy",
					Namespace: "openshift-sandboxed-containers-operator",
				}, daemonSet)
				return err == nil
			}, timeout, interval).Should(BeTrue())

			// Delete KataConfig
			Expect(k8sClient.Delete(ctx, kataConfig)).Should(Succeed())

			// Verify DaemonSet command changes to cleanup
			Eventually(func() bool {
				err := k8sClient.Get(ctx, types.NamespacedName{
					Name:      "kata-deploy",
					Namespace: "openshift-sandboxed-containers-operator",
				}, daemonSet)
				if err != nil {
					return false
				}
				container := daemonSet.Spec.Template.Spec.Containers[0]
				for _, arg := range container.Command {
					if arg == "cleanup" {
						return true
					}
				}
				return false
			}, timeout, interval).Should(BeTrue())

			// Eventually DaemonSet should be deleted after cleanup
			Eventually(func() bool {
				err := k8sClient.Get(ctx, types.NamespacedName{
					Name:      "kata-deploy",
					Namespace: "openshift-sandboxed-containers-operator",
				}, daemonSet)
				return errors.IsNotFound(err)
			}, timeout*2, interval).Should(BeTrue())
		})

		It("should delete RuntimeClasses when KataConfig is deleted", func() {
			Skip("Requires DaemonSet controller implementation - TDD placeholder")

			ctx := context.Background()

			kataConfig := &kataconfigurationv1.KataConfig{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-kataconfig-rc-cleanup",
				},
			}

			Expect(k8sClient.Create(ctx, kataConfig)).Should(Succeed())

			// Wait for RuntimeClasses to be created
			rc := &nodev1.RuntimeClass{}
			Eventually(func() bool {
				err := k8sClient.Get(ctx, types.NamespacedName{Name: "kata-qemu"}, rc)
				return err == nil
			}, timeout, interval).Should(BeTrue())

			// Delete KataConfig
			Expect(k8sClient.Delete(ctx, kataConfig)).Should(Succeed())

			// Verify RuntimeClasses are deleted
			Eventually(func() bool {
				err := k8sClient.Get(ctx, types.NamespacedName{Name: "kata-qemu"}, rc)
				return errors.IsNotFound(err)
			}, timeout*2, interval).Should(BeTrue())
		})
	})

	Context("Update Strategy", func() {
		It("should use RollingUpdate strategy with 10% maxUnavailable", func() {
			ctx := context.Background()

			testKataConfig = &kataconfigurationv1.KataConfig{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-kataconfig-update-strategy",
				},
				Spec: kataconfigurationv1.KataConfigSpec{
					DeploymentMode: kataconfigurationv1.DeploymentModeDaemonSet,
				},
			}

			Expect(k8sClient.Create(ctx, testKataConfig)).Should(Succeed())

			// Verify DaemonSet update strategy
			daemonSet := &appsv1.DaemonSet{}
			Eventually(func() bool {
				err := k8sClient.Get(ctx, types.NamespacedName{
					Name:      "kata-deploy",
					Namespace: "openshift-sandboxed-containers-operator",
				}, daemonSet)
				return err == nil
			}, timeout, interval).Should(BeTrue())

			Expect(daemonSet.Spec.UpdateStrategy.Type).To(Equal(appsv1.RollingUpdateDaemonSetStrategyType))
			Expect(daemonSet.Spec.UpdateStrategy.RollingUpdate).NotTo(BeNil())
			Expect(daemonSet.Spec.UpdateStrategy.RollingUpdate.MaxUnavailable.StrVal).To(Equal("10%"))

			// Cleanup handled by AfterEach
		})
	})
})

// Helper functions for tests

func createTestKataConfig(ctx context.Context, name string) *kataconfigurationv1.KataConfig {
	kataConfig := &kataconfigurationv1.KataConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
	}
	Expect(k8sClient.Create(ctx, kataConfig)).Should(Succeed())
	return kataConfig
}

func deleteTestKataConfig(ctx context.Context, kataConfig *kataconfigurationv1.KataConfig) {
	err := k8sClient.Delete(ctx, kataConfig)
	if err != nil && !errors.IsNotFound(err) {
		Expect(err).NotTo(HaveOccurred())
	}
}

func waitForDaemonSet(ctx context.Context, name, namespace string, timeout, interval time.Duration) *appsv1.DaemonSet {
	daemonSet := &appsv1.DaemonSet{}
	Eventually(func() bool {
		err := k8sClient.Get(ctx, types.NamespacedName{
			Name:      name,
			Namespace: namespace,
		}, daemonSet)
		return err == nil
	}, timeout, interval).Should(BeTrue())
	return daemonSet
}

func waitForRuntimeClass(ctx context.Context, name string, timeout, interval time.Duration) *nodev1.RuntimeClass {
	rc := &nodev1.RuntimeClass{}
	Eventually(func() bool {
		err := k8sClient.Get(ctx, types.NamespacedName{Name: name}, rc)
		return err == nil
	}, timeout, interval).Should(BeTrue())
	return rc
}

func getEnvVar(container corev1.Container, name string) *corev1.EnvVar {
	for i := range container.Env {
		if container.Env[i].Name == name {
			return &container.Env[i]
		}
	}
	return nil
}

// Ensure client is available for helper functions
var _ client.Client = k8sClient
