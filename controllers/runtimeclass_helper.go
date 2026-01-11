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

	corev1 "k8s.io/api/core/v1"
	nodeapi "k8s.io/api/node/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	kataconfigurationv1 "github.com/openshift/sandboxed-containers-operator/api/v1"
)

// RuntimeClassConfig holds the configuration for creating a RuntimeClass
type RuntimeClassConfig struct {
	// Name is the name of the RuntimeClass
	Name string
	// Handler is the runtime handler (e.g., "kata-qemu", "kata-qemu-tdx")
	Handler string
	// CPUOverhead is the CPU overhead for pods using this RuntimeClass
	CPUOverhead string
	// MemoryOverhead is the memory overhead for pods using this RuntimeClass
	MemoryOverhead string
	// ExtendedResources is a map of extended resources to add to the overhead
	// (e.g., "tdx.intel.com/keys": "1" for TDX, "sev-snp.amd.com/esids": "1" for SNP)
	ExtendedResources map[corev1.ResourceName]resource.Quantity
	// NodeSelector is the node selector for scheduling pods
	NodeSelector map[string]string
}

// RuntimeClassHelper provides common RuntimeClass operations for reconcilers
type RuntimeClassHelper struct {
	Client client.Client
	Scheme *runtime.Scheme
}

// NewRuntimeClassHelper creates a new RuntimeClassHelper
func NewRuntimeClassHelper(c client.Client, scheme *runtime.Scheme) *RuntimeClassHelper {
	return &RuntimeClassHelper{
		Client: c,
		Scheme: scheme,
	}
}

// CreateOrUpdateRuntimeClass creates or updates a RuntimeClass based on the provided config.
// It sets the KataConfig as owner and adds the standard finalizer for cleanup.
func (h *RuntimeClassHelper) CreateOrUpdateRuntimeClass(
	ctx context.Context,
	kataConfig *kataconfigurationv1.KataConfig,
	config RuntimeClassConfig,
) error {
	// Build pod overhead
	podFixed := corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse(config.CPUOverhead),
		corev1.ResourceMemory: resource.MustParse(config.MemoryOverhead),
	}

	// Add extended resources (NFD resources for TDX, SNP, etc.)
	for k, v := range config.ExtendedResources {
		podFixed[k] = v
	}

	rc := &nodeapi.RuntimeClass{
		ObjectMeta: metav1.ObjectMeta{
			Name:       config.Name,
			Finalizers: []string{runtimeClassFinalizerName},
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "sandboxed-containers-operator",
			},
		},
		Handler: config.Handler,
		Overhead: &nodeapi.Overhead{
			PodFixed: podFixed,
		},
	}

	if len(config.NodeSelector) > 0 {
		rc.Scheduling = &nodeapi.Scheduling{
			NodeSelector: config.NodeSelector,
		}
	}

	// Set KataConfig as owner
	if err := controllerutil.SetControllerReference(kataConfig, rc, h.Scheme); err != nil {
		return err
	}

	// Check if RuntimeClass exists
	foundRc := &nodeapi.RuntimeClass{}
	err := h.Client.Get(ctx, types.NamespacedName{Name: rc.Name}, foundRc)
	if err != nil {
		if !k8serrors.IsNotFound(err) {
			return err
		}
		// Create new RuntimeClass
		return h.Client.Create(ctx, rc)
	}

	// Update existing RuntimeClass
	foundRc.Handler = rc.Handler
	foundRc.Overhead = rc.Overhead
	foundRc.Scheduling = rc.Scheduling
	// Ensure finalizer is present
	if !controllerutil.ContainsFinalizer(foundRc, runtimeClassFinalizerName) {
		controllerutil.AddFinalizer(foundRc, runtimeClassFinalizerName)
	}
	return h.Client.Update(ctx, foundRc)
}

// DeleteRuntimeClass deletes a RuntimeClass by name
func (h *RuntimeClassHelper) DeleteRuntimeClass(ctx context.Context, name string) error {
	rc := &nodeapi.RuntimeClass{}
	err := h.Client.Get(ctx, types.NamespacedName{Name: name}, rc)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			return nil
		}
		return err
	}

	return h.Client.Delete(ctx, rc)
}

// UpdateKataConfigRuntimeClasses updates the KataConfig status with the list of runtime classes
func (h *RuntimeClassHelper) UpdateKataConfigRuntimeClasses(
	kataConfig *kataconfigurationv1.KataConfig,
	runtimeClassName string,
) {
	if !contains(kataConfig.Status.RuntimeClasses, runtimeClassName) {
		kataConfig.Status.RuntimeClasses = append(kataConfig.Status.RuntimeClasses, runtimeClassName)
	}
}

// Default overhead values used across the operator
const (
	DefaultCPUOverhead    = "250m"
	DefaultMemoryOverhead = "350Mi"
)

// KataRuntimeClassConfigs returns the standard RuntimeClass configurations for Kata
func KataRuntimeClassConfigs(nodeSelector map[string]string) []RuntimeClassConfig {
	return []RuntimeClassConfig{
		{
			Name:           "kata-qemu",
			Handler:        "kata-qemu",
			CPUOverhead:    DefaultCPUOverhead,
			MemoryOverhead: DefaultMemoryOverhead,
			NodeSelector:   nodeSelector,
		},
		{
			Name:           "kata-qemu-tdx",
			Handler:        "kata-qemu-tdx",
			CPUOverhead:    DefaultCPUOverhead,
			MemoryOverhead: DefaultMemoryOverhead,
			ExtendedResources: map[corev1.ResourceName]resource.Quantity{
				"tdx.intel.com/keys": resource.MustParse("1"),
			},
			NodeSelector: nodeSelector,
		},
		{
			Name:           "kata-qemu-snp",
			Handler:        "kata-qemu-snp",
			CPUOverhead:    DefaultCPUOverhead,
			MemoryOverhead: DefaultMemoryOverhead,
			ExtendedResources: map[corev1.ResourceName]resource.Quantity{
				"sev-snp.amd.com/esids": resource.MustParse("1"),
			},
			NodeSelector: nodeSelector,
		},
		{
			Name:           "kata-qemu-se",
			Handler:        "kata-qemu-se",
			CPUOverhead:    DefaultCPUOverhead,
			MemoryOverhead: DefaultMemoryOverhead,
			NodeSelector:   nodeSelector,
		},
	}
}
