package provision

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/kylape/sno-vm-operator/internal/cache"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func readySource(t *testing.T) (Allocator, cache.Image, cache.Entry, *corev1.ConfigMap) {
	t.Helper()
	r := allocator(t, "10.252.0.0/16")
	ctx := context.Background()
	r.Config.Instances.DefaultImage = "v1"
	image := cache.Image{Name: "v1", Reference: "registry.test/seed@sha256:" + string(makeHex('a', 64)), Architecture: "amd64", DiskSize: "500Gi", RegistrySecret: "registry-read", Qualified: true, SeedID: uuid.NewString()}
	r.Config.Images = []cache.Image{image}
	placement := cache.Placement{StorageClass: "local-lvm", NodeSelector: metav1.LabelSelector{MatchLabels: map[string]string{"pool": "a100"}}}
	r.Config.Placements = []cache.Placement{placement}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-one", Labels: map[string]string{corev1.LabelArchStable: "amd64", "pool": "a100"}}, Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
	dv := cache.DataVolume(r.Config.Namespace, node.Name, placement, image)
	dv.SetUID(types.UID(uuid.NewString()))
	dv.Object["status"] = map[string]interface{}{"phase": "Succeeded"}
	mode := corev1.PersistentVolumeBlock
	class := placement.StorageClass
	registrySecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: image.RegistrySecret, Namespace: r.Config.Namespace, UID: types.UID(uuid.NewString())}, Data: map[string][]byte{"accessKeyId": []byte("reader"), "secretKey": []byte("token")}}
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: dv.GetName(), Namespace: r.Config.Namespace, UID: types.UID(uuid.NewString()), OwnerReferences: []metav1.OwnerReference{{APIVersion: cache.DataVolumeGVK.GroupVersion().String(), Kind: "DataVolume", Name: dv.GetName(), UID: dv.GetUID()}}}, Spec: corev1.PersistentVolumeClaimSpec{StorageClassName: &class, VolumeMode: &mode, VolumeName: "local-source", Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("500Gi")}}}, Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound}}
	pv := &corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: pvc.Spec.VolumeName}, Spec: corev1.PersistentVolumeSpec{ClaimRef: &corev1.ObjectReference{Name: pvc.Name, Namespace: pvc.Namespace, UID: pvc.UID}, NodeAffinity: &corev1.VolumeNodeAffinity{Required: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{MatchExpressions: []corev1.NodeSelectorRequirement{{Key: corev1.LabelHostname, Operator: corev1.NodeSelectorOpIn, Values: []string{node.Name}}}}}}}}}
	entry := cache.Entry{Node: node.Name, Image: image.Reference, StorageClass: class, SourcePVC: pvc.Name, Phase: "Ready"}
	state := cache.State{ObservedAt: metav1.Now(), Entries: map[string]cache.Entry{pvc.Name: entry}, Coverage: map[string]cache.Coverage{image.Name: {Eligible: 1, Ready: 1}}}
	raw, _ := json.Marshal(state)
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: cache.StatusName, Namespace: r.Config.Namespace, Labels: map[string]string{"app.kubernetes.io/managed-by": cache.ManagedBy}}, Data: map[string]string{"status.json": string(raw)}}
	for _, object := range []interface{}{node, dv, pvc, pv, cm, registrySecret} {
		switch object := object.(type) {
		case *corev1.Node:
			if err := r.Client.Create(ctx, object); err != nil {
				t.Fatal(err)
			}
		case *corev1.PersistentVolumeClaim:
			if err := r.Client.Create(ctx, object); err != nil {
				t.Fatal(err)
			}
		case *corev1.PersistentVolume:
			if err := r.Client.Create(ctx, object); err != nil {
				t.Fatal(err)
			}
		case *corev1.ConfigMap:
			if err := r.Client.Create(ctx, object); err != nil {
				t.Fatal(err)
			}
		case *corev1.Secret:
			if err := r.Client.Create(ctx, object); err != nil {
				t.Fatal(err)
			}
		default:
			if err := r.Client.Create(ctx, dv); err != nil {
				t.Fatal(err)
			}
		}
	}
	return r, image, entry, cm
}

func TestSourceSelectionUsesConvertedDockerConfigSecretReference(t *testing.T) {
	r, image, entry, _ := readySource(t)
	ctx := context.Background()
	registrySecret := &corev1.Secret{}
	if err := r.Client.Get(ctx, types.NamespacedName{Namespace: r.Config.Namespace, Name: image.RegistrySecret}, registrySecret); err != nil {
		t.Fatal(err)
	}
	registrySecret.Data = map[string][]byte{corev1.DockerConfigJsonKey: []byte(`{"auths":{"registry.test":{"username":"reader","password":"token"}}}`)}
	registrySecret.Type = corev1.SecretTypeDockerConfigJson
	if err := r.Client.Update(ctx, registrySecret); err != nil {
		t.Fatal(err)
	}
	cdiSecretName, err := cache.CDIRegistrySecretName(registrySecret, image.Reference)
	if err != nil {
		t.Fatal(err)
	}
	dv := &unstructured.Unstructured{}
	dv.SetGroupVersionKind(cache.DataVolumeGVK)
	if err = r.Client.Get(ctx, types.NamespacedName{Namespace: r.Config.Namespace, Name: entry.SourcePVC}, dv); err != nil {
		t.Fatal(err)
	}
	if err = unstructured.SetNestedField(dv.Object, cdiSecretName, "spec", "source", "registry", "secretRef"); err != nil {
		t.Fatal(err)
	}
	if err = r.Client.Update(ctx, dv); err != nil {
		t.Fatal(err)
	}
	if _, _, err = r.SelectSource(ctx, "default"); err != nil {
		t.Fatalf("usable source with converted Docker config Secret rejected: %v", err)
	}
}

func makeHex(c byte, n int) []byte {
	result := make([]byte, n)
	for i := range result {
		result[i] = c
	}
	return result
}

func TestSourceSelectionRequiresQualifiedProfileAndLivePVLocality(t *testing.T) {
	r, image, entry, _ := readySource(t)
	ctx := context.Background()
	got, selected, err := r.SelectSource(ctx, "default")
	if err != nil || got.Reference != image.Reference || selected.SourcePVC != entry.SourcePVC {
		t.Fatalf("usable source rejected: %v", err)
	}
	pv := &corev1.PersistentVolume{}
	if err = r.Client.Get(ctx, types.NamespacedName{Name: "local-source"}, pv); err != nil {
		t.Fatal(err)
	}
	pv.Spec.NodeAffinity.Required.NodeSelectorTerms[0].MatchExpressions[0].Values = []string{"other-node"}
	if err = r.Client.Update(ctx, pv); err != nil {
		t.Fatal(err)
	}
	if _, _, err = r.SelectSource(ctx, "default"); err == nil {
		t.Fatal("trusted stale cache status over actual PV locality")
	}
	r.Config.Images[0].Qualified = false
	if _, _, err = r.SelectSource(ctx, "v1"); err == nil {
		t.Fatal("selected an unqualified profile")
	}
}

func TestDefaultRequiresAllNodeCoverageButExplicitQualifiedProofCanUseOne(t *testing.T) {
	r, _, _, cm := readySource(t)
	ctx := context.Background()
	var state cache.State
	if err := json.Unmarshal([]byte(cm.Data["status.json"]), &state); err != nil {
		t.Fatal(err)
	}
	state.Coverage["v1"] = cache.Coverage{Eligible: 2, Ready: 1, Pending: 1}
	raw, _ := json.Marshal(state)
	cm.Data["status.json"] = string(raw)
	if err := r.Client.Update(ctx, cm); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.SelectSource(ctx, "default"); err == nil {
		t.Fatal("default profile selected before complete coverage")
	}
	if _, _, err := r.SelectSource(ctx, "v1"); err != nil {
		t.Fatalf("explicit qualified source should remain available: %v", err)
	}
	state.ObservedAt = metav1.Time{}
	raw, _ = json.Marshal(state)
	cm.Data["status.json"] = string(raw)
	if err := r.Client.Update(ctx, cm); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.SelectSource(ctx, "v1"); err == nil {
		t.Fatal("selected from undated cache observation")
	}
}

func TestQualifiedPreloadedImageUsesOnlyItsBoundLocalProofPVC(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := storagev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	scheme.AddKnownTypeWithName(cache.DataVolumeGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(cache.DataVolumeGVK.GroupVersion().WithKind("DataVolumeList"), &unstructured.UnstructuredList{})
	mode := storagev1.VolumeBindingWaitForFirstConsumer
	class := &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "lvms-a100-tier1-storage"}, Provisioner: "topolvm.io", VolumeBindingMode: &mode}
	image := cache.PreloadedImage{
		Name: "compact-dev", Reference: "quay.io/klape/sno-vm-image@sha256:" + string(makeHex('c', 64)),
		Architecture: "amd64", DiskSize: "50Gi", Namespace: "sno-seed-build", PVC: "sno-seed-compact50",
		PVCUID: "seed-pvc", Node: "a100-01", StorageClass: class.Name, Qualified: true, SeedID: "627d4b39-092a-4a47-99a2-2f28880b9e41", SourceCAHash: string(makeHex('d', 64)),
	}
	dv := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": cache.DataVolumeGVK.GroupVersion().String(), "kind": "DataVolume",
		"metadata": map[string]interface{}{"name": image.PVC, "namespace": image.Namespace, "uid": "seed-dv", "labels": map[string]interface{}{cache.LabelPrefix + "candidate-proof": "true"}},
		"status":   map[string]interface{}{"phase": "Succeeded"},
	}}
	dv.SetGroupVersionKind(cache.DataVolumeGVK)
	volumeMode := corev1.PersistentVolumeBlock
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: image.PVC, Namespace: image.Namespace, UID: "seed-pvc", OwnerReferences: []metav1.OwnerReference{{Kind: "DataVolume", Name: image.PVC, UID: "seed-dv"}}},
		Spec:       corev1.PersistentVolumeClaimSpec{StorageClassName: &class.Name, VolumeMode: &volumeMode, VolumeName: "seed-pv", Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("50Gi")}}},
		Status:     corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
	}
	pv := &corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: "seed-pv"}, Spec: corev1.PersistentVolumeSpec{
		ClaimRef:     &corev1.ObjectReference{Name: pvc.Name, Namespace: pvc.Namespace, UID: pvc.UID},
		NodeAffinity: &corev1.VolumeNodeAffinity{Required: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{MatchExpressions: []corev1.NodeSelectorRequirement{{Key: corev1.LabelHostname, Operator: corev1.NodeSelectorOpIn, Values: []string{image.Node}}}}}}},
	}}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: image.Node, Labels: map[string]string{corev1.LabelArchStable: "amd64"}}, Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
	r := Allocator{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(class, dv, pvc, pv, node).Build(), Config: cache.Config{
		Namespace: "sno-vm-system", Instances: cache.InstancePolicy{DefaultImage: image.Name, PreloadedImages: []cache.PreloadedImage{image}},
	}}
	selected, entry, err := r.SelectSource(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	if selected.Reference != image.Reference || entry.Namespace != image.Namespace || entry.SourcePVC != image.PVC || entry.Node != image.Node {
		t.Fatalf("preloaded source selection changed: image=%+v source=%+v", selected, entry)
	}
	pv.Spec.NodeAffinity.Required.NodeSelectorTerms[0].MatchExpressions[0].Values = []string{"other-node"}
	if err := r.Client.Update(ctx, pv); err != nil {
		t.Fatal(err)
	}
	if _, _, err = r.SelectSource(ctx, "default"); err == nil {
		t.Fatal("accepted a preloaded seed pinned to another node")
	}
}
