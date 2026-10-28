package provision

import (
	"context"
	"testing"

	"github.com/google/uuid"
	api "github.com/kylape/sno-vm-operator/api/v1alpha1"
	"github.com/kylape/sno-vm-operator/internal/cache"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func allocator(t *testing.T, pool string) Allocator {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return Allocator{Client: fake.NewClientBuilder().WithScheme(scheme).Build(), Config: cache.Config{Namespace: "sno-vm-system", Instances: cache.InstancePolicy{BaseDomain: "apps.example.test", MachinePool: pool}}}
}
func request() *api.SNOVirtualMachine {
	return &api.SNOVirtualMachine{ObjectMeta: metav1.ObjectMeta{Name: "sno", Namespace: "env-one", UID: types.UID(uuid.NewString())}}
}
func source() cache.Entry {
	return cache.Entry{Node: "worker-one", StorageClass: "local-lvm", SourcePVC: "seed-one"}
}

func TestAllocationRetryPreservesIdentityAndImage(t *testing.T) {
	r := allocator(t, "10.252.0.0/16")
	ctx := context.Background()
	req := request()
	image := cache.Image{Name: "v1", Reference: "registry.test/image@sha256:original"}
	first, err := r.Create(ctx, req, image, source())
	if err != nil {
		t.Fatal(err)
	}
	retry, err := r.Create(ctx, req, cache.Image{Name: "v2"}, cache.Entry{Node: "other"})
	if err != nil {
		t.Fatal(err)
	}
	if *first != *retry || retry.MachineCIDR != "10.252.1.0/24" || retry.IP != "10.252.1.2" {
		t.Fatalf("allocation changed on retry: %#v", retry)
	}
	got, err := r.Get(ctx, req)
	if err != nil || got.ClusterID != first.ClusterID {
		t.Fatalf("identity was not durable: %v", err)
	}
	req.Namespace = "env-other"
	if _, err = r.Get(ctx, req); err == nil {
		t.Fatal("accepted allocation owned by another namespace")
	}
}

func TestIndependentAllocationsAndExplicitRelease(t *testing.T) {
	r := allocator(t, "10.252.0.0/16")
	ctx := context.Background()
	one, two := request(), request()
	a, err := r.Create(ctx, one, cache.Image{}, source())
	if err != nil {
		t.Fatal(err)
	}
	b, err := r.Create(ctx, two, cache.Image{}, source())
	if err != nil {
		t.Fatal(err)
	}
	if a.MAC == b.MAC || a.IP == b.IP || a.ClusterID == b.ClusterID || a.Hostname == b.Hostname || a.Domain() == b.Domain() {
		t.Fatal("two guests shared an identity")
	}
	if err = r.Release(ctx, string(one.UID)); err != nil {
		t.Fatal(err)
	}
	got, err := r.Get(ctx, one)
	if err != nil || got != nil {
		t.Fatalf("release failed: %v", err)
	}
	got, err = r.Get(ctx, two)
	if err != nil || got.ClusterID != b.ClusterID {
		t.Fatal("release affected peer")
	}
}

func TestAddressExhaustionAndNameCollisionFailClosed(t *testing.T) {
	r := allocator(t, "10.252.0.0/23")
	ctx := context.Background()
	one := request()
	if _, err := r.Create(ctx, one, cache.Image{}, source()); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Create(ctx, request(), cache.Image{}, source()); err == nil {
		t.Fatal("accepted exhausted pool")
	}
	r = allocator(t, "10.252.0.0/16")
	one, two := request(), request()
	one.Spec.ClusterName = "same"
	two.Spec.ClusterName = "same"
	if _, err := r.Create(ctx, one, cache.Image{}, source()); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Create(ctx, two, cache.Image{}, source()); err == nil {
		t.Fatal("accepted duplicate domain")
	}
}

func TestInvalidPoolsReturnErrorsWithoutPanicking(t *testing.T) {
	for _, pool := range []string{"invalid", "fd00::/16", "10.252.0.0/25", "10.252.1.0/16"} {
		t.Run(pool, func(t *testing.T) {
			r := allocator(t, pool)
			if _, err := r.Create(context.Background(), request(), cache.Image{}, source()); err == nil {
				t.Fatal("accepted invalid pool")
			}
		})
	}
}
