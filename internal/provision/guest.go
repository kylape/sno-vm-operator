package provision

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"time"

	"golang.org/x/crypto/ssh"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

type Observation struct {
	Ready        bool
	Bootstrapped bool
	Kubeconfig   []byte
	Reason       string
	Message      string
}

type GuestObserver interface {
	Observe(context.Context, Allocation, string, *corev1.Secret, *corev1.Secret) (Observation, error)
}

// SSHObserver trusts the per-instance host key, never the reference key or an
// accept-any-host callback. Management private keys are never mounted in guests.
type SSHObserver struct{}

func completionValid(data, config []byte, a Allocation) bool {
	var marker struct {
		Version     int    `json:"version"`
		InstanceUID string `json:"instanceUID"`
		ClusterID   string `json:"requestedClusterID"`
		Hash        string `json:"configSHA256"`
	}
	sum := sha256.Sum256(config)
	return json.Unmarshal(data, &marker) == nil && marker.Version == 1 && marker.InstanceUID == a.UID && marker.ClusterID == a.ClusterID && marker.Hash == hex.EncodeToString(sum[:])
}

func readGuest(ctx context.Context, address string, management *corev1.Secret) ([]byte, []byte, error) {
	signer, err := ssh.ParsePrivateKey(management.Data["ssh-privatekey"])
	if err != nil {
		return nil, nil, fmt.Errorf("invalid management SSH identity")
	}
	host, _, _, _, err := ssh.ParseAuthorizedKey(management.Data["ssh-host-publickey"])
	if err != nil {
		return nil, nil, fmt.Errorf("invalid pinned guest host key")
	}
	dialer := net.Dialer{Timeout: 5 * time.Second}
	connection, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(address, "22"))
	if err != nil {
		return nil, nil, fmt.Errorf("guest SSH is not reachable")
	}
	defer connection.Close()
	deadline := time.Now().Add(15 * time.Second)
	if parent, ok := ctx.Deadline(); ok && parent.Before(deadline) {
		deadline = parent
	}
	if err = connection.SetDeadline(deadline); err != nil {
		return nil, nil, err
	}
	config := &ssh.ClientConfig{User: "core", Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)}, HostKeyCallback: ssh.FixedHostKey(host), HostKeyAlgorithms: []string{host.Type()}, Timeout: 5 * time.Second}
	transport, channels, requests, err := ssh.NewClientConn(connection, address, config)
	if err != nil {
		return nil, nil, fmt.Errorf("guest SSH host identity or authentication is not ready")
	}
	client := ssh.NewClient(transport, channels, requests)
	defer client.Close()
	run := func(command string) ([]byte, error) {
		session, err := client.NewSession()
		if err != nil {
			return nil, err
		}
		defer session.Close()
		// Output contains administrator credentials and is kept only in memory.
		return session.Output(command)
	}
	marker, err := run("sudo cat /var/lib/sno-init/complete.json")
	if err != nil {
		return nil, nil, fmt.Errorf("guest specialization marker is not available")
	}
	kubeconfig, err := run("sudo /usr/bin/oc config view --raw --flatten --kubeconfig /etc/kubernetes/static-pod-resources/kube-apiserver-certs/secrets/node-kubeconfigs/lb-ext.kubeconfig")
	if err != nil {
		return nil, nil, fmt.Errorf("specialized guest administrator credentials are not available")
	}
	return marker, kubeconfig, nil
}

func portableKubeconfig(data []byte, a Allocation) ([]byte, error) {
	config, err := clientcmd.Load(data)
	if err != nil {
		return nil, fmt.Errorf("invalid guest kubeconfig")
	}
	current := config.Contexts[config.CurrentContext]
	if current == nil {
		return nil, fmt.Errorf("guest kubeconfig context is missing")
	}
	cluster, auth := config.Clusters[current.Cluster], config.AuthInfos[current.AuthInfo]
	if cluster == nil || auth == nil || cluster.InsecureSkipTLSVerify || len(cluster.CertificateAuthorityData) == 0 || cluster.CertificateAuthority != "" || cluster.ProxyURL != "" || auth.Exec != nil || auth.AuthProvider != nil || auth.TokenFile != "" || auth.ClientCertificate != "" || auth.ClientKey != "" || auth.Impersonate != "" || len(auth.ImpersonateGroups) > 0 || (auth.Token == "" && (len(auth.ClientCertificateData) == 0 || len(auth.ClientKeyData) == 0)) {
		return nil, fmt.Errorf("guest kubeconfig must contain portable credentials and verified embedded trust")
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(cluster.CertificateAuthorityData) {
		return nil, fmt.Errorf("guest CA bundle is invalid")
	}
	hash := sha256.Sum256(cluster.CertificateAuthorityData)
	if a.Image.SourceCAHash == "" || hex.EncodeToString(hash[:]) == a.Image.SourceCAHash {
		return nil, fmt.Errorf("guest still uses the source CA bundle")
	}
	cluster = cluster.DeepCopy()
	cluster.Server = a.Endpoint()
	cluster.TLSServerName = ""
	output := clientcmdapi.NewConfig()
	output.Clusters["guest"] = cluster
	output.AuthInfos["admin"] = auth.DeepCopy()
	output.Contexts["guest"] = &clientcmdapi.Context{Cluster: "guest", AuthInfo: "admin", Namespace: "default"}
	output.CurrentContext = "guest"
	return clientcmd.Write(*output)
}

func (SSHObserver) Observe(ctx context.Context, a Allocation, address string, management, bootstrap *corev1.Secret) (Observation, error) {
	waiting := Observation{Reason: "SpecializationPending", Message: "Waiting for independent guest identity and administrator access"}
	marker, raw, err := readGuest(ctx, address, management)
	if err != nil {
		return waiting, nil
	}
	if !completionValid(marker, bootstrap.Data["config.json"], a) {
		waiting.Reason = "SpecializationIdentityMismatch"
		waiting.Message = "Guest specialization does not match its allocated identity"
		return waiting, nil
	}
	waiting.Bootstrapped = true
	portable, err := portableKubeconfig(raw, a)
	if err != nil {
		waiting.Reason = "GuestTrustNotReady"
		waiting.Message = err.Error()
		return waiting, nil
	}
	// API, node, cluster ID, and application checks are performed separately.
	waiting.Kubeconfig = portable
	waiting.Reason = "GuestAPIPending"
	waiting.Message = "Guest identity is specialized; API and application readiness checks remain"
	return waiting, nil
}
