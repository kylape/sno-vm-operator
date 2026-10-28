package provision

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/kylape/sno-vm-operator/internal/cache"
	"golang.org/x/crypto/bcrypt"
	"golang.org/x/crypto/ssh"
)

func TestCredentialsHaveIndependentKeysAndKeepManagementKeyOutOfGuest(t *testing.T) {
	r := allocator(t, "10.252.0.0/16")
	req := request()
	a, err := r.Create(context.Background(), req, cache.Image{Qualified: true, SeedID: uuid.NewString()}, source())
	if err != nil {
		t.Fatal(err)
	}
	pull := []byte(`{"auths":{"test.invalid":{"auth":"TEST_ONLY"}}}`)
	first, err := GenerateCredentials(req, *a, pull, nil, []string{"172.30.32.118"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := GenerateCredentials(req, *a, pull, nil, []string{"172.30.32.118"})
	if err != nil {
		t.Fatal(err)
	}
	config := BootstrapConfig{}
	if err = json.Unmarshal(first.Bootstrap.Data["config.json"], &config); err != nil {
		t.Fatal(err)
	}
	host, err := ssh.ParsePrivateKey([]byte(config.SSHHostPrivateKey))
	if err != nil {
		t.Fatal(err)
	}
	client, err := ssh.ParsePrivateKey(first.Management.Data["ssh-privatekey"])
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(host.PublicKey().Marshal(), client.PublicKey().Marshal()) {
		t.Fatal("operator uses guest host private key")
	}
	if !bytes.Equal(ssh.MarshalAuthorizedKey(host.PublicKey()), first.Management.Data["ssh-host-publickey"]) {
		t.Fatal("host key verification cannot pin the guest")
	}
	if config.SSHAuthorizedKeys[0] != string(bytes.TrimSpace(ssh.MarshalAuthorizedKey(client.PublicKey()))) {
		t.Fatal("guest does not authorize its operator client")
	}
	if bytes.Contains(first.Bootstrap.Data["config.json"], first.Management.Data["ssh-privatekey"]) {
		t.Fatal("guest received the operator private key")
	}
	if err = bcrypt.CompareHashAndPassword([]byte(config.KubeadminPasswordHash), first.Management.Data["kubeadmin-password"]); err != nil {
		t.Fatal("kubeadmin password/hash mismatch")
	}
	if bytes.Equal(first.Management.Data["ssh-privatekey"], second.Management.Data["ssh-privatekey"]) || bytes.Equal(first.Management.Data["ssh-host-publickey"], second.Management.Data["ssh-host-publickey"]) {
		t.Fatal("credentials are not independently generated")
	}
	if !bytes.Equal(first.Management.Data["bootstrap-config.json"], first.Bootstrap.Data["config.json"]) {
		t.Fatal("cannot recover bootstrap creation from durable management material")
	}
	if first.Bootstrap.Immutable == nil || !*first.Bootstrap.Immutable || first.Management.Immutable == nil || !*first.Management.Immutable {
		t.Fatal("bootstrap credentials must be immutable")
	}
	if config.InstanceUID != a.UID || config.ClusterID != a.ClusterID || config.SeedID != a.Image.SeedID {
		t.Fatal("bootstrap uses another allocation")
	}
}

func TestCredentialInputsFailWithoutLeakingSecretContents(t *testing.T) {
	r := allocator(t, "10.252.0.0/16")
	req := request()
	a, err := r.Create(context.Background(), req, cache.Image{Qualified: true, SeedID: uuid.NewString()}, source())
	if err != nil {
		t.Fatal(err)
	}
	_, err = GenerateCredentials(req, *a, []byte("SENSITIVE_TEST_ONLY"), nil, []string{"172.30.32.118"})
	if err == nil || bytes.Contains([]byte(err.Error()), []byte("SENSITIVE_TEST_ONLY")) {
		t.Fatal("malformed pull secret accepted or leaked")
	}
	_, err = GenerateCredentials(req, *a, []byte(`{"auths":{"test.invalid":{}}}`), []byte("SENSITIVE_TEST_ONLY"), []string{"172.30.32.118"})
	if err == nil || bytes.Contains([]byte(err.Error()), []byte("SENSITIVE_TEST_ONLY")) {
		t.Fatal("malformed public key accepted or leaked")
	}
	a.Image.Qualified = false
	if _, err = GenerateCredentials(req, *a, nil, nil, nil); err == nil {
		t.Fatal("generated bootstrap for an unqualified seed")
	}
}
