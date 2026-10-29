package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/kylape/sno-vm-operator/internal/provision"
	"golang.org/x/crypto/ssh"
)

func TestProofRenderingStaysUnqualifiedAndNeverOverwritesAnEstablishedIdentity(t *testing.T) {
	dir := t.TempDir()
	meta, pull, key := filepath.Join(dir, "metadata.json"), filepath.Join(dir, "pull.json"), filepath.Join(dir, "key.pub")
	data, _ := json.Marshal(seed{SeedID: uuid.NewString(), Release: "4.22.18", ClusterName: "seed", BaseDomain: "apps.example.test", IP: "10.252.0.2", DiskSize: "50Gi"})
	if err := os.WriteFile(meta, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pull, []byte(`{"auths":{"test.invalid":{"auth":"TEST_ONLY"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(key, ssh.MarshalAuthorizedKey(signer.PublicKey()), 0o600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "proof")
	run := func(node, storage, cidr string) error {
		return render("proof-one", "sno-seed-build", node, storage, "source-root", "sno-seed-build", meta, pull, key, "172.30.32.118", cidr, out)
	}
	if err = run("master1", "lvms-control-plane-storage", "10.252.1.0/24"); err == nil {
		t.Fatal("allowed control-plane proof")
	}
	if err = run("worker-one", "local-lvm", "172.30.1.0/24"); err == nil {
		t.Fatal("allowed host service network overlap")
	}
	if err = run("worker-one", "local-lvm", "10.252.1.0/24"); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(filepath.Join(out, "allocation.json"))
	if err != nil {
		t.Fatal(err)
	}
	var allocation provision.Allocation
	if err = json.Unmarshal(original, &allocation); err != nil {
		t.Fatal(err)
	}
	if allocation.Image.DiskSize != "50Gi" {
		t.Fatalf("lost compact source capacity: %s", allocation.Image.DiskSize)
	}
	if allocation.Image.Qualified {
		t.Fatal("candidate proof promoted an image")
	}
	for _, name := range []string{"bootstrap-secrets.yaml", "clone-resources.yaml", "allocation.json"} {
		info, err := os.Stat(filepath.Join(out, name))
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("unprotected proof artifact %s: %v", name, err)
		}
	}
	if err = run("worker-one", "local-lvm", "10.252.1.0/24"); err == nil {
		t.Fatal("recreated an established proof identity")
	}
	after, err := os.ReadFile(filepath.Join(out, "allocation.json"))
	if err != nil || string(after) != string(original) {
		t.Fatal("retry changed established identity")
	}
}
