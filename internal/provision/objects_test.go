package provision

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func TestResourceEnsureRejectsCollisionsAndSecretDriftWithoutLeakingContents(t *testing.T) {
	r := allocator(t, "10.252.0.0/16")
	ctx := context.Background()
	req := request()
	immutable := true
	expected := &corev1.Secret{ObjectMeta: metadata(req, "bootstrap"), Immutable: &immutable, Data: map[string][]byte{"config.json": []byte("SENSITIVE_EXPECTED")}}
	if _, err := ensureObject(ctx, r.Client, req, expected); err != nil {
		t.Fatal(err)
	}
	if _, err := ensureObject(ctx, r.Client, req, expected); err != nil {
		t.Fatalf("ordinary retry failed: %v", err)
	}
	actual := &corev1.Secret{}
	if err := r.Client.Get(ctx, types.NamespacedName{Namespace: req.Namespace, Name: "bootstrap"}, actual); err != nil {
		t.Fatal(err)
	}
	actual.Data["config.json"] = []byte("SENSITIVE_CHANGED")
	if err := r.Client.Update(ctx, actual); err != nil {
		t.Fatal(err)
	}
	if _, err := ensureObject(ctx, r.Client, req, expected); err == nil || strings.Contains(err.Error(), "SENSITIVE") {
		t.Fatal("Secret drift accepted or leaked")
	}
	other := request()
	other.Name = req.Name
	other.Namespace = req.Namespace
	if _, err := ensureObject(ctx, r.Client, other, expected); err == nil {
		t.Fatal("adopted resources from an earlier request UID")
	}
}

func TestDNSKeepsPeerAndPlatformConfigurationAndRequiresZoneOwnership(t *testing.T) {
	r := allocator(t, "10.252.0.0/16")
	ctx := context.Background()
	dns := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "sno-guest-dns", Namespace: r.Config.Namespace}, Spec: corev1.ServiceSpec{ClusterIP: "172.30.32.118"}}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: dns.Name, Namespace: dns.Namespace}, Data: map[string]string{"Corefile": "PRESERVE_PLATFORM", "db.peer.apps.example.test": "PRESERVE_PEER"}}
	if err := r.Client.Create(ctx, dns); err != nil {
		t.Fatal(err)
	}
	if err := r.Client.Create(ctx, cm); err != nil {
		t.Fatal(err)
	}
	a := Allocation{UID: uuid.NewString(), ClusterName: "one", BaseDomain: "apps.example.test"}
	if err := r.ensureDNS(ctx, a, "172.30.1.2"); err != nil {
		t.Fatal(err)
	}
	key := types.NamespacedName{Namespace: cm.Namespace, Name: cm.Name}
	if err := r.Client.Get(ctx, key, cm); err != nil {
		t.Fatal(err)
	}
	version := cm.ResourceVersion
	if cm.Data["Corefile"] != "PRESERVE_PLATFORM" || cm.Data["db.peer.apps.example.test"] != "PRESERVE_PEER" {
		t.Fatal("zone update modified peer/platform ownership")
	}
	if !strings.Contains(cm.Data["db."+a.Domain()], "*.apps IN A 172.30.1.2") {
		t.Fatal("guest routes do not point at its access Service")
	}
	if err := r.ensureDNS(ctx, a, "172.30.1.2"); err != nil {
		t.Fatal(err)
	}
	if err := r.Client.Get(ctx, key, cm); err != nil {
		t.Fatal(err)
	}
	if cm.ResourceVersion != version {
		t.Fatal("unchanged DNS causes repeated zone writes")
	}
	different := a
	different.UID = uuid.NewString()
	if err := r.ensureDNS(ctx, different, "172.30.1.3"); err == nil {
		t.Fatal("replaced a peer's DNS zone")
	}
	if err := r.removeDNS(ctx, different); err == nil {
		t.Fatal("deleted a peer's DNS zone")
	}
	if err := r.removeDNS(ctx, a); err != nil {
		t.Fatal(err)
	}
	if err := r.Client.Get(ctx, key, cm); err != nil {
		t.Fatal(err)
	}
	if cm.Data["db."+a.Domain()] != "" || cm.Data["db.peer.apps.example.test"] != "PRESERVE_PEER" {
		t.Fatal("DNS cleanup affected a peer")
	}
}
