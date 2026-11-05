package provision

import (
	"context"
	"fmt"

	api "github.com/kylape/sno-vm-operator/api/v1alpha1"
	"github.com/kylape/sno-vm-operator/internal/cache"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ensureRootVolumeBound gives a WaitForFirstConsumer root PVC a short-lived
// consumer on the already allocated worker. CDI needs the PVC bound before it
// can finish the local clone; the guest VM is created only after that clone.
func (r *InstanceReconciler) ensureRootVolumeBound(ctx context.Context, request *api.SNOVirtualMachine, allocation Allocation, root client.Object) (bool, error) {
	pvc := &corev1.PersistentVolumeClaim{}
	key := types.NamespacedName{Namespace: request.Namespace, Name: root.GetName()}
	if err := r.Client.Get(ctx, key, pvc); apierrors.IsNotFound(err) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	if !rootPVCOwnedByDataVolume(pvc, root) {
		return false, fmt.Errorf("root PVC is not owned by the expected DataVolume")
	}

	binderKey := types.NamespacedName{Namespace: request.Namespace, Name: root.GetName() + "-bind"}
	binder := &corev1.Pod{}
	err := r.Client.Get(ctx, binderKey, binder)
	switch pvc.Status.Phase {
	case corev1.ClaimBound:
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		if !ownedBy(binder, request) {
			return false, fmt.Errorf("root volume binding Pod is not owned by the SNOVirtualMachine")
		}
		if binder.GetDeletionTimestamp() != nil {
			return false, nil
		}
		uid := binder.GetUID()
		if err = r.Client.Delete(ctx, binder, client.Preconditions{UID: &uid}, client.GracePeriodSeconds(0)); err != nil && !apierrors.IsNotFound(err) {
			return false, err
		}
		return false, nil
	case corev1.ClaimPending:
		if err == nil && binder.GetDeletionTimestamp() != nil {
			return false, nil
		}
		if err != nil && !apierrors.IsNotFound(err) {
			return false, err
		}
		expected := cache.Binder(request.Namespace, allocation.Node, root.GetName(), r.Config.BinderImage, nil)
		expected.ObjectMeta = metadata(request, expected.GetName())
		if err == nil && binder.Status.Phase == corev1.PodFailed {
			return false, fmt.Errorf("root volume binding Pod failed; inspect its events")
		}
		if _, err = ensureObject(ctx, r.Client, request, expected); err != nil {
			return false, err
		}
		return false, nil
	default:
		return false, fmt.Errorf("root PVC has unsupported phase %q", pvc.Status.Phase)
	}
}

func rootPVCOwnedByDataVolume(pvc *corev1.PersistentVolumeClaim, root client.Object) bool {
	for _, owner := range pvc.GetOwnerReferences() {
		if owner.APIVersion == "cdi.kubevirt.io/v1beta1" && owner.Kind == "DataVolume" && owner.Name == root.GetName() && owner.UID == root.GetUID() && owner.UID != "" {
			return true
		}
	}
	return false
}
