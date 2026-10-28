package cache

import (
	"fmt"
	"reflect"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var DataVolumeGVK = schema.GroupVersionKind{Group: "cdi.kubevirt.io", Version: "v1beta1", Kind: "DataVolume"}

func DataVolume(namespace, node string, p Placement, image Image) *unstructured.Unstructured {
	name := Key(node, p, image)
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "cdi.kubevirt.io/v1beta1", "kind": "DataVolume",
		"metadata": map[string]interface{}{
			"name": name, "namespace": namespace,
			"labels":      map[string]interface{}{"app.kubernetes.io/managed-by": ManagedBy, LabelPrefix + "cache-key": name},
			"annotations": map[string]interface{}{LabelPrefix + "node": node, LabelPrefix + "image": image.Reference},
		},
		"spec": map[string]interface{}{
			"source":  map[string]interface{}{"registry": map[string]interface{}{"url": "docker://" + image.Reference, "secretRef": image.RegistrySecret, "pullMethod": "pod"}},
			"storage": map[string]interface{}{"storageClassName": p.StorageClass, "volumeMode": "Block", "accessModes": []interface{}{"ReadWriteOnce"}, "resources": map[string]interface{}{"requests": map[string]interface{}{"storage": image.DiskSize}}},
		},
	}}
}

// Binder asks the scheduler to select the target node for WFFC. It never reads
// the block device; CDI retains exclusive responsibility for writing the disk.
func Binder(namespace, node, name, image string, tolerations []corev1.Toleration) *corev1.Pod {
	falseValue, trueValue := false, true
	uid := int64(65532)
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name + "-bind", Namespace: namespace, Labels: map[string]string{"app.kubernetes.io/managed-by": ManagedBy, LabelPrefix + "cache-key": name}},
		Spec: corev1.PodSpec{
			AutomountServiceAccountToken: &falseValue,
			NodeSelector:                 map[string]string{"kubernetes.io/hostname": node},
			Tolerations:                  tolerations,
			RestartPolicy:                corev1.RestartPolicyAlways,
			SecurityContext:              &corev1.PodSecurityContext{RunAsNonRoot: &trueValue, RunAsUser: &uid, SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}},
			Containers: []corev1.Container{{Name: "bind", Image: image, Command: []string{"/bin/sh", "-c", "sleep infinity"},
				SecurityContext: &corev1.SecurityContext{AllowPrivilegeEscalation: &falseValue, ReadOnlyRootFilesystem: &trueValue, Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}},
				Resources:       corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1m"), corev1.ResourceMemory: resource.MustParse("16Mi")}, Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("32Mi")}},
				VolumeDevices:   []corev1.VolumeDevice{{Name: "seed", DevicePath: "/dev/seed"}},
			}},
			Volumes: []corev1.Volume{{Name: "seed", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: name}}}},
		},
	}
}

func ValidateExisting(actual, expected *unstructured.Unstructured) error {
	if actual.GetLabels()["app.kubernetes.io/managed-by"] != ManagedBy ||
		actual.GetLabels()[LabelPrefix+"cache-key"] != expected.GetName() ||
		actual.GetAnnotations()[LabelPrefix+"node"] != expected.GetAnnotations()[LabelPrefix+"node"] ||
		actual.GetAnnotations()[LabelPrefix+"image"] != expected.GetAnnotations()[LabelPrefix+"image"] {
		return fmt.Errorf("cache name collision or ownership mismatch: %s", actual.GetName())
	}
	// Compare only our specified fields. CDI can default additional fields.
	for _, path := range [][]string{{"spec", "source", "registry", "url"}, {"spec", "source", "registry", "secretRef"}, {"spec", "storage", "storageClassName"}, {"spec", "storage", "volumeMode"}, {"spec", "storage", "resources", "requests", "storage"}} {
		a, _, _ := unstructured.NestedFieldNoCopy(actual.Object, path...)
		e, _, _ := unstructured.NestedFieldNoCopy(expected.Object, path...)
		if !reflect.DeepEqual(a, e) {
			return fmt.Errorf("cache %s has changed field %v", actual.GetName(), path)
		}
	}
	return nil
}

// Local sources must be explicitly pinned by every OR term. A broadly usable
// volume or a PV on the wrong node must never count toward local cache coverage.
func PinnedToNode(pv *corev1.PersistentVolume, node string) bool {
	if pv.Spec.NodeAffinity == nil || pv.Spec.NodeAffinity.Required == nil {
		return false
	}
	terms := pv.Spec.NodeAffinity.Required.NodeSelectorTerms
	if len(terms) == 0 {
		return false
	}
	for _, term := range terms {
		pinned := false
		for _, expr := range term.MatchExpressions {
			if (expr.Key == "kubernetes.io/hostname" || expr.Key == "topology.topolvm.io/node") && expr.Operator == corev1.NodeSelectorOpIn && len(expr.Values) == 1 && expr.Values[0] == node {
				pinned = true
			}
		}
		if !pinned {
			return false
		}
	}
	return true
}
