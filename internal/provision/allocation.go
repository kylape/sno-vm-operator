package provision

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/netip"

	"github.com/google/uuid"
	api "github.com/kylape/sno-vm-operator/api/v1alpha1"
	"github.com/kylape/sno-vm-operator/internal/cache"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const allocationName = "sno-instance-allocations"

// Allocation contains no credentials. Persist it before creating disks or VMs;
// neither status-update failures nor controller restarts may change identity.
type Allocation struct {
	UID          string      `json:"uid"`
	Namespace    string      `json:"namespace"`
	Name         string      `json:"name"`
	ClusterName  string      `json:"clusterName"`
	BaseDomain   string      `json:"baseDomain"`
	Hostname     string      `json:"hostname"`
	ClusterID    string      `json:"clusterID"`
	MachineCIDR  string      `json:"machineCIDR"`
	IP           string      `json:"ip"`
	MAC          string      `json:"mac"`
	Node         string      `json:"node"`
	StorageClass string      `json:"storageClass"`
	SourcePVC    string      `json:"sourcePVC"`
	Image        cache.Image `json:"image"`
}

func (a Allocation) Domain() string   { return a.ClusterName + "." + a.BaseDomain }
func (a Allocation) Endpoint() string { return "https://api." + a.Domain() }

type Allocator struct {
	Client client.Client
	Config cache.Config
}

func (r Allocator) load(ctx context.Context) (*corev1.ConfigMap, map[string]Allocation, error) {
	cm := &corev1.ConfigMap{}
	err := r.Client.Get(ctx, types.NamespacedName{Namespace: r.Config.Namespace, Name: allocationName}, cm)
	if apierrors.IsNotFound(err) {
		cm = &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: allocationName, Namespace: r.Config.Namespace, Labels: map[string]string{"app.kubernetes.io/managed-by": cache.ManagedBy}}}
		if err = r.Client.Create(ctx, cm); err != nil {
			return nil, nil, err
		}
	} else if err != nil {
		return nil, nil, err
	}
	if cm.Labels["app.kubernetes.io/managed-by"] != cache.ManagedBy {
		return nil, nil, fmt.Errorf("allocation ledger ownership mismatch")
	}
	entries := map[string]Allocation{}
	if raw := cm.Data["allocations.json"]; raw != "" {
		if err = json.Unmarshal([]byte(raw), &entries); err != nil {
			return nil, nil, fmt.Errorf("invalid allocation ledger")
		}
	}
	return cm, entries, nil
}

func (r Allocator) Get(ctx context.Context, request *api.SNOVirtualMachine) (*Allocation, error) {
	_, entries, err := r.load(ctx)
	if err != nil {
		return nil, err
	}
	a, ok := entries[string(request.UID)]
	if !ok {
		return nil, nil
	}
	if a.Namespace != request.Namespace || a.Name != request.Name || a.UID != string(request.UID) {
		return nil, fmt.Errorf("allocation identity mismatch")
	}
	return &a, nil
}

func (r Allocator) Create(ctx context.Context, request *api.SNOVirtualMachine, image cache.Image, source cache.Entry) (*Allocation, error) {
	cm, entries, err := r.load(ctx)
	if err != nil {
		return nil, err
	}
	uid := string(request.UID)
	if existing, ok := entries[uid]; ok {
		if existing.Namespace != request.Namespace || existing.Name != request.Name {
			return nil, fmt.Errorf("allocation ownership mismatch")
		}
		return &existing, nil
	}
	parsed, err := uuid.Parse(uid)
	if err != nil || parsed.String() != uid {
		return nil, fmt.Errorf("request requires a canonical Kubernetes UID")
	}
	name := request.Spec.ClusterName
	if name == "" {
		name = "sno-" + uid[:12]
	}
	hash := sha256.Sum256([]byte(uid))
	mac := fmt.Sprintf("02:%02x:%02x:%02x:%02x:%02x", hash[0], hash[1], hash[2], hash[3], hash[4])
	used := map[string]bool{}
	for _, a := range entries {
		used[a.MachineCIDR] = true
		if (a.ClusterName == name && a.BaseDomain == r.Config.Instances.BaseDomain) || a.MAC == mac {
			return nil, fmt.Errorf("cluster name or MAC allocation collision")
		}
	}
	pool, err := netip.ParsePrefix(r.Config.Instances.MachinePool)
	if err != nil {
		return nil, err
	}
	baseBytes := pool.Addr().As4()
	base := binary.BigEndian.Uint32(baseBytes[:])
	var chosen netip.Prefix
	// Reserve the first /24 for the disposable reference build.
	for index := uint32(1); index < (1 << uint(24-pool.Bits())); index++ {
		var b [4]byte
		binary.BigEndian.PutUint32(b[:], base+(index<<8))
		prefix := netip.PrefixFrom(netip.AddrFrom4(b), 24)
		if !used[prefix.String()] {
			chosen = prefix
			break
		}
	}
	if !chosen.IsValid() {
		return nil, fmt.Errorf("guest address pool exhausted")
	}
	a := Allocation{UID: uid, Namespace: request.Namespace, Name: request.Name, ClusterName: name, BaseDomain: r.Config.Instances.BaseDomain, Hostname: "sno-" + hex.EncodeToString(hash[:8]), ClusterID: uuid.NewString(), MachineCIDR: chosen.String(), IP: chosen.Addr().Next().Next().String(), MAC: mac, Node: source.Node, StorageClass: source.StorageClass, SourcePVC: source.SourcePVC, Image: image}
	entries[uid] = a
	b, err := json.Marshal(entries)
	if err != nil {
		return nil, err
	}
	cm.Data = map[string]string{"allocations.json": string(b)}
	// A resourceVersion conflict is retried by reconciliation. No allocation is
	// returned until its identity is durable, so concurrent creates cannot share it.
	if err = r.Client.Update(ctx, cm); err != nil {
		return nil, err
	}
	return &a, nil
}

func (r Allocator) Release(ctx context.Context, uid string) error {
	cm, entries, err := r.load(ctx)
	if err != nil {
		return err
	}
	delete(entries, uid)
	b, err := json.Marshal(entries)
	if err != nil {
		return err
	}
	cm.Data = map[string]string{"allocations.json": string(b)}
	return r.Client.Update(ctx, cm)
}
