package provision

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

const guestPodLimitKubeletConfigName = "sno-max-pods"

var guestKubeletConfigGVR = schema.GroupVersionResource{
	Group: "machineconfiguration.openshift.io", Version: "v1", Resource: "kubeletconfigs",
}

// guestMaxPodsForCores maps the SNO CPU tiers used by LWE to their pod limits.
func guestMaxPodsForCores(cores, override int32) int64 {
	if override > 0 {
		return int64(override)
	}
	switch {
	case cores <= 16:
		return 500
	case cores <= 32:
		return 1000
	default:
		return 2000
	}
}

func guestMaxPodsKubeletConfig(cores, override int32, uid string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": guestKubeletConfigGVR.GroupVersion().String(),
		"kind":       "KubeletConfig",
		"metadata": map[string]interface{}{
			"name": guestPodLimitKubeletConfigName,
			"labels": map[string]interface{}{
				"app.kubernetes.io/managed-by": "sno-vm-operator",
				"sno.llm-d.io/instance-uid":    uid,
			},
		},
		"spec": map[string]interface{}{
			"machineConfigPoolSelector": map[string]interface{}{
				"matchLabels": map[string]interface{}{
					"pools.operator.machineconfiguration.openshift.io/master": "",
				},
			},
			"kubeletConfig": map[string]interface{}{
				"maxPods":     guestMaxPodsForCores(cores, override),
				"podsPerCore": int64(0),
			},
		},
	}}
}

// ensureGuestMaxPods configures the single SNO master pool and waits until the
// Machine Config Operator has applied the setting to the node.
func ensureGuestMaxPods(ctx context.Context, guest dynamic.Interface, cores, override int32, node *corev1.Node, uid string) (bool, string, string, error) {
	expected := guestMaxPodsKubeletConfig(cores, override, uid)
	config, err := ensureGuestResource(ctx, guest, guestKubeletConfigGVR, "", expected)
	if apierrors.IsNotFound(err) {
		return false, "GuestPodLimitPending", "Waiting for the guest KubeletConfig API", nil
	}
	if err != nil {
		return false, "GuestPodLimitConflict", "The guest max-pods KubeletConfig conflicts with existing configuration", err
	}

	conditions, _, _ := unstructured.NestedSlice(config.Object, "status", "conditions")
	succeeded := false
	for _, item := range conditions {
		condition, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		typ, _, _ := unstructured.NestedString(condition, "type")
		status, _, _ := unstructured.NestedString(condition, "status")
		message, _, _ := unstructured.NestedString(condition, "message")
		if typ == "Failure" && status == string(corev1.ConditionTrue) {
			if message == "" {
				message = "The Machine Config Operator failed to apply the guest max-pods setting"
			}
			return false, "GuestPodLimitFailed", message, fmt.Errorf("KubeletConfig failed: %s", message)
		}
		if typ == "Success" && status == string(corev1.ConditionTrue) {
			succeeded = true
		}
	}
	if !succeeded {
		return false, "GuestPodLimitPending", "Waiting for the Machine Config Operator to apply the guest max-pods setting", nil
	}

	want := guestMaxPodsForCores(cores, override)
	allocatedPods := node.Status.Allocatable[corev1.ResourcePods]
	got := allocatedPods.Value()
	if got != want {
		return false, "GuestPodLimitPending", fmt.Sprintf("Waiting for the guest node pod capacity to reach %d (currently %d)", want, got), nil
	}
	return true, "GuestPodLimitReady", fmt.Sprintf("Guest node pod capacity is set to %d", want), nil
}
