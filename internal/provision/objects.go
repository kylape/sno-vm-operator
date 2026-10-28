package provision

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"time"

	api "github.com/kylape/sno-vm-operator/api/v1alpha1"
	"github.com/kylape/sno-vm-operator/internal/cache"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func ownedBy(object client.Object, request *api.SNOVirtualMachine) bool {
	o := metav1.GetControllerOf(object)
	return o != nil && o.UID == request.UID && o.UID != "" && o.Name == request.Name && o.Kind == "SNOVirtualMachine" && o.APIVersion == api.GroupVersion.String() && object.GetLabels()["app.kubernetes.io/managed-by"] == cache.ManagedBy && object.GetLabels()[cache.LabelPrefix+"instance-uid"] == string(request.UID)
}

// subset permits API defaulting, while protecting every explicitly requested
// field. Mismatch errors deliberately do not include Secret contents.
func subset(expected, actual interface{}) bool {
	switch expected := expected.(type) {
	case map[string]interface{}:
		actual, ok := actual.(map[string]interface{})
		if !ok {
			return false
		}
		for key, value := range expected {
			if !subset(value, actual[key]) {
				return false
			}
		}
		return true
	case []interface{}:
		actual, ok := actual.([]interface{})
		if !ok || len(expected) != len(actual) {
			return false
		}
		for i, value := range expected {
			if !subset(value, actual[i]) {
				return false
			}
		}
		return true
	default:
		return reflect.DeepEqual(expected, actual)
	}
}

func ensureObject(ctx context.Context, c client.Client, request *api.SNOVirtualMachine, expected client.Object) (client.Object, error) {
	actual := expected.DeepCopyObject().(client.Object)
	err := c.Get(ctx, client.ObjectKeyFromObject(expected), actual)
	if apierrors.IsNotFound(err) {
		if err = c.Create(ctx, expected); err != nil {
			return nil, err
		}
		return expected, nil
	}
	if err != nil {
		return nil, err
	}
	if !ownedBy(actual, request) || actual.GetDeletionTimestamp() != nil {
		return nil, fmt.Errorf("resource collision or deletion: %s/%s", actual.GetNamespace(), actual.GetName())
	}
	e, err := runtime.DefaultUnstructuredConverter.ToUnstructured(expected)
	if err != nil {
		return nil, err
	}
	a, err := runtime.DefaultUnstructuredConverter.ToUnstructured(actual)
	if err != nil {
		return nil, err
	}
	if !subset(e["spec"], a["spec"]) {
		return nil, fmt.Errorf("owned resource specification changed: %s/%s", actual.GetNamespace(), actual.GetName())
	}
	if _, ok := expected.(*corev1.Secret); ok {
		if !reflect.DeepEqual(e["data"], a["data"]) || !reflect.DeepEqual(e["immutable"], a["immutable"]) {
			return nil, fmt.Errorf("owned bootstrap Secret changed")
		}
	}
	return actual, nil
}

func (r Allocator) guestDNS(ctx context.Context) (string, error) {
	service := &corev1.Service{}
	if err := r.Client.Get(ctx, types.NamespacedName{Namespace: r.Config.Namespace, Name: "sno-guest-dns"}, service); err != nil {
		return "", err
	}
	if service.Spec.ClusterIP == "" || service.Spec.ClusterIP == corev1.ClusterIPNone {
		return "", fmt.Errorf("guest DNS Service is not allocated")
	}
	return service.Spec.ClusterIP, nil
}

func (r Allocator) ensureDNS(ctx context.Context, a Allocation, ip string) error {
	dns, err := r.guestDNS(ctx)
	if err != nil {
		return err
	}
	cm := &corev1.ConfigMap{}
	if err = r.Client.Get(ctx, types.NamespacedName{Namespace: r.Config.Namespace, Name: "sno-guest-dns"}, cm); err != nil {
		return err
	}
	if cm.Data["Corefile"] == "" {
		return fmt.Errorf("guest DNS platform configuration is missing")
	}
	key := "db." + a.Domain()
	marker := "; sno-instance-uid=" + a.UID + "\n"
	existing := cm.Data[key]
	if existing != "" && !strings.HasPrefix(existing, marker) {
		return fmt.Errorf("guest DNS zone is owned by another cluster")
	}
	// Stable records avoid restarting zone scans on every reconciliation.
	records := fmt.Sprintf("api IN A %s\napi-int IN A %s\n*.apps IN A %s\n", ip, ip, ip)
	if strings.Contains(existing, records) && strings.Contains(existing, "ns IN A "+dns+"\n") {
		return nil
	}
	zone := fmt.Sprintf("%s$ORIGIN %s.\n$TTL 1\n@ IN SOA ns.%s. hostmaster.%s. (%d 5 5 30 1)\n@ IN NS ns.%s.\nns IN A %s\n%s", marker, a.Domain(), a.Domain(), a.Domain(), time.Now().Unix(), a.Domain(), dns, records)
	cm.Data[key] = zone
	return r.Client.Update(ctx, cm)
}

func (r Allocator) removeDNS(ctx context.Context, a Allocation) error {
	cm := &corev1.ConfigMap{}
	if err := r.Client.Get(ctx, types.NamespacedName{Namespace: r.Config.Namespace, Name: "sno-guest-dns"}, cm); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	key := "db." + a.Domain()
	if cm.Data[key] == "" {
		return nil
	}
	if !strings.HasPrefix(cm.Data[key], "; sno-instance-uid="+a.UID+"\n") {
		return fmt.Errorf("refusing to remove another cluster's DNS zone")
	}
	delete(cm.Data, key)
	return r.Client.Update(ctx, cm)
}
