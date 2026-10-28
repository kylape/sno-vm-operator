package cache

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const StatusName = "sno-seed-cache-status"

type Entry struct {
	Node         string      `json:"node"`
	Image        string      `json:"image"`
	StorageClass string      `json:"storageClass"`
	SourcePVC    string      `json:"sourcePVC"`
	Phase        string      `json:"phase"`
	Message      string      `json:"message,omitempty"`
	Attempts     int         `json:"attempts"`
	LastAttempt  metav1.Time `json:"lastAttempt,omitempty"`
}

type Coverage struct {
	Eligible int `json:"eligible"`
	Ready    int `json:"ready"`
	Pending  int `json:"pending"`
	Failed   int `json:"failed"`
}

type State struct {
	Entries    map[string]Entry    `json:"entries"`
	Coverage   map[string]Coverage `json:"coverage"`
	ObservedAt metav1.Time         `json:"observedAt"`
}

type Reconciler struct {
	Client   client.Client
	Config   Config
	Interval time.Duration
}

// NeedLeaderElection ensures only one manager makes import admission decisions.
func (r *Reconciler) NeedLeaderElection() bool { return true }

func (r *Reconciler) Start(ctx context.Context) error {
	ticker := time.NewTicker(r.Interval)
	defer ticker.Stop()
	for {
		if err := r.Sync(ctx); err != nil {
			log.FromContext(ctx).Error(err, "seed cache reconciliation failed")
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func (r *Reconciler) load(ctx context.Context) (*corev1.ConfigMap, State, error) {
	cm := &corev1.ConfigMap{}
	err := r.Client.Get(ctx, types.NamespacedName{Namespace: r.Config.Namespace, Name: StatusName}, cm)
	if apierrors.IsNotFound(err) {
		cm = &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: StatusName, Namespace: r.Config.Namespace, Labels: map[string]string{"app.kubernetes.io/managed-by": ManagedBy}}}
		if err = r.Client.Create(ctx, cm); err != nil {
			return nil, State{}, err
		}
	} else if err != nil {
		return nil, State{}, err
	}
	if cm.Labels["app.kubernetes.io/managed-by"] != ManagedBy {
		return nil, State{}, fmt.Errorf("status ConfigMap is not owned by %s", ManagedBy)
	}
	s := State{Entries: map[string]Entry{}, Coverage: map[string]Coverage{}}
	if b := cm.Data["status.json"]; b != "" {
		if err = json.Unmarshal([]byte(b), &s); err != nil {
			return nil, s, err
		}
	}
	if s.Entries == nil {
		s.Entries = map[string]Entry{}
	}
	s.Coverage = map[string]Coverage{}
	return cm, s, nil
}

func (r *Reconciler) save(ctx context.Context, cm *corev1.ConfigMap, state State) error {
	state.ObservedAt = metav1.Now()
	b, err := json.Marshal(state)
	if err != nil {
		return err
	}
	cm.Data = map[string]string{"status.json": string(b)}
	return r.Client.Update(ctx, cm)
}

func (r *Reconciler) Sync(ctx context.Context) error {
	if err := r.Config.Validate(); err != nil {
		return err
	}
	cm, state, err := r.load(ctx)
	if err != nil {
		return err
	}
	nodes := &corev1.NodeList{}
	if err = r.Client.List(ctx, nodes); err != nil {
		return err
	}
	sort.Slice(nodes.Items, func(i, j int) bool { return nodes.Items[i].Name < nodes.Items[j].Name })
	// Count imports retained from prior configurations as well. They still consume
	// bandwidth even if their image revision was removed from desired policy.
	dvs := &unstructured.UnstructuredList{}
	dvs.SetGroupVersionKind(DataVolumeGVK.GroupVersion().WithKind("DataVolumeList"))
	if err = r.Client.List(ctx, dvs, client.InNamespace(r.Config.Namespace), client.MatchingLabels{"app.kubernetes.io/managed-by": ManagedBy}); err != nil {
		return err
	}
	active := 0
	for _, dv := range dvs.Items {
		phase, _, _ := unstructured.NestedString(dv.Object, "status", "phase")
		if phase != "Succeeded" && phase != "Failed" {
			active++
		}
	}
	// Old resources are deliberately retained; mark them inactive in coverage.
	for k, e := range state.Entries {
		e.Phase = "Retained"
		e.Message = "Not in current eligible node/image policy"
		state.Entries[k] = e
	}
	for _, image := range r.Config.Images {
		coverage := Coverage{}
		for _, node := range nodes.Items {
			if node.Labels[corev1.LabelArchStable] != image.Architecture {
				continue
			}
			p, err := r.Config.PlacementFor(node)
			if err != nil {
				return err
			}
			if p == nil {
				continue
			}
			coverage.Eligible++
			key := Key(node.Name, *p, image)
			e := state.Entries[key]
			e.Node = node.Name
			e.Image = image.Reference
			e.StorageClass = p.StorageClass
			e.SourcePVC = key
			e.Phase = "Pending"
			e.Message = ""
			state.Entries[key] = e
			// Admission attempt counters are saved before creation; restarts cannot
			// reset retry limits or cause unlimited failed imports.
			if err = r.syncEntry(ctx, node, *p, image, &e, &active, func() error { state.Entries[key] = e; return r.save(ctx, cm, state) }); err != nil {
				e.Phase = "Failed"
				e.Message = err.Error()
			}
			state.Entries[key] = e
			switch e.Phase {
			case "Ready":
				coverage.Ready++
			case "Failed":
				coverage.Failed++
			default:
				coverage.Pending++
			}
		}
		state.Coverage[image.Name] = coverage
	}
	return r.save(ctx, cm, state)
}

func (r *Reconciler) syncEntry(ctx context.Context, node corev1.Node, p Placement, image Image, e *Entry, active *int, persist func() error) error {
	if node.Spec.Unschedulable || node.DeletionTimestamp != nil {
		e.Message = "Node is cordoned or deleting"
		return nil
	}
	ready := false
	for _, c := range node.Status.Conditions {
		if c.Type == corev1.NodeReady && c.Status == corev1.ConditionTrue {
			ready = true
		}
	}
	if !ready {
		e.Message = "Node is not Ready"
		return nil
	}
	for _, taint := range node.Spec.Taints {
		if taint.Effect != corev1.TaintEffectNoSchedule && taint.Effect != corev1.TaintEffectNoExecute {
			continue
		}
		tolerated := false
		for _, t := range p.Tolerations {
			if t.ToleratesTaint(&taint) {
				tolerated = true
			}
		}
		if !tolerated {
			e.Message = "Node has an untolerated scheduling taint"
			return nil
		}
	}
	sc := &storagev1.StorageClass{}
	if err := r.Client.Get(ctx, types.NamespacedName{Name: p.StorageClass}, sc); err != nil {
		return err
	}
	if sc.Provisioner != "topolvm.io" || sc.VolumeBindingMode == nil || *sc.VolumeBindingMode != storagev1.VolumeBindingWaitForFirstConsumer {
		return fmt.Errorf("storage class must use TopoLVM with WaitForFirstConsumer")
	}
	secret := &corev1.Secret{}
	if err := r.Client.Get(ctx, types.NamespacedName{Name: image.RegistrySecret, Namespace: r.Config.Namespace}, secret); err != nil {
		return fmt.Errorf("registry credential Secret unavailable: %w", err)
	}
	if len(secret.Data["accessKeyId"]) == 0 || len(secret.Data["secretKey"]) == 0 {
		return fmt.Errorf("CDI registry Secret requires accessKeyId and secretKey")
	}
	expected := DataVolume(r.Config.Namespace, node.Name, p, image)
	dv := &unstructured.Unstructured{}
	dv.SetGroupVersionKind(DataVolumeGVK)
	key := types.NamespacedName{Name: expected.GetName(), Namespace: r.Config.Namespace}
	err := r.Client.Get(ctx, key, dv)
	if apierrors.IsNotFound(err) {
		// Never adopt or overwrite a pre-existing PVC without its DataVolume.
		pvc := &corev1.PersistentVolumeClaim{}
		if pe := r.Client.Get(ctx, key, pvc); pe == nil {
			return fmt.Errorf("PVC exists without expected DataVolume; inspect before retry")
		} else if !apierrors.IsNotFound(pe) {
			return pe
		}
		if e.Attempts >= r.Config.MaxImportAttempts {
			return fmt.Errorf("import attempt limit reached; administrator must inspect and reset status attempts")
		}
		if *active >= r.Config.MaxConcurrentImports {
			e.Message = "Waiting for import concurrency slot"
			return nil
		}
		e.Attempts++
		e.LastAttempt = metav1.Now()
		if err = persist(); err != nil {
			return err
		}
		if err = r.Client.Create(ctx, expected); err != nil {
			return err
		}
		*active++
		e.Message = "Import created; waiting for CDI PVC"
		return nil
	}
	if err != nil {
		return err
	}
	if err = ValidateExisting(dv, expected); err != nil {
		return err
	}
	if dv.GetDeletionTimestamp() != nil {
		e.Message = "Waiting for prior import deletion"
		return nil
	}
	phase, _, _ := unstructured.NestedString(dv.Object, "status", "phase")
	if phase == "Failed" {
		if e.Attempts >= r.Config.MaxImportAttempts {
			return fmt.Errorf("CDI import failed; retry limit reached")
		}
		if time.Since(e.LastAttempt.Time) < time.Minute {
			e.Message = "CDI import failed; waiting for retry backoff"
			return nil
		}
		if err = r.deleteBinder(ctx, key); err != nil {
			return err
		}
		uid := dv.GetUID()
		if err = r.Client.Delete(ctx, dv, client.Preconditions{UID: &uid}, client.PropagationPolicy(metav1.DeletePropagationForeground)); err != nil {
			return err
		}
		e.Message = "Retrying failed CDI import"
		return nil
	}
	pvc := &corev1.PersistentVolumeClaim{}
	if err = r.Client.Get(ctx, key, pvc); apierrors.IsNotFound(err) {
		if phase == "Succeeded" {
			if err = r.deleteBinder(ctx, key); err != nil {
				return err
			}
			uid := dv.GetUID()
			if err = r.Client.Delete(ctx, dv, client.Preconditions{UID: &uid}, client.PropagationPolicy(metav1.DeletePropagationForeground)); err != nil {
				return err
			}
			e.Attempts = 0 // Lost successful cache is a new repair cycle.
		}
		e.Message = "Waiting for cache PVC"
		return nil
	} else if err != nil {
		return err
	}
	owned := false
	for _, owner := range pvc.OwnerReferences {
		if owner.UID == dv.GetUID() && owner.Kind == "DataVolume" {
			owned = true
		}
	}
	if !owned {
		return fmt.Errorf("cache PVC is not owned by the expected DataVolume")
	}
	if pvc.DeletionTimestamp != nil {
		e.Message = "Waiting for cache PVC deletion"
		return nil
	}
	if phase == "Succeeded" && pvc.Status.Phase == corev1.ClaimBound {
		pv := &corev1.PersistentVolume{}
		if err = r.Client.Get(ctx, types.NamespacedName{Name: pvc.Spec.VolumeName}, pv); err != nil {
			return err
		}
		if pv.Spec.ClaimRef == nil || pv.Spec.ClaimRef.UID != pvc.UID || !PinnedToNode(pv, node.Name) {
			return fmt.Errorf("cache PV is not bound to the expected PVC and node")
		}
		if pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName != p.StorageClass {
			return fmt.Errorf("cache PVC storage class mismatch")
		}
		if err = r.deleteBinder(ctx, key); err != nil {
			return err
		}
		e.Phase = "Ready"
		e.Message = "CDI import succeeded and local volume binding verified"
		return nil
	}
	// Avoid keeping a consumer while the importer writes the disk. Binding is
	// sufficient to release CDI's WaitForFirstConsumer gate.
	if pvc.Status.Phase == corev1.ClaimBound {
		e.Message = "CDI phase: " + phase
		return r.deleteBinder(ctx, key)
	}
	binder := Binder(r.Config.Namespace, node.Labels[corev1.LabelHostname], key.Name, r.Config.BinderImage, p.Tolerations)
	if binder.Spec.NodeSelector[corev1.LabelHostname] == "" {
		return fmt.Errorf("node has no hostname label")
	}
	binder.OwnerReferences = []metav1.OwnerReference{{APIVersion: dv.GetAPIVersion(), Kind: dv.GetKind(), Name: dv.GetName(), UID: dv.GetUID()}}
	existing := &corev1.Pod{}
	err = r.Client.Get(ctx, types.NamespacedName{Namespace: key.Namespace, Name: binder.Name}, existing)
	if apierrors.IsNotFound(err) {
		return r.Client.Create(ctx, binder)
	}
	if err != nil {
		return err
	}
	if existing.Labels[LabelPrefix+"cache-key"] != key.Name || existing.Labels["app.kubernetes.io/managed-by"] != ManagedBy {
		return fmt.Errorf("binder ownership mismatch")
	}
	if existing.Status.Phase == corev1.PodFailed {
		return fmt.Errorf("binding pod failed; inspect pod events")
	}
	e.Message = "Waiting for target-node volume binding"
	return nil
}

func (r *Reconciler) deleteBinder(ctx context.Context, key types.NamespacedName) error {
	pod := &corev1.Pod{}
	if err := r.Client.Get(ctx, types.NamespacedName{Namespace: key.Namespace, Name: key.Name + "-bind"}, pod); apierrors.IsNotFound(err) {
		return nil
	} else if err != nil {
		return err
	}
	if pod.Labels["app.kubernetes.io/managed-by"] != ManagedBy || pod.Labels[LabelPrefix+"cache-key"] != key.Name {
		return fmt.Errorf("refusing to delete binder with unexpected ownership")
	}
	uid := pod.UID
	return client.IgnoreNotFound(r.Client.Delete(ctx, pod, client.Preconditions{UID: &uid}))
}
