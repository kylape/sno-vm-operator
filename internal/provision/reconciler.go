package provision

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"

	api "github.com/kylape/sno-vm-operator/api/v1alpha1"
	"github.com/kylape/sno-vm-operator/internal/cache"
	"golang.org/x/crypto/ssh"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const instanceFinalizer = "sno.llm-d.io/instance-finalizer"

var VirtualMachineInstanceGVK = schema.GroupVersionKind{Group: "kubevirt.io", Version: "v1", Kind: "VirtualMachineInstance"}

var errResourceTerminating = errors.New("owned resource is terminating")

// InstanceReconciler drives one immutable request through local disk cloning,
// first-boot specialization, guest readiness, and credential publication.
type InstanceReconciler struct {
	Client   client.Client
	Config   cache.Config
	Observer GuestObserver
	Interval time.Duration
}

func (r *InstanceReconciler) NeedLeaderElection() bool { return true }

func (r *InstanceReconciler) Start(ctx context.Context) error {
	interval := r.Interval
	if interval < time.Second {
		interval = 15 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := r.Sync(ctx); err != nil {
			log.FromContext(ctx).Error(err, "SNO instance reconciliation had errors")
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func (r *InstanceReconciler) Sync(ctx context.Context) error {
	requests := &api.SNOVirtualMachineList{}
	if err := r.Client.List(ctx, requests); err != nil {
		if meta.IsNoMatchError(err) {
			return nil // The administrator may not have installed the CRD yet.
		}
		return err
	}
	var failures []error
	for i := range requests.Items {
		request := &requests.Items[i]
		if err := r.syncOne(ctx, request); err != nil {
			if errors.Is(err, errResourceTerminating) {
				continue
			}
			failures = append(failures, fmt.Errorf("%s/%s: %w", request.Namespace, request.Name, err))
		}
	}
	return errors.Join(failures...)
}

func (r *InstanceReconciler) syncOne(ctx context.Context, request *api.SNOVirtualMachine) error {
	if request.DeletionTimestamp != nil {
		if contains(request.Finalizers, instanceFinalizer) {
			return r.cleanup(ctx, request)
		}
		return nil
	}
	if !r.Config.Instances.Enabled {
		return r.report(ctx, request, "Pending", "Ready", metav1.ConditionFalse, "InstancesDisabled", "Instance provisioning is disabled by operator policy", "")
	}
	if !contains(request.Finalizers, instanceFinalizer) {
		request.Finalizers = append(request.Finalizers, instanceFinalizer)
		if err := r.Client.Update(ctx, request); err != nil {
			return err
		}
		setMilestone(request, "Requested")
		return r.saveStatus(ctx, request)
	}
	allocator := Allocator{Client: r.Client, Config: r.Config}
	allocation, err := allocator.Get(ctx, request)
	if err != nil {
		return r.reportError(ctx, request, "Reconciling", "AllocationError", err)
	}
	if allocation == nil {
		image, source, selectErr := allocator.SelectSource(ctx, request.Spec.ImageProfile)
		if selectErr != nil {
			return r.report(ctx, request, "Pending", "Ready", metav1.ConditionFalse, "SourceUnavailable", selectErr.Error(), "")
		}
		allocation, err = allocator.Create(ctx, request, image, source)
		if err != nil {
			return r.reportError(ctx, request, "Reconciling", "AllocationError", err)
		}
	}
	a := *allocation
	request.Status.ImageDigest = a.Image.Reference
	request.Status.ClusterID = a.ClusterID
	request.Status.Node = a.Node
	request.Status.APIEndpoint = a.Endpoint()
	request.Status.VMRef = &corev1.LocalObjectReference{Name: request.Name}
	request.Status.RootPVCRef = &corev1.LocalObjectReference{Name: request.Name + "-root"}
	setMilestone(request, "Allocated")
	if _, err = Hardware(request, a); err != nil {
		return r.reportError(ctx, request, "Failed", "InvalidResources", err)
	}

	management, bootstrap, err := r.ensureCredentials(ctx, request, a, allocator)
	if err != nil {
		return r.reportError(ctx, request, "Reconciling", "CredentialsUnavailable", err)
	}

	service := AccessService(request)
	serviceResult, err := ensureObject(ctx, r.Client, request, service)
	if err != nil {
		return r.reportError(ctx, request, "Failed", "ResourceCollision", err)
	}
	service = serviceResult.(*corev1.Service)
	if service.Spec.ClusterIP == "" || service.Spec.ClusterIP == corev1.ClusterIPNone {
		return r.report(ctx, request, "Provisioning", "Ready", metav1.ConditionFalse, "ServicePending", "Waiting for the guest access Service address", "")
	}
	if err = allocator.ensureDNS(ctx, a, service.Spec.ClusterIP); err != nil {
		return r.reportError(ctx, request, "Reconciling", "GuestDNSUnavailable", err)
	}

	root, err := RootVolume(request, a, r.Config.Namespace)
	if err != nil {
		return r.reportError(ctx, request, "Failed", "InvalidRootVolume", err)
	}
	needsSource := true
	existingRoot := &unstructured.Unstructured{}
	existingRoot.SetGroupVersionKind(cache.DataVolumeGVK)
	if err = r.Client.Get(ctx, types.NamespacedName{Namespace: request.Namespace, Name: request.Name + "-root"}, existingRoot); err == nil {
		phase, _, _ := unstructured.NestedString(existingRoot.Object, "status", "phase")
		needsSource = phase != "Succeeded"
	} else if !apierrors.IsNotFound(err) {
		return r.reportError(ctx, request, "Reconciling", "RootVolumeUnavailable", err)
	}
	if needsSource {
		if err = allocator.allocationSourceReady(ctx, a); err != nil {
			return r.reportError(ctx, request, "Reconciling", "SourceUnavailable", err)
		}
	}
	rootResult, err := ensureObject(ctx, r.Client, request, root)
	if err != nil {
		return r.reportError(ctx, request, "Failed", "RootVolumeCollision", err)
	}
	root = rootResult.(*unstructured.Unstructured)
	rootPhase, _, _ := unstructured.NestedString(root.Object, "status", "phase")
	if rootPhase != "Succeeded" {
		return r.report(ctx, request, "Provisioning", "Ready", metav1.ConditionFalse, "RootVolumePending", "Waiting for CDI to finish the local seed clone", "")
	}
	setMilestone(request, "RootVolumeReady")
	rootPVC := &corev1.PersistentVolumeClaim{}
	if err = r.Client.Get(ctx, types.NamespacedName{Namespace: request.Namespace, Name: request.Name + "-root"}, rootPVC); err != nil {
		if !apierrors.IsNotFound(err) {
			return r.reportError(ctx, request, "Reconciling", "RootPVCUnavailable", err)
		}
		return r.report(ctx, request, "Provisioning", "Ready", metav1.ConditionFalse, "RootPVCUnbound", "Waiting for the cloned root PVC", "")
	}
	request.Status.RootPVCRef = &corev1.LocalObjectReference{Name: rootPVC.Name}

	vm, err := VirtualMachine(request, a)
	if err != nil {
		return r.reportError(ctx, request, "Failed", "InvalidVirtualMachine", err)
	}
	if _, err = ensureObject(ctx, r.Client, request, vm); err != nil {
		return r.reportError(ctx, request, "Failed", "VirtualMachineCollision", err)
	}
	route, err := ensureObject(ctx, r.Client, request, APIRoute(request, a))
	if err != nil {
		return r.reportError(ctx, request, "Failed", "APIRouteCollision", err)
	}
	if !routeAdmitted(route.(*unstructured.Unstructured), "api."+a.Domain()) {
		return r.report(ctx, request, "Provisioning", "Ready", metav1.ConditionFalse, "APIRoutePending", "Waiting for the guest API Route to be admitted", "")
	}

	vmi := &unstructured.Unstructured{}
	vmi.SetGroupVersionKind(VirtualMachineInstanceGVK)
	if err = r.Client.Get(ctx, types.NamespacedName{Namespace: request.Namespace, Name: request.Name}, vmi); err != nil {
		if !apierrors.IsNotFound(err) {
			return r.reportError(ctx, request, "Reconciling", "VirtualMachinePending", err)
		}
		return r.report(ctx, request, "Provisioning", "Ready", metav1.ConditionFalse, "VirtualMachinePending", "Waiting for the guest VM to start", "")
	}
	vmiPhase, _, _ := unstructured.NestedString(vmi.Object, "status", "phase")
	if vmiPhase != "Running" {
		return r.report(ctx, request, "Provisioning", "Ready", metav1.ConditionFalse, "VirtualMachinePending", "Waiting for the guest VM to start", "")
	}
	setMilestone(request, "VirtualMachineRunning")
	observer := r.Observer
	if observer == nil {
		observer = SSHObserver{}
	}
	observeCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	observation, err := observer.Observe(observeCtx, a, service.Spec.ClusterIP, management, bootstrap)
	cancel()
	if err != nil {
		return r.reportError(ctx, request, "Reconciling", "GuestObservationFailed", err)
	}
	if observation.Bootstrapped {
		setMilestone(request, "Specialized")
	}
	if !observation.Ready {
		phase := "Bootstrapping"
		if observation.Bootstrapped {
			phase = "Provisioning"
		}
		return r.report(ctx, request, phase, "Ready", metav1.ConditionFalse, observation.Reason, observation.Message, observation.Node)
	}
	if err = r.publishCredentials(ctx, request, observation.Kubeconfig); err != nil {
		return r.reportError(ctx, request, "Reconciling", "CredentialPublicationFailed", err)
	}
	setMilestone(request, "Ready")
	return r.report(ctx, request, "Ready", "Ready", metav1.ConditionTrue, "GuestReady", observation.Message, observation.Node)
}

func (r *InstanceReconciler) ensureCredentials(ctx context.Context, request *api.SNOVirtualMachine, a Allocation, allocator Allocator) (*corev1.Secret, *corev1.Secret, error) {
	key := types.NamespacedName{Namespace: request.Namespace, Name: request.Name + "-management"}
	management := &corev1.Secret{}
	err := r.Client.Get(ctx, key, management)
	if apierrors.IsNotFound(err) {
		pull := &corev1.Secret{}
		pullNamespace := r.Config.Instances.GuestPullSecretNamespace
		if pullNamespace == "" {
			pullNamespace = r.Config.Namespace
		}
		if err = r.Client.Get(ctx, types.NamespacedName{Namespace: pullNamespace, Name: r.Config.Instances.GuestPullSecret}, pull); err != nil {
			return nil, nil, fmt.Errorf("guest pull Secret is unavailable: %w", err)
		}
		pullData := pull.Data[corev1.DockerConfigJsonKey]
		if len(pullData) == 0 {
			pullData = pull.Data["config.json"]
		}
		keys, err := r.readPublicKeys(ctx, request)
		if err != nil {
			return nil, nil, err
		}
		dns, err := allocator.guestDNS(ctx)
		if err != nil {
			return nil, nil, err
		}
		generated, err := GenerateCredentials(request, a, pullData, keys, []string{dns})
		if err != nil {
			return nil, nil, err
		}
		if _, err = ensureObject(ctx, r.Client, request, generated.Management); err != nil {
			return nil, nil, err
		}
		management = generated.Management
	} else if err != nil {
		return nil, nil, err
	} else {
		if !ownedBy(management, request) || management.Immutable == nil || !*management.Immutable {
			return nil, nil, fmt.Errorf("management Secret ownership or immutability check failed")
		}
		if len(management.Data["ssh-privatekey"]) == 0 || len(management.Data["ssh-host-publickey"]) == 0 || len(management.Data["bootstrap-config.json"]) == 0 {
			return nil, nil, fmt.Errorf("management Secret is incomplete")
		}
		var bootstrapConfig BootstrapConfig
		if json.Unmarshal(management.Data["bootstrap-config.json"], &bootstrapConfig) != nil || bootstrapConfig.Version != 1 || bootstrapConfig.InstanceUID != a.UID || bootstrapConfig.ClusterID != a.ClusterID || bootstrapConfig.SeedID != a.Image.SeedID || bootstrapConfig.ClusterName != a.ClusterName || bootstrapConfig.BaseDomain != a.BaseDomain {
			return nil, nil, fmt.Errorf("management Secret identity does not match allocation")
		}
	}
	if err := validateManagementKeypair(management); err != nil {
		return nil, nil, err
	}
	immutable := true
	bootstrap := &corev1.Secret{ObjectMeta: metadata(request, request.Name+"-bootstrap"), Type: corev1.SecretTypeOpaque, Immutable: &immutable, Data: map[string][]byte{"config.json": append([]byte(nil), management.Data["bootstrap-config.json"]...)}}
	result, err := ensureObject(ctx, r.Client, request, bootstrap)
	if err != nil {
		return nil, nil, err
	}
	return management, result.(*corev1.Secret), nil
}

func (r *InstanceReconciler) readPublicKeys(ctx context.Context, request *api.SNOVirtualMachine) ([]byte, error) {
	name, namespace := r.Config.Instances.SSHPublicKeySecret, r.Config.Namespace
	if request.Spec.SSHPublicKeySecretRef != nil {
		name, namespace = request.Spec.SSHPublicKeySecretRef.Name, request.Namespace
	}
	if name == "" {
		return nil, fmt.Errorf("request must specify sshPublicKeySecretRef when no operator default is configured")
	}
	secret := &corev1.Secret{}
	if err := r.Client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, secret); err != nil {
		return nil, fmt.Errorf("SSH public key Secret %s/%s is unavailable: %w", namespace, name, err)
	}
	for _, key := range []string{"ssh-publickey", "publicKey", "id_rsa.pub"} {
		if value := secret.Data[key]; len(value) > 0 {
			return value, nil
		}
	}
	return nil, fmt.Errorf("SSH public key Secret %s/%s has no supported public-key field", namespace, name)
}

func validateManagementKeypair(secret *corev1.Secret) error {
	signer, err := ssh.ParsePrivateKey(secret.Data["ssh-privatekey"])
	if err != nil {
		return fmt.Errorf("management SSH identity is invalid")
	}
	public, _, _, _, err := ssh.ParseAuthorizedKey(secret.Data["ssh-host-publickey"])
	if err != nil || string(ssh.MarshalAuthorizedKey(public)) != string(ssh.MarshalAuthorizedKey(signer.PublicKey())) {
		return fmt.Errorf("management SSH host key does not match its private key")
	}
	return nil
}

func (r *InstanceReconciler) publishCredentials(ctx context.Context, request *api.SNOVirtualMachine, kubeconfig []byte) error {
	name := request.Name + "-credentials"
	expected := &corev1.Secret{ObjectMeta: metadata(request, name), Type: corev1.SecretTypeOpaque, Data: map[string][]byte{"kubeconfig": kubeconfig}}
	actual := &corev1.Secret{}
	err := r.Client.Get(ctx, types.NamespacedName{Namespace: request.Namespace, Name: name}, actual)
	if apierrors.IsNotFound(err) {
		if err = r.Client.Create(ctx, expected); err != nil {
			return err
		}
		request.Status.CredentialsSecretRef = &corev1.LocalObjectReference{Name: name}
		return nil
	}
	if err != nil {
		return err
	}
	if !ownedBy(actual, request) || actual.GetDeletionTimestamp() != nil {
		return fmt.Errorf("credential Secret collision or deletion")
	}
	if reflect.DeepEqual(actual.Data, expected.Data) {
		request.Status.CredentialsSecretRef = &corev1.LocalObjectReference{Name: name}
		return nil
	}
	actual.Data = expected.Data
	if err = r.Client.Update(ctx, actual); err != nil {
		return err
	}
	request.Status.CredentialsSecretRef = &corev1.LocalObjectReference{Name: name}
	return nil
}

func routeAdmitted(route *unstructured.Unstructured, expectedHost string) bool {
	ingresses, _, _ := unstructured.NestedSlice(route.Object, "status", "ingress")
	for _, item := range ingresses {
		ingress, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		host, _, _ := unstructured.NestedString(ingress, "host")
		if host != expectedHost {
			continue
		}
		conditions, _, _ := unstructured.NestedSlice(ingress, "conditions")
		for _, item := range conditions {
			condition, ok := item.(map[string]interface{})
			if !ok {
				continue
			}
			typ, _, _ := unstructured.NestedString(condition, "type")
			status, _, _ := unstructured.NestedString(condition, "status")
			if typ == "Admitted" && status == "True" {
				return true
			}
		}
	}
	return false
}

func (r *InstanceReconciler) cleanup(ctx context.Context, request *api.SNOVirtualMachine) error {
	allocator := Allocator{Client: r.Client, Config: r.Config}
	a, err := allocator.Get(ctx, request)
	if err != nil {
		return err
	}
	if err = r.deleteOwned(ctx, request, RouteGVK, request.Name+"-api"); err != nil {
		return err
	}
	if err = r.deleteOwned(ctx, request, VirtualMachineGVK, request.Name); err != nil {
		return err
	}
	if err = r.waitForGone(ctx, request.Namespace, VirtualMachineInstanceGVK, request.Name); err != nil {
		return err
	}
	if request.Spec.RootDiskRetentionPolicy == "Retain" {
		if err = r.detachRoot(ctx, request); err != nil {
			return err
		}
	} else if err = r.deleteOwned(ctx, request, cache.DataVolumeGVK, request.Name+"-root"); err != nil {
		return err
	}
	for _, name := range []string{request.Name + "-access", request.Name + "-bootstrap", request.Name + "-management", request.Name + "-credentials"} {
		var object client.Object
		if name == request.Name+"-access" {
			object = &corev1.Service{}
		} else {
			object = &corev1.Secret{}
		}
		if err = r.deleteNamedOwned(ctx, request, object, name); err != nil {
			return err
		}
	}
	if a != nil {
		if err = allocator.removeDNS(ctx, *a); err != nil {
			return err
		}
		if err = allocator.Release(ctx, string(request.UID)); err != nil {
			return err
		}
	}
	request.Finalizers = remove(request.Finalizers, instanceFinalizer)
	return r.Client.Update(ctx, request)
}

func (r *InstanceReconciler) deleteOwned(ctx context.Context, request *api.SNOVirtualMachine, gvk schema.GroupVersionKind, name string) error {
	item := &unstructured.Unstructured{}
	item.SetGroupVersionKind(gvk)
	return r.deleteNamedOwned(ctx, request, item, name)
}

func (r *InstanceReconciler) deleteNamedOwned(ctx context.Context, request *api.SNOVirtualMachine, object client.Object, name string) error {
	object.SetName(name)
	object.SetNamespace(request.Namespace)
	err := r.Client.Get(ctx, client.ObjectKeyFromObject(object), object)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !ownedBy(object, request) {
		return fmt.Errorf("refusing to delete unowned resource %s/%s", object.GetNamespace(), object.GetName())
	}
	if object.GetDeletionTimestamp() != nil {
		return errResourceTerminating
	}
	if err = r.Client.Delete(ctx, object); err != nil {
		return err
	}
	return errResourceTerminating
}

func (r *InstanceReconciler) waitForGone(ctx context.Context, namespace string, gvk schema.GroupVersionKind, name string) error {
	object := &unstructured.Unstructured{}
	object.SetGroupVersionKind(gvk)
	err := r.Client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, object)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	return errResourceTerminating
}

func (r *InstanceReconciler) detachRoot(ctx context.Context, request *api.SNOVirtualMachine) error {
	root := &unstructured.Unstructured{}
	root.SetGroupVersionKind(cache.DataVolumeGVK)
	key := types.NamespacedName{Namespace: request.Namespace, Name: request.Name + "-root"}
	err := r.Client.Get(ctx, key, root)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !ownedBy(root, request) {
		annotations := root.GetAnnotations()
		if len(root.GetOwnerReferences()) == 0 && annotations[cache.LabelPrefix+"retained-from-uid"] == string(request.UID) {
			return nil
		}
		return fmt.Errorf("refusing to retain unowned root DataVolume")
	}
	root.SetOwnerReferences(nil)
	annotations := root.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[cache.LabelPrefix+"retained-from-uid"] = string(request.UID)
	root.SetAnnotations(annotations)
	return r.Client.Update(ctx, root)
}

func (r *InstanceReconciler) reportError(ctx context.Context, request *api.SNOVirtualMachine, phase, reason string, err error) error {
	if statusErr := r.report(ctx, request, phase, "Ready", metav1.ConditionFalse, reason, err.Error(), request.Status.Node); statusErr != nil {
		return errors.Join(err, statusErr)
	}
	return err
}

func (r *InstanceReconciler) report(ctx context.Context, request *api.SNOVirtualMachine, phase, conditionType string, status metav1.ConditionStatus, reason, message, node string) error {
	request.Status.Phase = phase
	if node != "" && request.Status.Node == "" {
		request.Status.Node = node
	}
	meta.SetStatusCondition(&request.Status.Conditions, metav1.Condition{Type: conditionType, Status: status, Reason: reason, Message: message, ObservedGeneration: request.Generation})
	return r.saveStatus(ctx, request)
}

func (r *InstanceReconciler) saveStatus(ctx context.Context, request *api.SNOVirtualMachine) error {
	latest := &api.SNOVirtualMachine{}
	if err := r.Client.Get(ctx, client.ObjectKeyFromObject(request), latest); err != nil {
		return err
	}
	if reflect.DeepEqual(latest.Status, request.Status) {
		return nil
	}
	latest.Status = request.Status
	return r.Client.Status().Update(ctx, latest)
}

func setMilestone(request *api.SNOVirtualMachine, name string) {
	if request.Status.Milestones == nil {
		request.Status.Milestones = map[string]metav1.Time{}
	}
	if _, ok := request.Status.Milestones[name]; !ok {
		request.Status.Milestones[name] = metav1.Now()
	}
}

func contains(items []string, value string) bool {
	for _, item := range items {
		if item == value {
			return true
		}
	}
	return false
}

func remove(items []string, value string) []string {
	result := items[:0]
	for _, item := range items {
		if item != value {
			result = append(result, item)
		}
	}
	return result
}
