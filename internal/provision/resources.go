package provision

import (
	"fmt"
	"strconv"

	api "github.com/kylape/sno-vm-operator/api/v1alpha1"
	"github.com/kylape/sno-vm-operator/internal/cache"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
)

var VirtualMachineGVK = schema.GroupVersionKind{Group: "kubevirt.io", Version: "v1", Kind: "VirtualMachine"}
var RouteGVK = schema.GroupVersionKind{Group: "route.openshift.io", Version: "v1", Kind: "Route"}

func owner(request *api.SNOVirtualMachine) metav1.OwnerReference {
	yes := true
	return metav1.OwnerReference{APIVersion: api.GroupVersion.String(), Kind: "SNOVirtualMachine", Name: request.Name, UID: request.UID, Controller: &yes, BlockOwnerDeletion: &yes}
}
func metadata(request *api.SNOVirtualMachine, name string) metav1.ObjectMeta {
	return metav1.ObjectMeta{Name: name, Namespace: request.Namespace, OwnerReferences: []metav1.OwnerReference{owner(request)}, Labels: map[string]string{"app.kubernetes.io/managed-by": cache.ManagedBy, cache.LabelPrefix + "instance-uid": string(request.UID)}}
}
func stamp(object *unstructured.Unstructured, request *api.SNOVirtualMachine, name string) *unstructured.Unstructured {
	object.SetName(name)
	object.SetNamespace(request.Namespace)
	object.SetOwnerReferences([]metav1.OwnerReference{owner(request)})
	object.SetLabels(metadata(request, name).Labels)
	return object
}

// Hardware defaults match the initial LWE contract. In particular, never request
// a disk smaller than the immutable imported source's virtual capacity.
func Hardware(request *api.SNOVirtualMachine, a Allocation) (api.Resources, error) {
	result := request.Spec.Resources
	if result.Cores == 0 {
		result.Cores = 16
	}
	if result.Memory == "" {
		result.Memory = "128Gi"
	}
	if result.RootDisk == "" {
		result.RootDisk = "500Gi"
	}
	memory, err := resource.ParseQuantity(result.Memory)
	if err != nil || memory.Cmp(resource.MustParse("16Gi")) < 0 || result.Cores < 8 {
		return result, fmt.Errorf("SNO requires at least eight cores and 16Gi memory")
	}
	disk, err := resource.ParseQuantity(result.RootDisk)
	source, sourceErr := resource.ParseQuantity(a.Image.DiskSize)
	if err != nil || sourceErr != nil || source.Sign() <= 0 || disk.Cmp(source) < 0 {
		return result, fmt.Errorf("root disk must be at least the source virtual disk size")
	}
	return result, nil
}

func RootVolume(request *api.SNOVirtualMachine, a Allocation, cacheNamespace string) (*unstructured.Unstructured, error) {
	hardware, err := Hardware(request, a)
	if err != nil {
		return nil, err
	}
	if a.Node == "" || a.StorageClass == "" || a.SourcePVC == "" || cacheNamespace == "" {
		return nil, fmt.Errorf("local clone source placement is incomplete")
	}
	object := &unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": cache.DataVolumeGVK.GroupVersion().String(), "kind": cache.DataVolumeGVK.Kind, "spec": map[string]interface{}{
		"source":  map[string]interface{}{"pvc": map[string]interface{}{"namespace": cacheNamespace, "name": a.SourcePVC}},
		"storage": map[string]interface{}{"storageClassName": a.StorageClass, "volumeMode": "Block", "accessModes": []interface{}{"ReadWriteOnce"}, "resources": map[string]interface{}{"requests": map[string]interface{}{"storage": hardware.RootDisk}}},
	}}}
	return stamp(object, request, request.Name+"-root"), nil
}

func VirtualMachine(request *api.SNOVirtualMachine, a Allocation) (*unstructured.Unstructured, error) {
	hardware, err := Hardware(request, a)
	if err != nil {
		return nil, err
	}
	if a.UID != string(request.UID) || a.Name != request.Name || a.Namespace != request.Namespace || a.Node == "" {
		return nil, fmt.Errorf("VM allocation identity or placement mismatch")
	}
	object := &unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": VirtualMachineGVK.GroupVersion().String(), "kind": VirtualMachineGVK.Kind, "spec": map[string]interface{}{
		"runStrategy": "Always",
		"template": map[string]interface{}{"metadata": map[string]interface{}{"labels": map[string]interface{}{cache.LabelPrefix + "instance-uid": a.UID}}, "spec": map[string]interface{}{
			"architecture": "amd64", "nodeSelector": map[string]interface{}{"kubernetes.io/hostname": a.Node, "kubernetes.io/arch": "amd64"}, "terminationGracePeriodSeconds": int64(180),
			"domain": map[string]interface{}{
				"firmware":  map[string]interface{}{"uuid": a.UID},
				"cpu":       map[string]interface{}{"cores": int64(hardware.Cores), "sockets": int64(1), "threads": int64(1)},
				"memory":    map[string]interface{}{"guest": hardware.Memory},
				"resources": map[string]interface{}{"requests": map[string]interface{}{"cpu": strconv.Itoa(int(hardware.Cores)), "memory": hardware.Memory}},
				"devices": map[string]interface{}{
					"rng":        map[string]interface{}{},
					"disks":      []interface{}{map[string]interface{}{"name": "root", "disk": map[string]interface{}{"bus": "virtio"}, "bootOrder": int64(1)}, map[string]interface{}{"name": "bootstrap", "cdrom": map[string]interface{}{"bus": "sata", "readonly": true}}},
					"interfaces": []interface{}{map[string]interface{}{"name": "machine", "masquerade": map[string]interface{}{}, "model": "virtio", "macAddress": a.MAC}},
				},
			},
			"networks": []interface{}{map[string]interface{}{"name": "machine", "pod": map[string]interface{}{"vmNetworkCIDR": a.MachineCIDR}}},
			"volumes":  []interface{}{map[string]interface{}{"name": "root", "dataVolume": map[string]interface{}{"name": request.Name + "-root"}}, map[string]interface{}{"name": "bootstrap", "secret": map[string]interface{}{"secretName": request.Name + "-bootstrap", "volumeLabel": "SNO-CONFIG"}}},
		}},
	}}}
	return stamp(object, request, request.Name), nil
}

func AccessService(request *api.SNOVirtualMachine) *corev1.Service {
	service := &corev1.Service{ObjectMeta: metadata(request, request.Name+"-access"), Spec: corev1.ServiceSpec{
		Selector: map[string]string{cache.LabelPrefix + "instance-uid": string(request.UID)}, PublishNotReadyAddresses: true,
	}}
	for _, p := range []struct {
		name string
		port int32
	}{{"api", 6443}, {"http", 80}, {"https", 443}, {"management", 22}} {
		service.Spec.Ports = append(service.Spec.Ports, corev1.ServicePort{Name: p.name, Port: p.port, TargetPort: intstr.FromInt32(p.port), Protocol: corev1.ProtocolTCP})
	}
	return service
}

func APIRoute(request *api.SNOVirtualMachine, a Allocation) *unstructured.Unstructured {
	object := &unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": RouteGVK.GroupVersion().String(), "kind": RouteGVK.Kind, "spec": map[string]interface{}{
		"host": "api." + a.Domain(), "to": map[string]interface{}{"kind": "Service", "name": request.Name + "-access"}, "port": map[string]interface{}{"targetPort": "api"},
		"tls": map[string]interface{}{"termination": "passthrough", "insecureEdgeTerminationPolicy": "None"}, "wildcardPolicy": "None",
	}}}
	stamp(object, request, request.Name+"-api")
	object.SetAnnotations(map[string]string{"haproxy.router.openshift.io/timeout": "5m"})
	return object
}
