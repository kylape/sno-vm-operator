package provision

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/netip"
	"strings"

	api "github.com/kylape/sno-vm-operator/api/v1alpha1"
	"golang.org/x/crypto/bcrypt"
	"golang.org/x/crypto/ssh"
	corev1 "k8s.io/api/core/v1"
)

// BootstrapConfig is the versioned read-only ISO contract with sno-init. The
// operator's SSH client private key is deliberately absent from this contract.
type BootstrapConfig struct {
	Version               int      `json:"version"`
	InstanceUID           string   `json:"instanceUID"`
	SeedID                string   `json:"seedID"`
	ClusterID             string   `json:"clusterID"`
	ClusterName           string   `json:"clusterName"`
	BaseDomain            string   `json:"baseDomain"`
	Hostname              string   `json:"hostname"`
	IP                    string   `json:"ip"`
	MachineCIDR           string   `json:"machineCIDR"`
	MACAddress            string   `json:"macAddress"`
	DNSServers            []string `json:"dnsServers"`
	SSHAuthorizedKeys     []string `json:"sshAuthorizedKeys"`
	SSHHostPrivateKey     string   `json:"sshHostPrivateKey"`
	KubeadminPasswordHash string   `json:"kubeadminPasswordHash"`
	PullSecret            string   `json:"pullSecret"`
}

type Credentials struct {
	Bootstrap  *corev1.Secret
	Management *corev1.Secret
}

func sshKey(comment string) ([]byte, []byte, error) {
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	block, err := ssh.MarshalPrivateKey(private, comment)
	if err != nil {
		return nil, nil, err
	}
	signer, err := ssh.NewSignerFromKey(private)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(block), ssh.MarshalAuthorizedKey(signer.PublicKey()), nil
}

// GenerateCredentials must run once before VM creation, followed by durable
// Secret creation. Persist Management first: its canonical bootstrap copy lets
// reconciliation finish Bootstrap creation after a crash without rotating keys.
// Existing Secrets must be loaded rather than regenerating a started identity.
func GenerateCredentials(request *api.SNOVirtualMachine, a Allocation, pullSecret []byte, userKeys []byte, dns []string) (*Credentials, error) {
	if a.UID != string(request.UID) || a.Namespace != request.Namespace || a.Name != request.Name || !a.Image.Qualified || a.Image.SeedID == "" {
		return nil, fmt.Errorf("credentials require the allocated qualified seed identity")
	}
	var auth struct {
		Auths map[string]json.RawMessage `json:"auths"`
	}
	if json.Unmarshal(pullSecret, &auth) != nil || len(auth.Auths) == 0 {
		return nil, fmt.Errorf("invalid guest pull secret")
	}
	if len(dns) == 0 {
		return nil, fmt.Errorf("guest DNS is required")
	}
	for _, server := range dns {
		if address, err := netip.ParseAddr(server); err != nil || !address.Is4() {
			return nil, fmt.Errorf("guest DNS must use IPv4 addresses")
		}
	}
	hostPrivate, hostPublic, err := sshKey("sno-host")
	if err != nil {
		return nil, err
	}
	clientPrivate, clientPublic, err := sshKey("sno-operator")
	if err != nil {
		return nil, err
	}
	authorized := []string{strings.TrimSpace(string(clientPublic))}
	remaining := userKeys
	for len(strings.TrimSpace(string(remaining))) > 0 {
		key, _, options, rest, err := ssh.ParseAuthorizedKey(remaining)
		if err != nil || len(options) != 0 {
			return nil, fmt.Errorf("guest authorized keys must be valid unrestricted public keys")
		}
		authorized = append(authorized, strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key))))
		remaining = rest
	}
	var passwordBytes [32]byte
	if _, err = rand.Read(passwordBytes[:]); err != nil {
		return nil, err
	}
	password := base64.RawURLEncoding.EncodeToString(passwordBytes[:])
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return nil, err
	}
	config := BootstrapConfig{Version: 1, InstanceUID: a.UID, SeedID: a.Image.SeedID, ClusterID: a.ClusterID, ClusterName: a.ClusterName, BaseDomain: a.BaseDomain, Hostname: a.Hostname, IP: a.IP, MachineCIDR: a.MachineCIDR, MACAddress: a.MAC, DNSServers: append([]string(nil), dns...), SSHAuthorizedKeys: authorized, SSHHostPrivateKey: string(hostPrivate), KubeadminPasswordHash: string(hash), PullSecret: string(pullSecret)}
	data, err := json.Marshal(config)
	if err != nil {
		return nil, err
	}
	if len(data) > 900*1024 {
		return nil, fmt.Errorf("guest bootstrap configuration exceeds Secret size budget")
	}
	immutable := true
	return &Credentials{
		Bootstrap:  &corev1.Secret{ObjectMeta: metadata(request, request.Name+"-bootstrap"), Type: corev1.SecretTypeOpaque, Immutable: &immutable, Data: map[string][]byte{"config.json": data}},
		Management: &corev1.Secret{ObjectMeta: metadata(request, request.Name+"-management"), Type: corev1.SecretTypeOpaque, Immutable: &immutable, Data: map[string][]byte{"ssh-privatekey": clientPrivate, "ssh-host-publickey": hostPublic, "kubeadmin-password": []byte(password), "bootstrap-config.json": append([]byte(nil), data...)}},
	}, nil
}
