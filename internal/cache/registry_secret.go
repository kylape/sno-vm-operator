package cache

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

type dockerConfig struct {
	Auths map[string]dockerRegistryAuth `json:"auths"`
}

type dockerRegistryAuth struct {
	Username      string `json:"username"`
	Password      string `json:"password"`
	Auth          string `json:"auth"`
	IdentityToken string `json:"identitytoken"`
}

// ensureCDIRegistrySecret accepts CDI's username/password Secret directly or
// converts the matching registry entry from a standard Docker config Secret.
// The source Secret remains untouched; CDI receives only the credential for
// the registry referenced by this image.
func (r *Reconciler) ensureCDIRegistrySecret(ctx context.Context, source *corev1.Secret, imageReference string) (string, error) {
	name, err := CDIRegistrySecretName(source, imageReference)
	if err != nil {
		return "", err
	}
	if name == source.Name {
		return name, nil
	}
	configData := source.Data[corev1.DockerConfigJsonKey]
	if len(configData) == 0 {
		configData = source.Data["config.json"]
	}
	username, password, err := dockerRegistryCredentials(configData, registryHost(imageReference))
	if err != nil {
		return "", err
	}
	controller := true
	blockOwnerDeletion := false
	expected := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: r.Config.Namespace,
			Name:      name,
			Labels:    map[string]string{"app.kubernetes.io/managed-by": ManagedBy, LabelPrefix + "registry-auth": "true"},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "v1", Kind: "Secret", Name: source.Name, UID: source.UID,
				Controller: &controller, BlockOwnerDeletion: &blockOwnerDeletion,
			}},
			Annotations: map[string]string{LabelPrefix + "registry-host": registryHost(imageReference)},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{"accessKeyId": []byte(username), "secretKey": []byte(password)},
	}
	actual := &corev1.Secret{}
	key := types.NamespacedName{Namespace: expected.Namespace, Name: expected.Name}
	err = r.Client.Get(ctx, key, actual)
	if apierrors.IsNotFound(err) {
		if err = r.Client.Create(ctx, expected); err != nil {
			return "", err
		}
		return name, nil
	}
	if err != nil {
		return "", err
	}
	if actual.Labels["app.kubernetes.io/managed-by"] != ManagedBy || !ownedBySecret(actual, source) {
		return "", fmt.Errorf("CDI credential Secret %s is not owned by registry Secret %s", name, source.Name)
	}
	if actual.Type == expected.Type && equalCredentials(actual.Data, expected.Data) {
		return name, nil
	}
	actual.Type = expected.Type
	actual.Data = expected.Data
	actual.OwnerReferences = expected.OwnerReferences
	actual.Labels = expected.Labels
	actual.Annotations = expected.Annotations
	if err = r.Client.Update(ctx, actual); err != nil {
		return "", err
	}
	return name, nil
}

// CDIRegistrySecretName returns the Secret reference that CDI uses for an
// image's registry credentials. Docker config Secrets are converted to a
// deterministic CDI Secret name based on the source Secret UID and registry.
func CDIRegistrySecretName(source *corev1.Secret, imageReference string) (string, error) {
	if len(source.Data["accessKeyId"]) > 0 && len(source.Data["secretKey"]) > 0 {
		return source.Name, nil
	}
	configData := source.Data[corev1.DockerConfigJsonKey]
	if len(configData) == 0 {
		configData = source.Data["config.json"]
	}
	if len(configData) == 0 {
		return "", fmt.Errorf("registry Secret %s must contain Docker config JSON or CDI accessKeyId/secretKey fields", source.Name)
	}
	host := registryHost(imageReference)
	if _, _, err := dockerRegistryCredentials(configData, host); err != nil {
		return "", err
	}
	if source.UID == "" {
		return "", fmt.Errorf("registry Docker config Secret %s has no Kubernetes UID", source.Name)
	}
	return cdiRegistrySecretName(source.UID, host), nil
}

func dockerRegistryCredentials(configData []byte, host string) (string, string, error) {
	var config dockerConfig
	if err := json.Unmarshal(configData, &config); err != nil {
		return "", "", fmt.Errorf("Docker config Secret is not valid JSON")
	}
	for registry, auth := range config.Auths {
		if dockerConfigHost(registry) != host {
			continue
		}
		username, password := auth.Username, auth.Password
		if username == "" && auth.Auth != "" {
			decoded, err := base64.StdEncoding.DecodeString(auth.Auth)
			if err != nil {
				return "", "", fmt.Errorf("Docker config credentials for %s have invalid auth encoding", host)
			}
			parts := strings.SplitN(string(decoded), ":", 2)
			if len(parts) == 2 {
				username, password = parts[0], parts[1]
			}
		}
		if username == "" || password == "" {
			if auth.IdentityToken != "" {
				return "", "", fmt.Errorf("Docker config for %s uses an identity token; CDI pod pulls require username/password credentials", host)
			}
			return "", "", fmt.Errorf("Docker config for %s has no usable username/password credentials", host)
		}
		return username, password, nil
	}
	return "", "", fmt.Errorf("Docker config Secret has no credentials for registry %s", host)
}

func registryHost(imageReference string) string {
	first, _, _ := strings.Cut(imageReference, "/")
	if first == "" || (!strings.ContainsAny(first, ".:") && first != "localhost") {
		return "docker.io"
	}
	return dockerConfigHost(first)
}

func dockerConfigHost(registry string) string {
	registry = strings.TrimSpace(registry)
	if registry == "" {
		return ""
	}
	if !strings.Contains(registry, "://") {
		registry = "https://" + registry
	}
	u, err := url.Parse(registry)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Host)
}

func cdiRegistrySecretName(sourceUID types.UID, host string) string {
	sum := sha256.Sum256([]byte(string(sourceUID) + "\x00" + host))
	return "cdi-auth-" + hex.EncodeToString(sum[:8])
}

func ownedBySecret(actual, source *corev1.Secret) bool {
	for _, owner := range actual.OwnerReferences {
		if owner.APIVersion == "v1" && owner.Kind == "Secret" && owner.Name == source.Name && owner.UID == source.UID && owner.Controller != nil && *owner.Controller {
			return true
		}
	}
	return false
}

func equalCredentials(a, b map[string][]byte) bool {
	return string(a["accessKeyId"]) == string(b["accessKeyId"]) && string(a["secretKey"]) == string(b["secretKey"])
}
