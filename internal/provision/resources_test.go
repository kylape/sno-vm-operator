package provision

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kylape/sno-vm-operator/internal/cache"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestCloneAndVMKeepDurableLocalPlacementAndSeparateCredentials(t *testing.T) {
	r := allocator(t, "10.252.0.0/16")
	req := request()
	a, err := r.Create(context.Background(), req, cache.Image{DiskSize: "500Gi"}, source())
	if err != nil {
		t.Fatal(err)
	}
	disk, err := RootVolume(req, *a, "sno-vm-system")
	if err != nil {
		t.Fatal(err)
	}
	ns, _, _ := unstructured.NestedString(disk.Object, "spec", "source", "pvc", "namespace")
	pvc, _, _ := unstructured.NestedString(disk.Object, "spec", "source", "pvc", "name")
	class, _, _ := unstructured.NestedString(disk.Object, "spec", "storage", "storageClassName")
	if ns != "sno-vm-system" || pvc != a.SourcePVC || class != a.StorageClass {
		t.Fatal("clone no longer uses allocated local cache")
	}
	vm, err := VirtualMachine(req, *a)
	if err != nil {
		t.Fatal(err)
	}
	node, _, _ := unstructured.NestedString(vm.Object, "spec", "template", "spec", "nodeSelector", "kubernetes.io/hostname")
	if node != a.Node {
		t.Fatal("VM can schedule away from its local source")
	}
	data, err := json.Marshal(vm)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"-management", "kubeconfig", "cloudInit", "registry.test", "sshPrivateKey"} {
		if strings.Contains(string(data), forbidden) {
			t.Fatalf("VM exposes forbidden credential source: %s", forbidden)
		}
	}
	volumes, _, _ := unstructured.NestedSlice(vm.Object, "spec", "template", "spec", "volumes")
	label, _, _ := unstructured.NestedString(volumes[1].(map[string]interface{}), "secret", "volumeLabel")
	if label != "SNO-CONFIG" {
		t.Fatal("guest gate cannot discover its configuration media")
	}
	if len(vm.GetOwnerReferences()) != 1 || vm.GetOwnerReferences()[0].UID != req.UID {
		t.Fatal("VM lifecycle is not bound to request UID")
	}
}

func TestSmallerRootAndChangedIdentityAreRejected(t *testing.T) {
	r := allocator(t, "10.252.0.0/16")
	req := request()
	a, err := r.Create(context.Background(), req, cache.Image{DiskSize: "500Gi"}, source())
	if err != nil {
		t.Fatal(err)
	}
	req.Spec.Resources.RootDisk = "100Gi"
	if _, err = RootVolume(req, *a, "sno-vm-system"); err == nil {
		t.Fatal("allowed truncation of source virtual disk")
	}
	req.Spec.Resources.RootDisk = "500Gi"
	req.UID = "different"
	if _, err = VirtualMachine(req, *a); err == nil {
		t.Fatal("allowed VM identity replacement")
	}
}

func TestExternalAPIKeepsGuestTLSAndInternalApplicationsHaveNoHostRoutes(t *testing.T) {
	req := request()
	a := Allocation{ClusterName: "sno-one", BaseDomain: "apps.example.test"}
	route := APIRoute(req, a)
	tls, _, _ := unstructured.NestedMap(route.Object, "spec", "tls")
	if tls["termination"] != "passthrough" || tls["certificate"] != nil || tls["key"] != nil {
		t.Fatal("route terminates guest TLS")
	}
	host, _, _ := unstructured.NestedString(route.Object, "spec", "host")
	if host != "api.sno-one.apps.example.test" || a.Endpoint() != "https://"+host {
		t.Fatal("external endpoint does not match route")
	}
	service := AccessService(req)
	if service.Spec.Selector[cache.LabelPrefix+"instance-uid"] != string(req.UID) {
		t.Fatal("service can select a peer VM")
	}
	if service.Spec.Type != "" || service.Spec.LoadBalancerIP != "" {
		t.Fatal("unexpected external application publication")
	}
	if len(service.Spec.Ports) != 4 {
		t.Fatal("missing internal application or management path")
	}
}
