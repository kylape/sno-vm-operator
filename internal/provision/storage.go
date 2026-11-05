package provision

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

const (
	guestStorageNamespace     = "openshift-lvm-storage"
	guestLVMSSubscriptionName = "lvms"
	guestLVMSCluster          = "sno-workloads"
	guestLVMSDeviceClass      = "workloads"
	guestLVMSStorageClass     = "lvms-workloads"
)

var (
	guestNamespaceGVR     = schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}
	guestOperatorGroupGVR = schema.GroupVersionResource{Group: "operators.coreos.com", Version: "v1", Resource: "operatorgroups"}
	guestSubscriptionGVR  = schema.GroupVersionResource{Group: "operators.coreos.com", Version: "v1alpha1", Resource: "subscriptions"}
	guestCSVGVR           = schema.GroupVersionResource{Group: "operators.coreos.com", Version: "v1alpha1", Resource: "clusterserviceversions"}
	guestLVMClusterGVR    = schema.GroupVersionResource{Group: "lvm.topolvm.io", Version: "v1alpha1", Resource: "lvmclusters"}
	guestStorageClassGVR  = schema.GroupVersionResource{Group: "storage.k8s.io", Version: "v1", Resource: "storageclasses"}
	ocpMinorVersionRE     = regexp.MustCompile(`^(\d+\.\d+)\.`)
)

func guestDataDiskSerial(uid string) string {
	sum := sha256.Sum256([]byte(uid))
	return "sno-" + hex.EncodeToString(sum[:8])
}

func guestDataDiskPath(uid string) string {
	return "/dev/disk/by-id/virtio-" + guestDataDiskSerial(uid)
}

func lvmsChannel(version string) (string, error) {
	match := ocpMinorVersionRE.FindStringSubmatch(strings.TrimSpace(version))
	if len(match) != 2 {
		return "", fmt.Errorf("cannot determine LVMS channel from OpenShift version %q", version)
	}
	return "stable-" + match[1], nil
}

func guestStorageLabels(uid string) map[string]interface{} {
	return map[string]interface{}{
		"app.kubernetes.io/managed-by": "sno-vm-operator",
		"sno.llm-d.io/instance-uid":    uid,
	}
}

func guestNamespace(uid string) *unstructured.Unstructured {
	labels := guestStorageLabels(uid)
	labels["openshift.io/cluster-monitoring"] = "true"
	labels["pod-security.kubernetes.io/enforce"] = "privileged"
	labels["pod-security.kubernetes.io/audit"] = "privileged"
	labels["pod-security.kubernetes.io/warn"] = "privileged"
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "Namespace",
		"metadata": map[string]interface{}{
			"name":   guestStorageNamespace,
			"labels": labels,
		},
	}}
}

func guestOperatorGroup(uid string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "operators.coreos.com/v1",
		"kind":       "OperatorGroup",
		"metadata": map[string]interface{}{
			"name":      "lvms-operatorgroup",
			"namespace": guestStorageNamespace,
			"labels":    guestStorageLabels(uid),
		},
		"spec": map[string]interface{}{
			"targetNamespaces": []interface{}{guestStorageNamespace},
		},
	}}
}

func guestLVMSSubscription(uid, channel string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "operators.coreos.com/v1alpha1",
		"kind":       "Subscription",
		"metadata": map[string]interface{}{
			"name":      guestLVMSSubscriptionName,
			"namespace": guestStorageNamespace,
			"labels":    guestStorageLabels(uid),
		},
		"spec": map[string]interface{}{
			"channel":             channel,
			"installPlanApproval": "Automatic",
			"name":                "lvms-operator",
			"source":              "redhat-operators",
			"sourceNamespace":     "openshift-marketplace",
		},
	}}
}

func guestLVMCluster(uid string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "lvm.topolvm.io/v1alpha1",
		"kind":       "LVMCluster",
		"metadata": map[string]interface{}{
			"name":      guestLVMSCluster,
			"namespace": guestStorageNamespace,
			"labels":    guestStorageLabels(uid),
		},
		"spec": map[string]interface{}{
			"storage": map[string]interface{}{
				"deviceClasses": []interface{}{map[string]interface{}{
					"name":    guestLVMSDeviceClass,
					"default": true,
					"deviceSelector": map[string]interface{}{
						"paths": []interface{}{guestDataDiskPath(uid)},
					},
					"thinPoolConfig": map[string]interface{}{
						"name":               "workloads-pool",
						"sizePercent":        int64(90),
						"overprovisionRatio": int64(1),
					},
				}},
			},
		},
	}}
}

// ensureGuestStorage installs LVMS into the isolated SNO guest, configures
// only the dedicated workload disk, and waits for its default StorageClass.
func ensureGuestStorage(ctx context.Context, guest dynamic.Interface, version, uid string) (bool, string, string, error) {
	channel, err := lvmsChannel(version)
	if err != nil {
		return false, "GuestStorageVersionUnsupported", err.Error(), nil
	}
	if err = ensureGuestNamespace(ctx, guest, guestNamespace(uid)); err != nil {
		return false, "GuestStorageNamespaceFailed", "Could not prepare the LVMS namespace", err
	}
	for _, item := range []struct {
		gvr      schema.GroupVersionResource
		resource string
		object   *unstructured.Unstructured
	}{
		{guestOperatorGroupGVR, guestStorageNamespace, guestOperatorGroup(uid)},
		{guestSubscriptionGVR, guestStorageNamespace, guestLVMSSubscription(uid, channel)},
	} {
		if _, err = ensureGuestResource(ctx, guest, item.gvr, item.resource, item.object); err != nil {
			return false, "GuestStorageInstallFailed", "Could not configure the LVMS operator subscription", err
		}
	}

	subscription, err := guest.Resource(guestSubscriptionGVR).Namespace(guestStorageNamespace).Get(ctx, guestLVMSSubscriptionName, metav1.GetOptions{})
	if err != nil {
		return false, "GuestStorageOperatorPending", "Waiting for the LVMS subscription", nil
	}
	csvName, _, _ := unstructured.NestedString(subscription.Object, "status", "currentCSV")
	if csvName == "" {
		csvName, _, _ = unstructured.NestedString(subscription.Object, "status", "installedCSV")
	}
	if csvName == "" {
		return false, "GuestStorageOperatorPending", subscriptionPendingMessage(subscription), nil
	}
	csv, err := guest.Resource(guestCSVGVR).Namespace(guestStorageNamespace).Get(ctx, csvName, metav1.GetOptions{})
	if err != nil {
		return false, "GuestStorageOperatorPending", "Waiting for LVMS operator installation to complete", nil
	}
	csvPhase, _, _ := unstructured.NestedString(csv.Object, "status", "phase")
	if csvPhase == "Failed" {
		message, _, _ := unstructured.NestedString(csv.Object, "status", "message")
		if message == "" {
			message = "The LVMS operator installation failed"
		}
		return false, "GuestStorageOperatorFailed", message, nil
	}
	if csvPhase != "Succeeded" {
		return false, "GuestStorageOperatorPending", "Waiting for the LVMS operator to become ready", nil
	}

	clusters, err := guest.Resource(guestLVMClusterGVR).Namespace(guestStorageNamespace).List(ctx, metav1.ListOptions{})
	if apierrors.IsNotFound(err) {
		return false, "GuestStorageOperatorPending", "Waiting for the LVMS API to become available", nil
	}
	if err != nil {
		return false, "GuestStorageConfigurationFailed", "Could not inspect the guest LVMCluster", err
	}
	for _, existing := range clusters.Items {
		if existing.GetName() != guestLVMSCluster {
			return false, "GuestStorageConfigurationConflict", fmt.Sprintf("Guest already has an LVMCluster named %q; SNO operator will not replace it", existing.GetName()), nil
		}
	}
	cluster, err := ensureGuestResource(ctx, guest, guestLVMClusterGVR, guestStorageNamespace, guestLVMCluster(uid))
	if apierrors.IsNotFound(err) {
		return false, "GuestStorageOperatorPending", "Waiting for the LVMS API to become available", nil
	}
	if err != nil {
		return false, "GuestStorageConfigurationConflict", "The guest LVMCluster does not match the SNO workload disk configuration", err
	}
	state, _, _ := unstructured.NestedString(cluster.Object, "status", "state")
	if state == "Failed" {
		message := lvmClusterFailureMessage(cluster)
		return false, "GuestStorageConfigurationFailed", message, nil
	}
	if state != "Ready" {
		return false, "GuestStoragePending", "Waiting for LVMS to initialize the workload disk", nil
	}

	storageClasses, err := guest.Resource(guestStorageClassGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return false, "GuestStoragePending", "Waiting for the LVMS StorageClass", nil
	}
	var defaultClasses []string
	var workloadClass *unstructured.Unstructured
	for i := range storageClasses.Items {
		storageClass := &storageClasses.Items[i]
		if storageClass.GetAnnotations()["storageclass.kubernetes.io/is-default-class"] == "true" {
			defaultClasses = append(defaultClasses, storageClass.GetName())
		}
		if storageClass.GetName() == guestLVMSStorageClass {
			workloadClass = storageClass
		}
	}
	if workloadClass == nil {
		return false, "GuestStoragePending", "Waiting for the LVMS workload StorageClass", nil
	}
	provisioner, _, _ := unstructured.NestedString(workloadClass.Object, "provisioner")
	if provisioner != "topolvm.io" {
		return false, "GuestStorageConfigurationFailed", "The LVMS workload StorageClass has an unexpected provisioner", nil
	}
	if len(defaultClasses) != 1 || defaultClasses[0] != guestLVMSStorageClass {
		return false, "GuestStorageDefaultClassConflict", "The workload StorageClass is not the guest cluster's only default StorageClass", nil
	}
	return true, "GuestStorageReady", "LVMS is ready and lvms-workloads is the guest default StorageClass", nil
}

func ensureGuestNamespace(ctx context.Context, guest dynamic.Interface, expected *unstructured.Unstructured) error {
	resource := guest.Resource(guestNamespaceGVR)
	actual, err := resource.Get(ctx, guestStorageNamespace, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = resource.Create(ctx, expected, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return err
	}
	if actual.GetDeletionTimestamp() != nil {
		return fmt.Errorf("guest LVMS namespace is terminating")
	}
	labels := actual.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	changed := false
	expectedLabels := expected.GetLabels()
	for key, value := range expectedLabels {
		if labels[key] != value {
			labels[key] = value
			changed = true
		}
	}
	if changed {
		actual.SetLabels(labels)
		_, err = resource.Update(ctx, actual, metav1.UpdateOptions{})
	}
	return err
}

func ensureGuestResource(ctx context.Context, guest dynamic.Interface, gvr schema.GroupVersionResource, namespace string, expected *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	namespaceable := guest.Resource(gvr)
	var resource dynamic.ResourceInterface
	if namespace != "" {
		resource = namespaceable.Namespace(namespace)
	} else {
		resource = namespaceable
	}
	actual, err := resource.Get(ctx, expected.GetName(), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return resource.Create(ctx, expected, metav1.CreateOptions{})
	}
	if err != nil {
		return nil, err
	}
	if actual.GetDeletionTimestamp() != nil || !subset(expected.Object["spec"], actual.Object["spec"]) {
		return nil, fmt.Errorf("guest resource %s/%s has conflicting configuration", namespace, expected.GetName())
	}
	labels := actual.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	changed := false
	for key, value := range expected.GetLabels() {
		if labels[key] != value {
			labels[key] = value
			changed = true
		}
	}
	if changed {
		actual.SetLabels(labels)
		actual, err = resource.Update(ctx, actual, metav1.UpdateOptions{})
	}
	return actual, err
}

func subscriptionPendingMessage(subscription *unstructured.Unstructured) string {
	conditions, _, _ := unstructured.NestedSlice(subscription.Object, "status", "conditions")
	for _, item := range conditions {
		condition, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		status, _, _ := unstructured.NestedString(condition, "status")
		message, _, _ := unstructured.NestedString(condition, "message")
		if status == "True" && message != "" {
			return message
		}
	}
	return "Waiting for the LVMS operator subscription to resolve"
}

func lvmClusterFailureMessage(cluster *unstructured.Unstructured) string {
	classes, _, _ := unstructured.NestedSlice(cluster.Object, "status", "deviceClassStatuses")
	for _, item := range classes {
		class, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		nodes, _, _ := unstructured.NestedSlice(class, "nodeStatus")
		for _, item := range nodes {
			node, ok := item.(map[string]interface{})
			if !ok {
				continue
			}
			message, _, _ := unstructured.NestedString(node, "reason")
			if message != "" {
				return message
			}
		}
	}
	return "LVMS failed to initialize the workload disk"
}
