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

	api "github.com/kylape/sno-vm-operator/api/v1alpha1"
	"golang.org/x/crypto/ssh"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	coretyped "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

type Observation struct {
	Ready        bool
	Bootstrapped bool
	Kubeconfig   []byte
	Node         string
	Reason       string
	Message      string
}

type GuestObserver interface {
	Observe(context.Context, Allocation, api.Resources, int32, string, *corev1.Secret, *corev1.Secret) (Observation, error)
}

// SSHObserver trusts the per-instance host key, never the reference key or an
// accept-any-host callback. Management private keys are never mounted in guests.
type SSHObserver struct{}

var clusterVersionGVR = schema.GroupVersionResource{Group: "config.openshift.io", Version: "v1", Resource: "clusterversions"}
var clusterOperatorGVR = schema.GroupVersionResource{Group: "config.openshift.io", Version: "v1", Resource: "clusteroperators"}

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

// inspectGuest pins the cloned cluster's ID to the durable host allocation and
// waits until the SNO API, its single node, and all cluster operators are ready.
// The kubeconfig is already TLS-verified and points at the guest's passthrough
// API Route, so these checks also validate the external API path.
func inspectGuest(ctx context.Context, raw []byte, a Allocation, hardware api.Resources, maxPods int32) (bool, string, string, string, error) {
	config, err := clientcmd.RESTConfigFromKubeConfig(raw)
	if err != nil {
		return false, "", "GuestTrustNotReady", "Invalid portable guest kubeconfig", fmt.Errorf("invalid portable guest kubeconfig")
	}
	config.Timeout = 10 * time.Second
	kube, err := coretyped.NewForConfig(config)
	if err != nil {
		return false, "", "GuestAPIPending", "Waiting for the guest API", fmt.Errorf("create guest API client: %w", err)
	}
	if err = kube.CoreV1().RESTClient().Get().AbsPath("/readyz").Do(ctx).Error(); err != nil {
		return false, "", "GuestAPIPending", "Waiting for the guest API", nil
	}
	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		return false, "", "GuestAPIPending", "Waiting for the guest API", fmt.Errorf("create guest config API client: %w", err)
	}
	version, err := dynamicClient.Resource(clusterVersionGVR).Get(ctx, "version", metav1.GetOptions{})
	if err != nil {
		return false, "", "GuestAPIPending", "Waiting for the guest ClusterVersion", nil
	}
	clusterID, _, _ := unstructured.NestedString(version.Object, "spec", "clusterID")
	if clusterID != a.ClusterID {
		copy := version.DeepCopy()
		if err = unstructured.SetNestedField(copy.Object, a.ClusterID, "spec", "clusterID"); err != nil {
			return false, "", "GuestIdentityPending", "Setting guest cluster identity", fmt.Errorf("set guest cluster identity: %w", err)
		}
		if _, err = dynamicClient.Resource(clusterVersionGVR).Update(ctx, copy, metav1.UpdateOptions{}); err != nil {
			return false, "", "GuestIdentityPending", "Setting guest cluster identity", nil
		}
		return false, "", "GuestIdentityPending", "Setting guest cluster identity", nil
	}
	versionConditions, _, _ := unstructured.NestedSlice(version.Object, "status", "conditions")
	versionStates := map[string]string{}
	for _, item := range versionConditions {
		condition, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		typ, _, _ := unstructured.NestedString(condition, "type")
		status, _, _ := unstructured.NestedString(condition, "status")
		versionStates[typ] = status
	}
	if !clusterVersionReady(versionStates) {
		return false, "", "GuestVersionPending", "Waiting for the guest OpenShift version to become ready", nil
	}
	nodes, err := kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil || len(nodes.Items) == 0 {
		return false, "", "GuestNodePending", "Waiting for the SNO node", nil
	}
	if len(nodes.Items) != 1 {
		return false, "", "GuestNodeCountMismatch", "Guest must contain exactly one SNO node", fmt.Errorf("guest must contain exactly one SNO node")
	}
	node := nodes.Items[0]
	ready := false
	for _, condition := range node.Status.Conditions {
		if condition.Type == corev1.NodeReady && condition.Status == corev1.ConditionTrue {
			ready = true
		}
	}
	if !ready {
		return false, node.Name, "GuestNodePending", "Waiting for the SNO node to become Ready", nil
	}
	operators, err := dynamicClient.Resource(clusterOperatorGVR).List(ctx, metav1.ListOptions{})
	if err != nil || len(operators.Items) == 0 {
		return false, node.Name, "GuestOperatorsPending", "Waiting for guest cluster operators", nil
	}
	for _, operator := range operators.Items {
		conditions, _, _ := unstructured.NestedSlice(operator.Object, "status", "conditions")
		states := map[string]string{}
		for _, item := range conditions {
			condition, ok := item.(map[string]interface{})
			if !ok {
				continue
			}
			typ, _, _ := unstructured.NestedString(condition, "type")
			status, _, _ := unstructured.NestedString(condition, "status")
			states[typ] = status
		}
		if states["Available"] != "True" || states["Degraded"] != "False" || states["Progressing"] != "False" {
			return false, node.Name, "GuestOperatorsPending", "Waiting for guest cluster operators", nil
		}
	}
	limitReady, limitReason, limitMessage, err := ensureGuestMaxPods(ctx, dynamicClient, hardware.Cores, maxPods, &node, a.UID)
	if err != nil {
		return false, node.Name, limitReason, limitMessage, err
	}
	if !limitReady {
		return false, node.Name, limitReason, limitMessage, nil
	}
	clusterVersion, _, _ := unstructured.NestedString(version.Object, "status", "desired", "version")
	storageReady, storageReason, storageMessage, err := ensureGuestStorage(ctx, dynamicClient, clusterVersion, a.UID)
	if err != nil {
		return false, node.Name, "GuestStorageConfigurationFailed", err.Error(), err
	}
	if !storageReady {
		return false, node.Name, storageReason, storageMessage, nil
	}
	return true, node.Name, "Ready", limitMessage + "; " + storageMessage, nil
}

func clusterVersionReady(states map[string]string) bool {
	return states["Available"] == "True" && states["Failing"] == "False" && states["Progressing"] == "False"
}

func (SSHObserver) Observe(ctx context.Context, a Allocation, hardware api.Resources, maxPods int32, address string, management, bootstrap *corev1.Secret) (Observation, error) {
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
	waiting.Kubeconfig = portable
	ready, node, reason, message, err := inspectGuest(ctx, portable, a, hardware, maxPods)
	if err != nil {
		waiting.Reason = reason
		waiting.Message = err.Error()
		return waiting, nil
	}
	waiting.Node = node
	waiting.Ready = ready
	waiting.Reason = reason
	waiting.Message = message
	return waiting, nil
}
