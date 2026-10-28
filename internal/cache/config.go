package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"github.com/google/uuid"
	"net/netip"
	"os"
	"regexp"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/yaml"
)

const ManagedBy = "sno-vm-operator"
const LabelPrefix = "sno.klape.io/"

var pinnedImage = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9./:_-]*@sha256:[a-f0-9]{64}$`)

// Config is administrator-owned policy. Image revisions are never overwritten or
// garbage collected automatically: clones may still depend on their source.
type Config struct {
	Namespace            string         `json:"namespace"`
	BinderImage          string         `json:"binderImage"`
	MaxConcurrentImports int            `json:"maxConcurrentImports"`
	MaxImportAttempts    int            `json:"maxImportAttempts"`
	Placements           []Placement    `json:"placements"`
	Images               []Image        `json:"images"`
	Instances            InstancePolicy `json:"instances,omitempty"`
}

type InstancePolicy struct {
	Enabled            bool   `json:"enabled"`
	DefaultImage       string `json:"defaultImage,omitempty"`
	BaseDomain         string `json:"baseDomain,omitempty"`
	MachinePool        string `json:"machinePool,omitempty"`
	GuestPullSecret    string `json:"guestPullSecret,omitempty"`
	SSHPublicKeySecret string `json:"sshPublicKeySecret,omitempty"`
}

type Placement struct {
	StorageClass string               `json:"storageClass"`
	NodeSelector metav1.LabelSelector `json:"nodeSelector"`
	Tolerations  []corev1.Toleration  `json:"tolerations,omitempty"`
}

type Image struct {
	Name         string `json:"name"`
	Reference    string `json:"reference"`
	Architecture string `json:"architecture"`
	DiskSize     string `json:"diskSize"`
	// CDI pod-pull credentials: accessKeyId and secretKey in the cache namespace.
	RegistrySecret string `json:"registrySecret"`
	// Qualification is an administrator decision after the two-clone proof.
	Qualified    bool   `json:"qualified,omitempty"`
	SeedID       string `json:"seedID,omitempty"`
	SourceCAHash string `json:"sourceCAHash,omitempty"`
}

func Load(path string) (Config, error) {
	var c Config
	b, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	if err = yaml.UnmarshalStrict(b, &c); err != nil {
		return c, err
	}
	return c, c.Validate()
}

func (c Config) Validate() error {
	if errs := validation.IsDNS1123Label(c.Namespace); len(errs) > 0 {
		return fmt.Errorf("namespace: %v", errs)
	}
	if c.MaxConcurrentImports < 1 || c.MaxImportAttempts < 1 {
		return fmt.Errorf("import concurrency and attempts must be positive")
	}
	if !pinnedImage.MatchString(c.BinderImage) {
		return fmt.Errorf("binderImage must be pinned by sha256 digest")
	}
	if len(c.Placements) == 0 {
		return fmt.Errorf("at least one placement is required")
	}
	for _, p := range c.Placements {
		if len(validation.IsDNS1123Subdomain(p.StorageClass)) > 0 {
			return fmt.Errorf("invalid storage class %q", p.StorageClass)
		}
		if len(p.NodeSelector.MatchLabels)+len(p.NodeSelector.MatchExpressions) == 0 {
			return fmt.Errorf("empty node selector is not allowed")
		}
		if _, err := metav1.LabelSelectorAsSelector(&p.NodeSelector); err != nil {
			return err
		}
	}
	names := map[string]bool{}
	for _, i := range c.Images {
		if len(validation.IsDNS1123Label(i.Name)) > 0 || names[i.Name] {
			return fmt.Errorf("invalid or duplicate image name %q", i.Name)
		}
		names[i.Name] = true
		if !pinnedImage.MatchString(i.Reference) {
			return fmt.Errorf("image %s must be pinned by sha256 digest", i.Name)
		}
		if i.Architecture != "amd64" {
			return fmt.Errorf("image %s: initial guest supports amd64 only", i.Name)
		}
		q, err := resource.ParseQuantity(i.DiskSize)
		if err != nil || q.Sign() <= 0 {
			return fmt.Errorf("image %s: invalid disk size", i.Name)
		}
		if len(validation.IsDNS1123Subdomain(i.RegistrySecret)) > 0 {
			return fmt.Errorf("image %s: registrySecret is required", i.Name)
		}
		if i.Qualified {
			if id, err := uuid.Parse(i.SeedID); err != nil || id.String() != i.SeedID {
				return fmt.Errorf("image %s: qualified seed requires a canonical seedID", i.Name)
			}
			if !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(i.SourceCAHash) {
				return fmt.Errorf("image %s: qualified seed requires source CA bundle hash", i.Name)
			}
		}
	}
	if c.Instances.Enabled {
		for _, label := range regexp.MustCompile(`\.`).Split(c.Instances.BaseDomain, -1) {
			if len(validation.IsDNS1123Label(label)) > 0 {
				return fmt.Errorf("invalid instance baseDomain")
			}
		}
		pool, err := netip.ParsePrefix(c.Instances.MachinePool)
		if err != nil || !pool.Addr().Is4() || pool.Bits() < 12 || pool.Bits() > 23 || pool != pool.Masked() {
			return fmt.Errorf("machinePool must be a canonical IPv4 CIDR with /24 allocations")
		}
		for _, protected := range []string{"10.128.0.0/14", "172.30.0.0/16", "10.14.216.0/23", "10.240.0.0/16", "172.31.0.0/16"} {
			if pool.Overlaps(netip.MustParsePrefix(protected)) {
				return fmt.Errorf("machinePool overlaps a protected host or guest network")
			}
		}
		for _, name := range []string{c.Instances.GuestPullSecret, c.Instances.SSHPublicKeySecret} {
			if len(validation.IsDNS1123Subdomain(name)) > 0 {
				return fmt.Errorf("instance credential Secret names are required")
			}
		}
	}
	return nil
}

func (c Config) PlacementFor(node corev1.Node) (*Placement, error) {
	var selected *Placement
	for j := range c.Placements {
		p := &c.Placements[j]
		s, err := metav1.LabelSelectorAsSelector(&p.NodeSelector)
		if err != nil {
			return nil, err
		}
		if s.Matches(labels.Set(node.Labels)) {
			if selected != nil {
				return nil, fmt.Errorf("node %s matches multiple storage placements", node.Name)
			}
			selected = p
		}
	}
	return selected, nil
}

// Key includes the complete source and placement specification. Changing a size,
// digest, architecture or pool creates a separate cache rather than editing data.
func Key(node string, p Placement, image Image) string {
	s := sha256.Sum256([]byte(node + "\x00" + p.StorageClass + "\x00" + image.Reference + "\x00" + image.Architecture + "\x00" + image.DiskSize))
	return "seed-" + hex.EncodeToString(s[:16])
}
