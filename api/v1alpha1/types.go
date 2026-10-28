// Package v1alpha1 defines the provisioning boundary consumed by LWE.
package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var GroupVersion = schema.GroupVersion{Group: "sno.klape.io", Version: "v1alpha1"}

func AddToScheme(s *runtime.Scheme) error {
	s.AddKnownTypes(GroupVersion, &SNOVirtualMachine{}, &SNOVirtualMachineList{})
	metav1.AddToGroupVersion(s, GroupVersion)
	return nil
}

type Resources struct {
	Cores    int32  `json:"cores,omitempty"`
	Memory   string `json:"memory,omitempty"`
	RootDisk string `json:"rootDisk,omitempty"`
}

// Spec is immutable. Established identity and root storage must never change
// during retries, profile rollout, or a controller restart.
type SNOVirtualMachineSpec struct {
	ImageProfile            string                       `json:"imageProfile,omitempty"`
	ClusterName             string                       `json:"clusterName,omitempty"`
	Resources               Resources                    `json:"resources,omitempty"`
	RootDiskRetentionPolicy string                       `json:"rootDiskRetentionPolicy,omitempty"`
	SSHPublicKeySecretRef   *corev1.LocalObjectReference `json:"sshPublicKeySecretRef,omitempty"`
}

type SNOVirtualMachineStatus struct {
	Phase                string                       `json:"phase,omitempty"`
	ImageDigest          string                       `json:"imageDigest,omitempty"`
	ClusterID            string                       `json:"clusterID,omitempty"`
	Node                 string                       `json:"node,omitempty"`
	VMRef                *corev1.LocalObjectReference `json:"vmRef,omitempty"`
	RootPVCRef           *corev1.LocalObjectReference `json:"rootPVCRef,omitempty"`
	CredentialsSecretRef *corev1.LocalObjectReference `json:"credentialsSecretRef,omitempty"`
	// APIEndpoint is the external HTTPS Route URL; credentials must validate the guest CA.
	APIEndpoint string             `json:"apiEndpoint,omitempty"`
	Conditions  []metav1.Condition `json:"conditions,omitempty"`
	// Milestones are wall-clock timestamps, including the unadjusted request time.
	Milestones map[string]metav1.Time `json:"milestones,omitempty"`
}

type SNOVirtualMachine struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              SNOVirtualMachineSpec   `json:"spec,omitempty"`
	Status            SNOVirtualMachineStatus `json:"status,omitempty"`
}

type SNOVirtualMachineList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SNOVirtualMachine `json:"items"`
}

func (in *SNOVirtualMachine) DeepCopyInto(out *SNOVirtualMachine) {
	*out = *in
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	if in.Spec.SSHPublicKeySecretRef != nil {
		copy := *in.Spec.SSHPublicKeySecretRef
		out.Spec.SSHPublicKeySecretRef = &copy
	}
	if in.Status.VMRef != nil {
		copy := *in.Status.VMRef
		out.Status.VMRef = &copy
	}
	if in.Status.RootPVCRef != nil {
		copy := *in.Status.RootPVCRef
		out.Status.RootPVCRef = &copy
	}
	if in.Status.CredentialsSecretRef != nil {
		copy := *in.Status.CredentialsSecretRef
		out.Status.CredentialsSecretRef = &copy
	}
	if in.Status.Conditions != nil {
		out.Status.Conditions = append([]metav1.Condition{}, in.Status.Conditions...)
	}
	if in.Status.Milestones != nil {
		out.Status.Milestones = make(map[string]metav1.Time, len(in.Status.Milestones))
		for k, v := range in.Status.Milestones {
			out.Status.Milestones[k] = v
		}
	}
}

func (in *SNOVirtualMachine) DeepCopy() *SNOVirtualMachine {
	if in == nil {
		return nil
	}
	out := new(SNOVirtualMachine)
	in.DeepCopyInto(out)
	return out
}
func (in *SNOVirtualMachine) DeepCopyObject() runtime.Object { return in.DeepCopy() }
func (in *SNOVirtualMachineList) DeepCopy() *SNOVirtualMachineList {
	if in == nil {
		return nil
	}
	out := new(SNOVirtualMachineList)
	*out = *in
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]SNOVirtualMachine, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
	return out
}
func (in *SNOVirtualMachineList) DeepCopyObject() runtime.Object { return in.DeepCopy() }
