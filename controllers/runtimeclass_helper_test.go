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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	kataconfigurationv1 "github.com/openshift/sandboxed-containers-operator/api/v1"
)

var _ = Describe("RuntimeClassHelper", func() {
	Context("KataRuntimeClassConfigs", func() {
		It("should return four RuntimeClass configurations", func() {
			nodeSelector := map[string]string{
				"katacontainers.io/kata-runtime": "true",
			}
			configs := KataRuntimeClassConfigs(nodeSelector)

			Expect(configs).To(HaveLen(4))
		})

		It("should return correct configuration for kata-qemu", func() {
			nodeSelector := map[string]string{
				"katacontainers.io/kata-runtime": "true",
			}
			configs := KataRuntimeClassConfigs(nodeSelector)

			var kataQemu *RuntimeClassConfig
			for i := range configs {
				if configs[i].Name == "kata-qemu" {
					kataQemu = &configs[i]
					break
				}
			}

			Expect(kataQemu).NotTo(BeNil())
			Expect(kataQemu.Handler).To(Equal("kata-qemu"))
			Expect(kataQemu.CPUOverhead).To(Equal(DefaultCPUOverhead))
			Expect(kataQemu.MemoryOverhead).To(Equal(DefaultMemoryOverhead))
			Expect(kataQemu.ExtendedResources).To(BeEmpty())
			Expect(kataQemu.NodeSelector).To(HaveKeyWithValue("katacontainers.io/kata-runtime", "true"))
		})

		It("should return correct configuration for kata-qemu-tdx with NFD resource", func() {
			nodeSelector := map[string]string{
				"katacontainers.io/kata-runtime": "true",
			}
			configs := KataRuntimeClassConfigs(nodeSelector)

			var kataTdx *RuntimeClassConfig
			for i := range configs {
				if configs[i].Name == "kata-qemu-tdx" {
					kataTdx = &configs[i]
					break
				}
			}

			Expect(kataTdx).NotTo(BeNil())
			Expect(kataTdx.Handler).To(Equal("kata-qemu-tdx"))
			Expect(kataTdx.ExtendedResources).To(HaveKey(corev1.ResourceName("tdx.intel.com/keys")))
			Expect(kataTdx.ExtendedResources[corev1.ResourceName("tdx.intel.com/keys")]).To(Equal(resource.MustParse("1")))
		})

		It("should return correct configuration for kata-qemu-snp with NFD resource", func() {
			nodeSelector := map[string]string{
				"katacontainers.io/kata-runtime": "true",
			}
			configs := KataRuntimeClassConfigs(nodeSelector)

			var kataSnp *RuntimeClassConfig
			for i := range configs {
				if configs[i].Name == "kata-qemu-snp" {
					kataSnp = &configs[i]
					break
				}
			}

			Expect(kataSnp).NotTo(BeNil())
			Expect(kataSnp.Handler).To(Equal("kata-qemu-snp"))
			Expect(kataSnp.ExtendedResources).To(HaveKey(corev1.ResourceName("sev-snp.amd.com/esids")))
			Expect(kataSnp.ExtendedResources[corev1.ResourceName("sev-snp.amd.com/esids")]).To(Equal(resource.MustParse("1")))
		})

		It("should return correct configuration for kata-qemu-se", func() {
			nodeSelector := map[string]string{
				"katacontainers.io/kata-runtime": "true",
			}
			configs := KataRuntimeClassConfigs(nodeSelector)

			var kataSe *RuntimeClassConfig
			for i := range configs {
				if configs[i].Name == "kata-qemu-se" {
					kataSe = &configs[i]
					break
				}
			}

			Expect(kataSe).NotTo(BeNil())
			Expect(kataSe.Handler).To(Equal("kata-qemu-se"))
			Expect(kataSe.ExtendedResources).To(BeEmpty())
		})

		It("should use provided nodeSelector for all configurations", func() {
			customNodeSelector := map[string]string{
				"custom-label": "custom-value",
				"another":      "label",
			}
			configs := KataRuntimeClassConfigs(customNodeSelector)

			for _, config := range configs {
				Expect(config.NodeSelector).To(Equal(customNodeSelector))
			}
		})
	})

	Context("RuntimeClassHelper CreateOrUpdateRuntimeClass", func() {
		It("should create a RuntimeClass when it does not exist", func() {
			Skip("Requires envtest setup - integration test")

			ctx := context.Background()
			helper := NewRuntimeClassHelper(k8sClient, k8sManager.GetScheme())

			kataConfig := &kataconfigurationv1.KataConfig{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-kataconfig-helper-create",
				},
			}
			Expect(k8sClient.Create(ctx, kataConfig)).Should(Succeed())

			config := RuntimeClassConfig{
				Name:           "test-kata-qemu",
				Handler:        "kata-qemu",
				CPUOverhead:    DefaultCPUOverhead,
				MemoryOverhead: DefaultMemoryOverhead,
				NodeSelector: map[string]string{
					"katacontainers.io/kata-runtime": "true",
				},
			}

			err := helper.CreateOrUpdateRuntimeClass(ctx, kataConfig, config)
			Expect(err).NotTo(HaveOccurred())

			// Verify RuntimeClass was created
			rc := &nodev1.RuntimeClass{}
			err = k8sClient.Get(ctx, types.NamespacedName{Name: "test-kata-qemu"}, rc)
			Expect(err).NotTo(HaveOccurred())
			Expect(rc.Handler).To(Equal("kata-qemu"))
			Expect(rc.Overhead.PodFixed[corev1.ResourceCPU]).To(Equal(resource.MustParse(DefaultCPUOverhead)))
			Expect(rc.Overhead.PodFixed[corev1.ResourceMemory]).To(Equal(resource.MustParse(DefaultMemoryOverhead)))

			// Cleanup
			Expect(k8sClient.Delete(ctx, kataConfig)).Should(Succeed())
			Expect(k8sClient.Delete(ctx, rc)).Should(Succeed())
		})

		It("should update an existing RuntimeClass", func() {
			Skip("Requires envtest setup - integration test")

			ctx := context.Background()
			helper := NewRuntimeClassHelper(k8sClient, k8sManager.GetScheme())

			kataConfig := &kataconfigurationv1.KataConfig{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-kataconfig-helper-update",
				},
			}
			Expect(k8sClient.Create(ctx, kataConfig)).Should(Succeed())

			// Create initial RuntimeClass
			config := RuntimeClassConfig{
				Name:           "test-kata-qemu-update",
				Handler:        "kata-qemu",
				CPUOverhead:    "100m",
				MemoryOverhead: "200Mi",
				NodeSelector: map[string]string{
					"katacontainers.io/kata-runtime": "true",
				},
			}

			err := helper.CreateOrUpdateRuntimeClass(ctx, kataConfig, config)
			Expect(err).NotTo(HaveOccurred())

			// Update with new overhead values
			config.CPUOverhead = "300m"
			config.MemoryOverhead = "400Mi"

			err = helper.CreateOrUpdateRuntimeClass(ctx, kataConfig, config)
			Expect(err).NotTo(HaveOccurred())

			// Verify RuntimeClass was updated
			rc := &nodev1.RuntimeClass{}
			err = k8sClient.Get(ctx, types.NamespacedName{Name: "test-kata-qemu-update"}, rc)
			Expect(err).NotTo(HaveOccurred())
			Expect(rc.Overhead.PodFixed[corev1.ResourceCPU]).To(Equal(resource.MustParse("300m")))
			Expect(rc.Overhead.PodFixed[corev1.ResourceMemory]).To(Equal(resource.MustParse("400Mi")))

			// Cleanup
			Expect(k8sClient.Delete(ctx, kataConfig)).Should(Succeed())
			Expect(k8sClient.Delete(ctx, rc)).Should(Succeed())
		})

		It("should include extended resources in overhead", func() {
			Skip("Requires envtest setup - integration test")

			ctx := context.Background()
			helper := NewRuntimeClassHelper(k8sClient, k8sManager.GetScheme())

			kataConfig := &kataconfigurationv1.KataConfig{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-kataconfig-helper-nfd",
				},
			}
			Expect(k8sClient.Create(ctx, kataConfig)).Should(Succeed())

			config := RuntimeClassConfig{
				Name:           "test-kata-qemu-tdx",
				Handler:        "kata-qemu-tdx",
				CPUOverhead:    DefaultCPUOverhead,
				MemoryOverhead: DefaultMemoryOverhead,
				ExtendedResources: map[corev1.ResourceName]resource.Quantity{
					"tdx.intel.com/keys": resource.MustParse("1"),
				},
				NodeSelector: map[string]string{
					"katacontainers.io/kata-runtime": "true",
				},
			}

			err := helper.CreateOrUpdateRuntimeClass(ctx, kataConfig, config)
			Expect(err).NotTo(HaveOccurred())

			// Verify RuntimeClass has NFD resource in overhead
			rc := &nodev1.RuntimeClass{}
			err = k8sClient.Get(ctx, types.NamespacedName{Name: "test-kata-qemu-tdx"}, rc)
			Expect(err).NotTo(HaveOccurred())
			Expect(rc.Overhead.PodFixed).To(HaveKey(corev1.ResourceName("tdx.intel.com/keys")))

			// Cleanup
			Expect(k8sClient.Delete(ctx, kataConfig)).Should(Succeed())
			Expect(k8sClient.Delete(ctx, rc)).Should(Succeed())
		})
	})

	Context("RuntimeClassHelper UpdateKataConfigRuntimeClasses", func() {
		It("should add new RuntimeClass to status", func() {
			helper := NewRuntimeClassHelper(nil, nil)

			kataConfig := &kataconfigurationv1.KataConfig{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-kataconfig",
				},
				Status: kataconfigurationv1.KataConfigStatus{
					RuntimeClasses: []string{},
				},
			}

			helper.UpdateKataConfigRuntimeClasses(kataConfig, "kata-qemu")

			Expect(kataConfig.Status.RuntimeClasses).To(ContainElement("kata-qemu"))
		})

		It("should not add duplicate RuntimeClass to status", func() {
			helper := NewRuntimeClassHelper(nil, nil)

			kataConfig := &kataconfigurationv1.KataConfig{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-kataconfig",
				},
				Status: kataconfigurationv1.KataConfigStatus{
					RuntimeClasses: []string{"kata-qemu"},
				},
			}

			helper.UpdateKataConfigRuntimeClasses(kataConfig, "kata-qemu")

			Expect(kataConfig.Status.RuntimeClasses).To(HaveLen(1))
			Expect(kataConfig.Status.RuntimeClasses).To(ContainElement("kata-qemu"))
		})

		It("should preserve existing RuntimeClasses when adding new one", func() {
			helper := NewRuntimeClassHelper(nil, nil)

			kataConfig := &kataconfigurationv1.KataConfig{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-kataconfig",
				},
				Status: kataconfigurationv1.KataConfigStatus{
					RuntimeClasses: []string{"kata-qemu"},
				},
			}

			helper.UpdateKataConfigRuntimeClasses(kataConfig, "kata-qemu-tdx")

			Expect(kataConfig.Status.RuntimeClasses).To(HaveLen(2))
			Expect(kataConfig.Status.RuntimeClasses).To(ContainElements("kata-qemu", "kata-qemu-tdx"))
		})
	})

	Context("Default overhead values", func() {
		It("should have correct default CPU overhead", func() {
			Expect(DefaultCPUOverhead).To(Equal("250m"))
		})

		It("should have correct default memory overhead", func() {
			Expect(DefaultMemoryOverhead).To(Equal("350Mi"))
		})
	})
})
