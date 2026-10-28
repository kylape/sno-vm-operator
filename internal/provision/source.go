package provision

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/kylape/sno-vm-operator/internal/cache"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

// SelectSource verifies live source ownership and locality, rather than trusting
// the cache ledger alone. It is used only for a new allocation; established roots
// must not be moved or replaced when image policy or cache availability changes.
func (r Allocator) SelectSource(ctx context.Context, profile string) (cache.Image, cache.Entry, error) {
	var image cache.Image
	isDefault := profile == "" || profile == "default"
	if isDefault {
		profile = r.Config.Instances.DefaultImage
	}
	for _, candidate := range r.Config.Images {
		if candidate.Name == profile {
			image = candidate
			break
		}
	}
	if image.Name == "" || !image.Qualified {
		return image, cache.Entry{}, fmt.Errorf("requested image profile is not qualified")
	}
	cm := &corev1.ConfigMap{}
	if err := r.Client.Get(ctx, types.NamespacedName{Namespace: r.Config.Namespace, Name: cache.StatusName}, cm); err != nil {
		return image, cache.Entry{}, err
	}
	if cm.Labels["app.kubernetes.io/managed-by"] != cache.ManagedBy {
		return image, cache.Entry{}, fmt.Errorf("cache status ownership mismatch")
	}
	var state cache.State
	if json.Unmarshal([]byte(cm.Data["status.json"]), &state) != nil || state.ObservedAt.IsZero() || time.Since(state.ObservedAt.Time) > time.Minute || time.Until(state.ObservedAt.Time) > time.Minute {
		return image, cache.Entry{}, fmt.Errorf("cache coverage observation is missing or stale")
	}
	coverage := state.Coverage[image.Name]
	if isDefault && (coverage.Eligible == 0 || coverage.Ready != coverage.Eligible || coverage.Pending != 0 || coverage.Failed != 0) {
		return image, cache.Entry{}, fmt.Errorf("default image requires complete eligible-node cache coverage")
	}
	_, allocations, err := r.load(ctx)
	if err != nil {
		return image, cache.Entry{}, err
	}
	load := map[string]int{}
	for _, a := range allocations {
		load[a.Node]++
	}
	candidates := []cache.Entry{}
	for _, entry := range state.Entries {
		if entry.Image == image.Reference && entry.Phase == "Ready" {
			candidates = append(candidates, entry)
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		if load[candidates[i].Node] != load[candidates[j].Node] {
			return load[candidates[i].Node] < load[candidates[j].Node]
		}
		return candidates[i].Node < candidates[j].Node
	})
	for _, entry := range candidates {
		if r.sourceReady(ctx, image, entry) == nil {
			return image, entry, nil
		}
	}
	return image, cache.Entry{}, fmt.Errorf("no verified local cache source is currently usable")
}

func (r Allocator) sourceReady(ctx context.Context, image cache.Image, entry cache.Entry) error {
	node := &corev1.Node{}
	if err := r.Client.Get(ctx, types.NamespacedName{Name: entry.Node}, node); err != nil {
		return err
	}
	if node.Spec.Unschedulable || node.DeletionTimestamp != nil || node.Labels[corev1.LabelArchStable] != image.Architecture {
		return fmt.Errorf("source node is not eligible")
	}
	ready := false
	for _, condition := range node.Status.Conditions {
		if condition.Type == corev1.NodeReady && condition.Status == corev1.ConditionTrue {
			ready = true
		}
	}
	if !ready {
		return fmt.Errorf("source node is not Ready")
	}
	// Initial instance templates have no scheduling tolerations. A cache may be
	// retained on a tainted worker, but it cannot be selected for these VMs.
	for _, taint := range node.Spec.Taints {
		if taint.Effect == corev1.TaintEffectNoSchedule || taint.Effect == corev1.TaintEffectNoExecute {
			return fmt.Errorf("source node requires VM scheduling tolerations")
		}
	}
	placement, err := r.Config.PlacementFor(*node)
	if err != nil || placement == nil || placement.StorageClass != entry.StorageClass || cache.Key(node.Name, *placement, image) != entry.SourcePVC {
		return fmt.Errorf("source no longer matches immutable placement")
	}
	dv := &unstructured.Unstructured{}
	dv.SetGroupVersionKind(cache.DataVolumeGVK)
	key := types.NamespacedName{Namespace: r.Config.Namespace, Name: entry.SourcePVC}
	if err = r.Client.Get(ctx, key, dv); err != nil {
		return err
	}
	if err = cache.ValidateExisting(dv, cache.DataVolume(r.Config.Namespace, node.Name, *placement, image)); err != nil {
		return err
	}
	phase, _, _ := unstructured.NestedString(dv.Object, "status", "phase")
	if phase != "Succeeded" || dv.GetDeletionTimestamp() != nil {
		return fmt.Errorf("source import is not complete")
	}
	pvc := &corev1.PersistentVolumeClaim{}
	if err = r.Client.Get(ctx, key, pvc); err != nil {
		return err
	}
	owned := false
	for _, o := range pvc.OwnerReferences {
		if o.Kind == "DataVolume" && o.Name == dv.GetName() && o.UID == dv.GetUID() && o.UID != "" {
			owned = true
		}
	}
	if !owned || pvc.DeletionTimestamp != nil || pvc.Status.Phase != corev1.ClaimBound || pvc.Spec.VolumeName == "" || pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName != entry.StorageClass || pvc.Spec.VolumeMode == nil || *pvc.Spec.VolumeMode != corev1.PersistentVolumeBlock {
		return fmt.Errorf("source PVC is not owned and bound as expected")
	}
	requested := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
	minimum, err := resource.ParseQuantity(image.DiskSize)
	if err != nil || requested.Cmp(minimum) < 0 {
		return fmt.Errorf("source PVC capacity is insufficient")
	}
	pv := &corev1.PersistentVolume{}
	if err = r.Client.Get(ctx, types.NamespacedName{Name: pvc.Spec.VolumeName}, pv); err != nil {
		return err
	}
	if pv.Spec.ClaimRef == nil || pv.Spec.ClaimRef.UID != pvc.UID || pv.Spec.ClaimRef.Name != pvc.Name || pv.Spec.ClaimRef.Namespace != pvc.Namespace || !cache.PinnedToNode(pv, entry.Node) {
		return fmt.Errorf("source PV does not confirm local PVC ownership")
	}
	return nil
}
