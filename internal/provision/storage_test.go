package provision

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

func guestStorageFake() *dynamicfake.FakeDynamicClient {
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		guestLVMClusterGVR:   "LVMClusterList",
		guestStorageClassGVR: "StorageClassList",
	})
}

func TestGuestDataDiskPathIsStableAndInstanceSpecific(t *testing.T) {
	first := guestDataDiskPath("instance-one")
	if first != guestDataDiskPath("instance-one") {
		t.Fatal("disk path changed for the same instance")
	}
	if first == guestDataDiskPath("instance-two") {
		t.Fatal("different instances received the same disk path")
	}
	if len(guestDataDiskSerial("instance-one")) > 20 {
		t.Fatalf("disk serial exceeds the virtio serial limit: %q", guestDataDiskSerial("instance-one"))
	}
}

func TestLVMSChannelTracksGuestOpenShiftMinor(t *testing.T) {
	for version, want := range map[string]string{"4.22.18": "stable-4.22", "4.21.7+build.1": "stable-4.21"} {
		got, err := lvmsChannel(version)
		if err != nil || got != want {
			t.Fatalf("lvmsChannel(%q) = %q, %v; want %q", version, got, err, want)
		}
	}
	if _, err := lvmsChannel("unknown"); err == nil {
		t.Fatal("invalid OpenShift version unexpectedly produced an LVMS channel")
	}
}

func TestEnsureGuestStorageInstallsLVMSAndWaitsForDefaultStorageClass(t *testing.T) {
	ctx := context.Background()
	uid := "instance-uid"
	guest := guestStorageFake()

	ready, reason, _, err := ensureGuestStorage(ctx, guest, "4.22.18", uid)
	if err != nil || ready || reason != "GuestStorageOperatorPending" {
		t.Fatalf("initial reconcile = ready %t, reason %q, error %v", ready, reason, err)
	}
	subscription, err := guest.Resource(guestSubscriptionGVR).Namespace(guestStorageNamespace).Get(ctx, guestLVMSSubscriptionName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	channel, _, _ := unstructured.NestedString(subscription.Object, "spec", "channel")
	if channel != "stable-4.22" {
		t.Fatalf("LVMS subscription channel = %q, want stable-4.22", channel)
	}
	classes, _, _ := unstructured.NestedSlice(guestLVMCluster(uid).Object, "spec", "storage", "deviceClasses")
	deviceClass := classes[0].(map[string]interface{})
	paths, _, _ := unstructured.NestedStringSlice(deviceClass, "deviceSelector", "paths")
	if len(paths) != 1 || paths[0] != guestDataDiskPath(uid) {
		t.Fatalf("LVMCluster device paths = %v, want only %q", paths, guestDataDiskPath(uid))
	}

	if err = unstructured.SetNestedField(subscription.Object, "lvms-operator.v4.22.0", "status", "currentCSV"); err != nil {
		t.Fatal(err)
	}
	if _, err = guest.Resource(guestSubscriptionGVR).Namespace(guestStorageNamespace).Update(ctx, subscription, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	csv := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "operators.coreos.com/v1alpha1",
		"kind":       "ClusterServiceVersion",
		"metadata":   map[string]interface{}{"name": "lvms-operator.v4.22.0", "namespace": guestStorageNamespace},
		"status":     map[string]interface{}{"phase": "Succeeded"},
	}}
	if _, err = guest.Resource(guestCSVGVR).Namespace(guestStorageNamespace).Create(ctx, csv, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	ready, reason, _, err = ensureGuestStorage(ctx, guest, "4.22.18", uid)
	if err != nil || ready || reason != "GuestStoragePending" {
		t.Fatalf("LVMCluster creation reconcile = ready %t, reason %q, error %v", ready, reason, err)
	}

	cluster, err := guest.Resource(guestLVMClusterGVR).Namespace(guestStorageNamespace).Get(ctx, guestLVMSCluster, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err = unstructured.SetNestedField(cluster.Object, "Ready", "status", "state"); err != nil {
		t.Fatal(err)
	}
	if _, err = guest.Resource(guestLVMClusterGVR).Namespace(guestStorageNamespace).Update(ctx, cluster, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	storageClass := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "storage.k8s.io/v1",
		"kind":       "StorageClass",
		"metadata": map[string]interface{}{
			"name":        guestLVMSStorageClass,
			"annotations": map[string]interface{}{"storageclass.kubernetes.io/is-default-class": "true"},
		},
		"provisioner": "topolvm.io",
	}}
	if _, err = guest.Resource(guestStorageClassGVR).Create(ctx, storageClass, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	ready, reason, message, err := ensureGuestStorage(ctx, guest, "4.22.18", uid)
	if err != nil || !ready || reason != "GuestStorageReady" {
		t.Fatalf("ready reconcile = ready %t, reason %q, error %v", ready, reason, err)
	}
	if message == "" {
		t.Fatal("ready reconcile returned no status message")
	}
	if _, err = guest.Resource(guestNamespaceGVR).Get(ctx, guestStorageNamespace, metav1.GetOptions{}); err != nil {
		t.Fatalf("LVMS namespace was not created: %v", err)
	}
	group, err := guest.Resource(guestOperatorGroupGVR).Namespace(guestStorageNamespace).Get(ctx, "lvms-operatorgroup", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("LVMS OperatorGroup was not created: group=%v error=%v", group, err)
	}
	targetNamespaces, _, _ := unstructured.NestedStringSlice(group.Object, "spec", "targetNamespaces")
	if len(targetNamespaces) != 1 || targetNamespaces[0] != guestStorageNamespace {
		t.Fatalf("OperatorGroup targets %v, want only %q", targetNamespaces, guestStorageNamespace)
	}
}

func TestEnsureGuestStorageRejectsOtherDefaultStorageClasses(t *testing.T) {
	ctx := context.Background()
	uid := "instance-uid"
	guest := guestStorageFake()
	_, _, _, err := ensureGuestStorage(ctx, guest, "4.22.18", uid)
	if err != nil {
		t.Fatal(err)
	}
	subscription, _ := guest.Resource(guestSubscriptionGVR).Namespace(guestStorageNamespace).Get(ctx, guestLVMSSubscriptionName, metav1.GetOptions{})
	_ = unstructured.SetNestedField(subscription.Object, "lvms-operator.v4.22.0", "status", "currentCSV")
	_, _ = guest.Resource(guestSubscriptionGVR).Namespace(guestStorageNamespace).Update(ctx, subscription, metav1.UpdateOptions{})
	csv := &unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": "operators.coreos.com/v1alpha1", "kind": "ClusterServiceVersion", "metadata": map[string]interface{}{"name": "lvms-operator.v4.22.0", "namespace": guestStorageNamespace}, "status": map[string]interface{}{"phase": "Succeeded"}}}
	_, _ = guest.Resource(guestCSVGVR).Namespace(guestStorageNamespace).Create(ctx, csv, metav1.CreateOptions{})
	_, _, _, err = ensureGuestStorage(ctx, guest, "4.22.18", uid)
	if err != nil {
		t.Fatal(err)
	}
	cluster, _ := guest.Resource(guestLVMClusterGVR).Namespace(guestStorageNamespace).Get(ctx, guestLVMSCluster, metav1.GetOptions{})
	_ = unstructured.SetNestedField(cluster.Object, "Ready", "status", "state")
	_, _ = guest.Resource(guestLVMClusterGVR).Namespace(guestStorageNamespace).Update(ctx, cluster, metav1.UpdateOptions{})
	for name := range map[string]struct{}{"lvms-workloads": {}, "other-default": {}} {
		annotations := map[string]interface{}{}
		if name == "other-default" {
			annotations["storageclass.kubernetes.io/is-default-class"] = "true"
		}
		storageClass := &unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": "storage.k8s.io/v1", "kind": "StorageClass", "metadata": map[string]interface{}{"name": name, "annotations": annotations}, "provisioner": "topolvm.io"}}
		_, _ = guest.Resource(guestStorageClassGVR).Create(ctx, storageClass, metav1.CreateOptions{})
	}
	ready, reason, _, err := ensureGuestStorage(ctx, guest, "4.22.18", uid)
	if err != nil || ready || reason != "GuestStorageDefaultClassConflict" {
		t.Fatalf("conflicting default reconcile = ready %t, reason %q, error %v", ready, reason, err)
	}
}
