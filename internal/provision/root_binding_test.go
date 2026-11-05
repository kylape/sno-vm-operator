package provision

import (
	"context"
	"testing"

	api "github.com/kylape/sno-vm-operator/api/v1alpha1"
	"github.com/kylape/sno-vm-operator/internal/cache"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestRootVolumeBindingUsesAllocatedWorkerAndReleasesBinder(t *testing.T) {
	ctx := context.Background()
	req := request()
	root := rootDataVolume(req)
	volumeMode := corev1.PersistentVolumeBlock
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name: root.GetName(), Namespace: req.Namespace, UID: types.UID("root-pvc-uid"),
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "cdi.kubevirt.io/v1beta1", Kind: "DataVolume", Name: root.GetName(), UID: root.GetUID()}},
		},
		Spec:   corev1.PersistentVolumeClaimSpec{VolumeMode: &volumeMode},
		Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimPending},
	}
	scheme := runtimeScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pvc).Build()
	r := InstanceReconciler{Client: c, Config: cache.Config{Namespace: "sno-vm-system", BinderImage: "registry.test/pause@sha256:deadbeef"}}
	allocation := Allocation{Node: "worker-one"}

	bound, err := r.ensureRootVolumeBound(ctx, req, allocation, root)
	if err != nil || bound {
		t.Fatalf("pending root volume should request a binder: bound=%t err=%v", bound, err)
	}
	binder := &corev1.Pod{}
	if err = c.Get(ctx, types.NamespacedName{Namespace: req.Namespace, Name: root.GetName() + "-bind"}, binder); err != nil {
		t.Fatal(err)
	}
	if !ownedBy(binder, req) || binder.Spec.NodeSelector[corev1.LabelHostname] != allocation.Node || binder.Spec.Volumes[0].PersistentVolumeClaim.ClaimName != pvc.Name {
		t.Fatal("root binder is not owned, pinned, and attached to the requested PVC")
	}

	if err = c.Get(ctx, types.NamespacedName{Namespace: req.Namespace, Name: pvc.Name}, pvc); err != nil {
		t.Fatal(err)
	}
	pvc.Status.Phase = corev1.ClaimBound
	if err = c.Status().Update(ctx, pvc); err != nil {
		t.Fatal(err)
	}
	if bound, err = r.ensureRootVolumeBound(ctx, req, allocation, root); err != nil || bound {
		t.Fatalf("bound PVC should first release its binder: bound=%t err=%v", bound, err)
	}
	if err = c.Get(ctx, types.NamespacedName{Namespace: req.Namespace, Name: root.GetName() + "-bind"}, binder); err == nil {
		// The fake client does not run garbage collection for deleting Pods.
		binder.OwnerReferences = nil
		binder.Finalizers = nil
		binder.DeletionTimestamp = nil
		if err = c.Update(ctx, binder); err != nil {
			t.Fatal(err)
		}
		if err = c.Delete(ctx, binder); err != nil {
			t.Fatal(err)
		}
	} else if !apierrors.IsNotFound(err) {
		t.Fatal(err)
	}
	if bound, err = r.ensureRootVolumeBound(ctx, req, allocation, root); err != nil || !bound {
		t.Fatalf("bound PVC without binder should be ready for CDI clone: bound=%t err=%v", bound, err)
	}
}

func TestRootVolumeBindingRejectsUnownedPVC(t *testing.T) {
	ctx := context.Background()
	req := request()
	root := rootDataVolume(req)
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: root.GetName(), Namespace: req.Namespace, OwnerReferences: []metav1.OwnerReference{{APIVersion: "v1", Kind: "Secret", Name: "unrelated", UID: "other"}}}, Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimPending}}
	c := fake.NewClientBuilder().WithScheme(runtimeScheme(t)).WithObjects(pvc).Build()
	r := InstanceReconciler{Client: c, Config: cache.Config{Namespace: "sno-vm-system", BinderImage: "pause"}}
	if _, err := r.ensureRootVolumeBound(ctx, req, Allocation{Node: "worker-one"}, root); err == nil {
		t.Fatal("accepted a PVC not owned by the expected root DataVolume")
	}
}

func runtimeScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return scheme
}

func rootDataVolume(req *api.SNOVirtualMachine) *unstructured.Unstructured {
	root := &unstructured.Unstructured{Object: map[string]interface{}{}}
	root.SetGroupVersionKind(cache.DataVolumeGVK)
	root.SetName(req.Name + "-root")
	root.SetNamespace(req.Namespace)
	root.SetUID(types.UID("root-dv-uid"))
	return root
}
