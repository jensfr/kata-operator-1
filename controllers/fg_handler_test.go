package controllers

import (
	"context"
	"strconv"
	"testing"

	mcfgv1 "github.com/openshift/api/machineconfiguration/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type hcpFeatureGateClient struct {
	client.Client
	machineConfigLists int
}

func (c *hcpFeatureGateClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if u, ok := obj.(*unstructured.Unstructured); ok && u.GetAPIVersion() == "machineconfiguration.openshift.io/v1" {
		return &meta.NoKindMatchError{GroupKind: u.GroupVersionKind().GroupKind(), SearchedVersions: []string{"v1"}}
	}
	return c.Client.Get(ctx, key, obj, opts...)
}

func (c *hcpFeatureGateClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if _, ok := list.(*mcfgv1.MachineConfigList); ok {
		c.machineConfigLists++
		return &meta.NoKindMatchError{
			GroupKind:        schema.GroupKind{Group: machineConfigGroup, Kind: machineConfigKind},
			SearchedVersions: []string{machineConfigVersion},
		}
	}
	return c.Client.List(ctx, list, opts...)
}

func TestKataDeployFeatureGatesWithoutMachineConfigAPI(t *testing.T) {
	for _, mode := range []DeploymentModeOption{KataDeployOption, KataDeployFallbackOption} {
		for _, layered := range []bool{false, true} {
			t.Run(string(mode)+"/layered="+strconv.FormatBool(layered), func(t *testing.T) {
				r := newDispatcherTestReconciler(&corev1.ConfigMap{
					ObjectMeta: metav1.ObjectMeta{Name: FgConfigMapName, Namespace: OperatorNamespace},
					Data: map[string]string{
						DeploymentModeConfig:   string(mode),
						LayeredImageDeployment: strconv.FormatBool(layered),
					},
				})
				c := &hcpFeatureGateClient{Client: r.Client}
				r.Client = c
				r.ImgMc = &mcfgv1.MachineConfig{ObjectMeta: metav1.ObjectMeta{Name: image_mc_name}}
				if err := r.processFeatureGates(); err != nil {
					t.Fatalf("HCP feature gates must not require MachineConfig objects: %v", err)
				}
				if r.DeploymentMode != KataDeployMode || c.machineConfigLists != 0 || r.ImgMc != nil {
					t.Fatalf("unexpected HCP feature gate state: mode=%v lists=%d image=%v", r.DeploymentMode, c.machineConfigLists, r.ImgMc)
				}
			})
		}
	}
}
