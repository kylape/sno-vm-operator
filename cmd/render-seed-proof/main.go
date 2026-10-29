// render-seed-proof produces protected, offline manifests for administrator seed
// qualification. It does not connect to Kubernetes or enable a runtime image.
package main

import (
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
	api "github.com/kylape/sno-vm-operator/api/v1alpha1"
	"github.com/kylape/sno-vm-operator/internal/cache"
	"github.com/kylape/sno-vm-operator/internal/provision"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/yaml"
)

type seed struct {
	SeedID       string `json:"seedID"`
	Release      string `json:"release"`
	ClusterName  string `json:"clusterName"`
	BaseDomain   string `json:"baseDomain"`
	IP           string `json:"ip"`
	SourceCAHash string `json:"sourceCAHash"`
	DiskSize     string `json:"diskSize"`
}

func main() {
	var name, namespace, node, storage, pvc, sourceNamespace, metadata, pull, key, dns, cidr, output string
	flag.StringVar(&name, "name", "", "Fresh disposable proof VM name")
	flag.StringVar(&namespace, "namespace", "sno-seed-build", "Host namespace")
	flag.StringVar(&node, "node", "", "Local seed node")
	flag.StringVar(&storage, "storage-class", "", "Local seed storage class")
	flag.StringVar(&pvc, "source-pvc", "", "Prepared powered-off seed PVC")
	flag.StringVar(&sourceNamespace, "source-namespace", "sno-seed-build", "Seed PVC namespace")
	flag.StringVar(&metadata, "metadata", "", "Healthy seed metadata JSON")
	flag.StringVar(&pull, "pull-secret", "", "Guest pull-secret JSON file")
	flag.StringVar(&key, "ssh-public-key", "", "Administrator public key file")
	flag.StringVar(&dns, "dns", "", "Host guest-DNS Service IPv4 address")
	flag.StringVar(&cidr, "machine-cidr", "", "Unique guest /24 NAT subnet")
	flag.StringVar(&output, "output", "", "New protected output directory")
	flag.Parse()
	if err := render(name, namespace, node, storage, pvc, sourceNamespace, metadata, pull, key, dns, cidr, output); err != nil {
		fmt.Fprintln(os.Stderr, "Seed proof rendering failed:", err)
		os.Exit(1)
	}
}

func render(name, namespace, node, storage, pvc, sourceNamespace, metadata, pull, key, dns, cidr, output string) error {
	if len(name) > 48 || len(validation.IsDNS1123Label(name)) > 0 {
		return fmt.Errorf("fresh proof name must be a DNS label of at most 48 characters")
	}
	for _, v := range []string{namespace, node, storage, pvc, sourceNamespace} {
		if len(validation.IsDNS1123Subdomain(v)) > 0 {
			return fmt.Errorf("invalid proof placement")
		}
	}
	if strings.HasPrefix(node, "master") || strings.Contains(storage, "control-plane") {
		return fmt.Errorf("candidate proofs must use worker-local storage")
	}
	network, err := netip.ParsePrefix(cidr)
	if err != nil || !network.Addr().Is4() || network.Bits() != 24 || network != network.Masked() {
		return fmt.Errorf("proof machine CIDR must be a canonical IPv4 /24")
	}
	for _, protected := range []string{"10.128.0.0/14", "172.30.0.0/16", "10.14.216.0/23", "10.240.0.0/16", "172.31.0.0/16"} {
		if network.Overlaps(netip.MustParsePrefix(protected)) {
			return fmt.Errorf("proof machine CIDR overlaps a protected network")
		}
	}
	content, err := os.ReadFile(metadata)
	if err != nil {
		return fmt.Errorf("cannot read seed metadata")
	}
	var source seed
	if json.Unmarshal(content, &source) != nil || source.Release != "4.22.18" || source.ClusterName == name {
		return fmt.Errorf("invalid source release or reused cluster name")
	}
	if id, err := uuid.Parse(source.SeedID); err != nil || id.String() != source.SeedID {
		return fmt.Errorf("invalid seed identity")
	}
	if source.DiskSize == "" {
		source.DiskSize = "500Gi" // legacy candidate metadata
	}
	address := network.Addr().Next().Next().String()
	if address == source.IP {
		return fmt.Errorf("proof cannot reuse source address")
	}
	pullData, err := os.ReadFile(pull)
	if err != nil {
		return fmt.Errorf("cannot read guest pull secret")
	}
	publicKey, err := os.ReadFile(key)
	if err != nil {
		return fmt.Errorf("cannot read administrator public key")
	}
	id := uuid.NewString()
	hash := sha256.Sum256([]byte(id))
	request := &api.SNOVirtualMachine{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, UID: types.UID(id)}}
	allocation := provision.Allocation{UID: id, Namespace: namespace, Name: name, ClusterName: name, BaseDomain: source.BaseDomain, Hostname: name + "-0", ClusterID: uuid.NewString(), MachineCIDR: cidr, IP: address, MAC: fmt.Sprintf("02:%02x:%02x:%02x:%02x:%02x", hash[0], hash[1], hash[2], hash[3], hash[4]), Node: node, StorageClass: storage, SourcePVC: pvc, Image: cache.Image{Name: "candidate-proof", SeedID: source.SeedID, SourceCAHash: source.SourceCAHash, DiskSize: source.DiskSize}}
	// This administrator-only candidate proof does not promote an image profile.
	// Qualify the supplied identity only for credential rendering, then clear the
	// bit before writing the allocation. No runtime controller consumes this file.
	candidate := allocation
	candidate.Image.Qualified = true
	credentials, err := provision.GenerateCredentials(request, candidate, pullData, publicKey, []string{dns})
	if err != nil {
		return err
	}
	disk, err := provision.RootVolume(request, allocation, sourceNamespace)
	if err != nil {
		return err
	}
	vm, err := provision.VirtualMachine(request, allocation)
	if err != nil {
		return err
	}
	if err = unstructuredSetHalted(vm.Object); err != nil {
		return err
	}
	service := provision.AccessService(request)
	service.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "Service"}
	route := provision.APIRoute(request, allocation)
	credentials.Bootstrap.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"}
	credentials.Management.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"}
	objects := []runtime.Object{disk, service, route, vm}
	for _, object := range objects {
		if accessor, ok := object.(metav1.Object); ok {
			accessor.SetOwnerReferences(nil)
			labels := accessor.GetLabels()
			labels[cache.LabelPrefix+"candidate-proof"] = "true"
			accessor.SetLabels(labels)
		}
	}
	credentials.Bootstrap.OwnerReferences = nil
	credentials.Management.OwnerReferences = nil
	if output == "" {
		return fmt.Errorf("new output directory is required")
	}
	if err = os.Mkdir(output, 0o700); err != nil {
		return fmt.Errorf("output must be a new directory")
	}
	write := func(name string, data []byte) error { return os.WriteFile(filepath.Join(output, name), data, 0o600) }
	for _, file := range []struct {
		name    string
		objects []runtime.Object
	}{{"clone-resources.yaml", objects}, {"bootstrap-secrets.yaml", []runtime.Object{credentials.Management, credentials.Bootstrap}}} {
		var data []byte
		for _, object := range file.objects {
			b, e := yaml.Marshal(object)
			if e != nil {
				return e
			}
			data = append(data, []byte("---\n")...)
			data = append(data, b...)
		}
		if err = write(file.name, data); err != nil {
			return err
		}
	}
	data, err := json.MarshalIndent(allocation, "", "  ")
	if err != nil {
		return err
	}
	if err = write("allocation.json", data); err != nil {
		return err
	}
	fmt.Println("Protected candidate-proof manifests written; no cluster resources or image policy changed")
	return nil
}

func unstructuredSetHalted(object map[string]interface{}) error {
	spec, ok := object["spec"].(map[string]interface{})
	if !ok {
		return fmt.Errorf("VM specification missing")
	}
	spec["runStrategy"] = "Halted"
	return nil
}
