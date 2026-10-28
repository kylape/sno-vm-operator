package provision

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/kylape/sno-vm-operator/internal/cache"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
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
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: dv.GetName(), Namespace: r.Config.Namespace, UID: types.UID(uuid.NewString()), OwnerReferences: []metav1.OwnerReference{{APIVersion: cache.DataVolumeGVK.GroupVersion().String(), Kind: "DataVolume", Name: dv.GetName(), UID: dv.GetUID()}}}, Spec: corev1.PersistentVolumeClaimSpec{StorageClassName: &class, VolumeMode: &mode, VolumeName: "local-source", Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("500Gi")}}}, Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound}}
	pv := &corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: pvc.Spec.VolumeName}, Spec: corev1.PersistentVolumeSpec{ClaimRef: &corev1.ObjectReference{Name: pvc.Name, Namespace: pvc.Namespace, UID: pvc.UID}, NodeAffinity: &corev1.VolumeNodeAffinity{Required: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{MatchExpressions: []corev1.NodeSelectorRequirement{{Key: corev1.LabelHostname, Operator: corev1.NodeSelectorOpIn, Values: []string{node.Name}}}}}}}}}
	entry := cache.Entry{Node: node.Name, Image: image.Reference, StorageClass: class, SourcePVC: pvc.Name, Phase: "Ready"}
	state := cache.State{ObservedAt: metav1.Now(), Entries: map[string]cache.Entry{pvc.Name: entry}, Coverage: map[string]cache.Coverage{image.Name: {Eligible: 1, Ready: 1}}}
	raw, _ := json.Marshal(state)
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: cache.StatusName, Namespace: r.Config.Namespace, Labels: map[string]string{"app.kubernetes.io/managed-by": cache.ManagedBy}}, Data: map[string]string{"status.json": string(raw)}}
	for _, object := range []interface{}{node, dv, pvc, pv, cm} {
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
		default:
			if err := r.Client.Create(ctx, dv); err != nil {
				t.Fatal(err)
			}
		}
	}
	return r, image, entry, cm
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
