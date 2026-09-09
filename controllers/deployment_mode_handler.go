package controllers

import (
	"context"
	"fmt"
	"time"

	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type DeploymentMode int

const (
	MachineConfigMode DeploymentMode = iota
	DaemonSetMode
	KataDeployMode
)

type DeploymentModeOption string

const (
	MachineConfigOption      DeploymentModeOption = "MachineConfig"
	DaemonSetOption          DeploymentModeOption = "DaemonSet"
	DaemonSetFallbackOption  DeploymentModeOption = "DaemonSetFallback"
	KataDeployOption         DeploymentModeOption = "KataDeploy"
	KataDeployFallbackOption DeploymentModeOption = "KataDeployFallback"
)

const (
	machineConfigGroup    = "machineconfiguration.openshift.io"
	machineConfigVersion  = "v1"
	machineConfigKind     = "MachineConfig"
	machineConfigPoolKind = "MachineConfigPool"
)

func ParseDeploymentModeOption(s string) (DeploymentModeOption, error) {
	switch DeploymentModeOption(s) {
	case MachineConfigOption, DaemonSetOption, DaemonSetFallbackOption,
		KataDeployOption, KataDeployFallbackOption:
		return DeploymentModeOption(s), nil
	default:
		return "", fmt.Errorf("invalid DeploymentMode: %q", s)
	}
}

func (d DeploymentModeOption) String() string {
	return string(d)
}

// Process the DeploymentMode feature gate (FG).
// This method is invoked by the reconcile loop at its initiation.
// It examines the current state of the FeatureGate and adjusts the deployment mode (DaemonSet or MachineConfig)
// based on the selected DeploymentModeOption and the availability of the MachineConfig Add-on.
//
// The behavior is as follows:
//   - If the mode is MachineConfigOption, the deployment mode is set to MachineConfig.
//   - If the mode is DaemonSetOption, the deployment mode is forcibly set to DaemonSet, regardless of MachineConfig availability.
//   - If the mode is DaemonSetFallbackOption, the deployment mode is set to DaemonSet only if the MachineConfig Add-on is unavailable.
//     Otherwise, it defaults to MachineConfig.
func (r *KataConfigOpenShiftReconciler) handleDeploymentModeFeature(mode DeploymentModeOption) error {
	r.Log.Info("Feature gate", "featuregate", DeploymentModeConfig, "state", mode)

	machineConfigAvailable, err := r.isMachineConfigAvailable()
	if err != nil {
		r.Log.Info("Error checking if MachineConfig is available")
		return err
	}

	if mode == MachineConfigOption {
		if !machineConfigAvailable {
			return fmt.Errorf("deployment mode is set to MachineConfig, but it's not available")
		}

		r.Log.Info("Deployment mode will be set to MachineConfig")
		r.DeploymentMode = MachineConfigMode
		return nil
	}

	if mode == DaemonSetOption {
		r.Log.Info("Deployment mode will be set to DaemonSet")
		r.DeploymentMode = DaemonSetMode
		return nil
	}

	if mode == KataDeployOption {
		r.Log.Info("Deployment mode will be set to KataDeploy")
		r.DeploymentMode = KataDeployMode
		return nil
	}

	if mode == KataDeployFallbackOption {
		if machineConfigAvailable {
			r.Log.Info("Deployment mode will be set to MachineConfig")
			r.DeploymentMode = MachineConfigMode
		} else {
			r.Log.Info("MachineConfig is not available, deployment mode will be set to KataDeploy")
			r.DeploymentMode = KataDeployMode
		}
		return nil
	}

	if mode == DaemonSetFallbackOption && !machineConfigAvailable {
		r.Log.Info("MachineConfig is not available, deployment mode will be set to DaemonSet")
		r.DeploymentMode = DaemonSetMode
	} else {
		r.Log.Info("Deployment mode will be set to MachineConfig")
		r.DeploymentMode = MachineConfigMode
	}

	return nil
}

// isMachineConfigAvailable checks if MachineConfig CRD is available.
//
// It returns a boolean indicating availability and an error if any.
func (r *KataConfigOpenShiftReconciler) isMachineConfigAvailable() (bool, error) {
	machineConfig := &unstructured.Unstructured{}
	machineConfig.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   machineConfigGroup,
		Version: machineConfigVersion,
		Kind:    machineConfigKind,
	})

	// Attempt to GET any MachineConfig to verify if the GVK is known.
	// If the CRD isn’t installed, it will return a NoMatchError.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := r.Client.Get(ctx, client.ObjectKey{Name: "dummy-machine-config"}, machineConfig)
	if err == nil || k8serrors.IsNotFound(err) {
		r.Log.Info("MachineConfig CRD is present")
		return true, nil
	}

	if meta.IsNoMatchError(err) {
		r.Log.Info("MachineConfig CRD not found")
		return false, nil
	}

	return false, err
}

// isMachineConfigPoolAvailable reports whether the MachineConfigPool API is registered.
// Hosted control plane guest clusters often omit MCP while still exposing other MCO types;
// an unconditional MCP watch prevents the operator manager from starting (see KATA-4840).
func (r *KataConfigOpenShiftReconciler) isMachineConfigPoolAvailable() (bool, error) {
	mcp := &unstructured.Unstructured{}
	mcp.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   machineConfigGroup,
		Version: machineConfigVersion,
		Kind:    machineConfigPoolKind,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := r.Client.Get(ctx, client.ObjectKey{Name: "dummy-machine-config-pool"}, mcp)
	if err == nil || k8serrors.IsNotFound(err) {
		r.Log.Info("MachineConfigPool CRD is present")
		return true, nil
	}

	if meta.IsNoMatchError(err) {
		r.Log.Info("MachineConfigPool CRD not found")
		return false, nil
	}

	return false, err
}
