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
// resolveDeploymentMode maps a feature-gate option and MachineConfig
// availability to the concrete DeploymentMode. This is a pure function
// so it can be tested and reused without hitting the API server.
func resolveDeploymentMode(mode DeploymentModeOption, machineConfigAvailable bool) (DeploymentMode, error) {
	switch mode {
	case MachineConfigOption:
		if !machineConfigAvailable {
			return 0, fmt.Errorf("deployment mode is set to MachineConfig, but it's not available")
		}
		return MachineConfigMode, nil
	case DaemonSetOption:
		return DaemonSetMode, nil
	case KataDeployOption:
		return KataDeployMode, nil
	case DaemonSetFallbackOption:
		if machineConfigAvailable {
			return MachineConfigMode, nil
		}
		return DaemonSetMode, nil
	case KataDeployFallbackOption:
		if machineConfigAvailable {
			return MachineConfigMode, nil
		}
		return KataDeployMode, nil
	}
	return 0, fmt.Errorf("unknown deployment mode %q", mode)
}

func (r *KataConfigOpenShiftReconciler) handleDeploymentModeFeature(mode DeploymentModeOption) error {
	r.Log.Info("Feature gate", "featuregate", DeploymentModeConfig, "state", mode)

	machineConfigAvailable, err := r.isMachineConfigAvailable()
	if err != nil {
		r.Log.Info("Error checking if MachineConfig is available")
		return err
	}

	resolved, err := resolveDeploymentMode(mode, machineConfigAvailable)
	if err != nil {
		return err
	}

	r.Log.Info("Deployment mode resolved", "mode", resolved)
	r.DeploymentMode = resolved

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
