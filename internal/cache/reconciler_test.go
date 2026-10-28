package cache

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func policy() Config {
	return Config{Namespace: "sno-images", BinderImage: "registry.test/binder@sha256:" + strings.Repeat("a", 64), MaxConcurrentImports: 1, MaxImportAttempts: 2,
		Placements: []Placement{{StorageClass: "lvms-test", NodeSelector: metav1.LabelSelector{MatchLabels: map[string]string{"pool": "test"}}}},
		Images:     []Image{{Name: "ocp-4-22-18", Reference: "quay.io/klape/sno-vm-image@sha256:" + strings.Repeat("b", 64), Architecture: "amd64", DiskSize: "500Gi", RegistrySecret: "registry"}}}
}

func node(name, arch string) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{corev1.LabelHostname: name, corev1.LabelArchStable: arch, "pool": "test"}}, Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
}

func setup(t *testing.T, objects ...client.Object) *Reconciler {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := storagev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	scheme.AddKnownTypeWithName(DataVolumeGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(DataVolumeGVK.GroupVersion().WithKind("DataVolumeList"), &unstructured.UnstructuredList{})
	mode := storagev1.VolumeBindingWaitForFirstConsumer
	objects = append(objects, &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "lvms-test"}, Provisioner: "topolvm.io", VolumeBindingMode: &mode}, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "registry", Namespace: "sno-images"}, Data: map[string][]byte{"accessKeyId": []byte("robot"), "secretKey": []byte("test-only")}})
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
	return &Reconciler{Client: c, Config: policy(), Interval: time.Second}
}

func syncOK(t *testing.T, r *Reconciler) State {
	t.Helper()
	if err := r.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	cm := &corev1.ConfigMap{}
	if err := r.Client.Get(context.Background(), types.NamespacedName{Namespace: "sno-images", Name: StatusName}, cm); err != nil {
		t.Fatal(err)
	}
	var state State
	if err := json.Unmarshal([]byte(cm.Data["status.json"]), &state); err != nil {
		t.Fatal(err)
	}
	return state
}

func getDV(t *testing.T, r *Reconciler, n string) *unstructured.Unstructured {
	t.Helper()
	dv := &unstructured.Unstructured{}
	dv.SetGroupVersionKind(DataVolumeGVK)
	if err := r.Client.Get(context.Background(), types.NamespacedName{Namespace: "sno-images", Name: Key(n, r.Config.Placements[0], r.Config.Images[0])}, dv); err != nil {
		t.Fatal(err)
	}
	return dv
}

func TestConfigurationRequiresPinsAndExplicitPlacement(t *testing.T) {
	c := policy()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	c.Images[0].Reference = "quay.io/klape/sno-vm-image:latest"
	if c.Validate() == nil {
		t.Fatal("mutable tag accepted")
	}
	c = policy()
	c.Placements[0].NodeSelector = metav1.LabelSelector{}
	if c.Validate() == nil {
		t.Fatal("empty placement accepted")
	}
	c = policy()
	c.Images[0].Architecture = "arm64"
	if c.Validate() == nil {
		t.Fatal("unqualified guest architecture accepted")
	}
	c = policy()
	c.Placements = append(c.Placements, c.Placements[0])
	if _, err := c.PlacementFor(*node("n1", "amd64")); err == nil {
		t.Fatal("ambiguous pool accepted")
	}
}

func TestConcurrencyAndArchitectureSurviveRestart(t *testing.T) {
	r := setup(t, node("n1", "amd64"), node("n2", "amd64"), node("arm", "arm64"))
	s := syncOK(t, r)
	if c := s.Coverage["ocp-4-22-18"]; c.Eligible != 2 || c.Pending != 2 || c.Ready != 0 {
		t.Fatalf("coverage: %+v", c)
	}
	getDV(t, r, "n1")
	r = &Reconciler{Client: r.Client, Config: policy(), Interval: time.Second}
	s = syncOK(t, r)
	if e := s.Entries[Key("n2", r.Config.Placements[0], r.Config.Images[0])]; e.Attempts != 0 || !strings.Contains(e.Message, "concurrency") {
		t.Fatalf("second import not bounded: %+v", e)
	}
	if e := s.Entries[Key("n1", r.Config.Placements[0], r.Config.Images[0])]; e.Attempts != 1 {
		t.Fatalf("restart repeated import: %+v", e)
	}
}

func TestReadyRequiresSuccessfulImportAndCorrectLocalPV(t *testing.T) {
	r := setup(t, node("n1", "amd64"))
	syncOK(t, r)
	dv := getDV(t, r, "n1")
	dv.SetUID(types.UID("test-dv"))
	if err := r.Client.Update(context.Background(), dv); err != nil {
		t.Fatal(err)
	}
	sc := "lvms-test"
	mode := corev1.PersistentVolumeBlock
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: dv.GetName(), Namespace: "sno-images", UID: types.UID("pvc"), OwnerReferences: []metav1.OwnerReference{{Kind: "DataVolume", UID: dv.GetUID()}}}, Spec: corev1.PersistentVolumeClaimSpec{StorageClassName: &sc, VolumeMode: &mode}}
	if err := r.Client.Create(context.Background(), pvc); err != nil {
		t.Fatal(err)
	}
	s := syncOK(t, r)
	if s.Entries[dv.GetName()].Phase == "Ready" {
		t.Fatal("unimported PVC ready")
	}
	binder := &corev1.Pod{}
	if err := r.Client.Get(context.Background(), types.NamespacedName{Name: dv.GetName() + "-bind", Namespace: "sno-images"}, binder); err != nil {
		t.Fatal(err)
	}
	if binder.Spec.NodeSelector[corev1.LabelHostname] != "n1" || binder.Spec.NodeName != "" {
		t.Fatal("binding bypasses scheduler")
	}
	if binder.Spec.SecurityContext.RunAsUser != nil {
		t.Fatal("fixed UID breaks OpenShift SCC")
	}
	if err := unstructured.SetNestedField(dv.Object, "Succeeded", "status", "phase"); err != nil {
		t.Fatal(err)
	}
	if err := r.Client.Update(context.Background(), dv); err != nil {
		t.Fatal(err)
	}
	pvc.Spec.VolumeName = "pv"
	if err := r.Client.Update(context.Background(), pvc); err != nil {
		t.Fatal(err)
	}
	pvc.Status.Phase = corev1.ClaimBound
	if err := r.Client.Status().Update(context.Background(), pvc); err != nil {
		t.Fatal(err)
	}
	pv := &corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: "pv"}, Spec: corev1.PersistentVolumeSpec{ClaimRef: &corev1.ObjectReference{UID: pvc.UID}, NodeAffinity: &corev1.VolumeNodeAffinity{Required: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{MatchExpressions: []corev1.NodeSelectorRequirement{{Key: "topology.topolvm.io/node", Operator: corev1.NodeSelectorOpIn, Values: []string{"wrong-node"}}}}}}}}}
	if err := r.Client.Create(context.Background(), pv); err != nil {
		t.Fatal(err)
	}
	s = syncOK(t, r)
	if e := s.Entries[dv.GetName()]; e.Phase != "Failed" || !strings.Contains(e.Message, "expected PVC and node") {
		t.Fatalf("wrong-node source counted ready: %+v", e)
	}
	pv.Spec.NodeAffinity.Required.NodeSelectorTerms[0].MatchExpressions[0].Values = []string{"n1"}
	if err := r.Client.Update(context.Background(), pv); err != nil {
		t.Fatal(err)
	}
	s = syncOK(t, r)
	if e := s.Entries[dv.GetName()]; e.Phase != "Ready" {
		t.Fatalf("valid source not ready: %+v", e)
	}
	if err := r.Client.Get(context.Background(), types.NamespacedName{Name: binder.Name, Namespace: binder.Namespace}, &corev1.Pod{}); !apierrors.IsNotFound(err) {
		t.Fatalf("binding pod not cleaned up: %v", err)
	}
}

func TestLostCacheIsRecreatedAndOldRevisionRetained(t *testing.T) {
	r := setup(t, node("n1", "amd64"))
	syncOK(t, r)
	dv := getDV(t, r, "n1")
	if err := unstructured.SetNestedField(dv.Object, "Succeeded", "status", "phase"); err != nil {
		t.Fatal(err)
	}
	if err := r.Client.Update(context.Background(), dv); err != nil {
		t.Fatal(err)
	}
	syncOK(t, r) // Completed import lost its PVC; discard stale DV.
	s := syncOK(t, r)
	if e := s.Entries[dv.GetName()]; e.Attempts != 1 {
		t.Fatalf("cache repair not started: %+v", e)
	}
	dv = getDV(t, r, "n1")
	if err := unstructured.SetNestedField(dv.Object, "Succeeded", "status", "phase"); err != nil {
		t.Fatal(err)
	}
	if err := r.Client.Update(context.Background(), dv); err != nil {
		t.Fatal(err)
	}
	r.Config.Images[0].Reference = "quay.io/klape/sno-vm-image@sha256:" + strings.Repeat("c", 64)
	s = syncOK(t, r)
	if s.Entries[dv.GetName()].Phase != "Retained" {
		t.Fatal("previous image revision not retained")
	}
	old := &unstructured.Unstructured{}
	old.SetGroupVersionKind(DataVolumeGVK)
	if err := r.Client.Get(context.Background(), client.ObjectKeyFromObject(dv), old); err != nil {
		t.Fatalf("previous image removed: %v", err)
	}
}

func TestImportFailureHasBoundedRetries(t *testing.T) {
	r := setup(t, node("n1", "amd64"))
	syncOK(t, r)
	key := Key("n1", r.Config.Placements[0], r.Config.Images[0])
	for attempt := 1; attempt <= 2; attempt++ {
		dv := getDV(t, r, "n1")
		if err := unstructured.SetNestedField(dv.Object, "Failed", "status", "phase"); err != nil {
			t.Fatal(err)
		}
		if err := r.Client.Update(context.Background(), dv); err != nil {
			t.Fatal(err)
		}
		cm, state, err := r.load(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		e := state.Entries[key]
		e.LastAttempt = metav1.NewTime(time.Now().Add(-2 * time.Minute))
		state.Entries[key] = e
		if err = r.save(context.Background(), cm, state); err != nil {
			t.Fatal(err)
		}
		s := syncOK(t, r)
		if attempt == 2 {
			if e = s.Entries[key]; e.Phase != "Failed" || !strings.Contains(e.Message, "retry limit") {
				t.Fatalf("retry limit not enforced: %+v", e)
			}
		} else {
			syncOK(t, r)
		}
	}
}

func TestNoAdoptionOfConflictingVolume(t *testing.T) {
	c := policy()
	key := Key("n1", c.Placements[0], c.Images[0])
	r := setup(t, node("n1", "amd64"), &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: key, Namespace: "sno-images"}})
	s := syncOK(t, r)
	if e := s.Entries[key]; e.Phase != "Failed" || e.Attempts != 0 {
		t.Fatalf("unowned PVC adopted: %+v", e)
	}
}
