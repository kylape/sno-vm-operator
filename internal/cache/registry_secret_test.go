package cache

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

func TestDockerConfigCredentialsMatchRegistryHost(t *testing.T) {
	encoded := base64.StdEncoding.EncodeToString([]byte("robot:pull-token"))
	config := []byte(`{"auths":{"https://quay.io/v1/":{"auth":"` + encoded + `"},"registry.redhat.io":{"username":"redhat","password":"another-token"}}}`)

	username, password, err := dockerRegistryCredentials(config, "quay.io")
	if err != nil {
		t.Fatal(err)
	}
	if username != "robot" || password != "pull-token" {
		t.Fatalf("unexpected credentials: username=%q, password returned=%t", username, password != "")
	}
	if _, _, err = dockerRegistryCredentials(config, "registry.example.test"); err == nil || !strings.Contains(err.Error(), "no credentials") {
		t.Fatalf("missing registry credentials were not rejected clearly: %v", err)
	}
}

func TestDockerConfigRegistryCredentialsAreConvertedForCDI(t *testing.T) {
	encoded := base64.StdEncoding.EncodeToString([]byte("robot:pull-token"))
	dockerConfig, err := json.Marshal(dockerConfig{Auths: map[string]dockerRegistryAuth{
		"quay.io": {Auth: encoded},
	}})
	if err != nil {
		t.Fatal(err)
	}
	source := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "quay-pull", Namespace: "sno-images", UID: types.UID("source-secret-uid")},
		Type:       corev1.SecretTypeDockerConfigJson,
		Data:       map[string][]byte{corev1.DockerConfigJsonKey: dockerConfig},
	}
	r := setup(t, node("n1", "amd64"), source)
	r.Config.Images[0].RegistrySecret = source.Name
	syncOK(t, r)

	dv := getDV(t, r, "n1")
	secretRef, found, err := unstructured.NestedString(dv.Object, "spec", "source", "registry", "secretRef")
	if err != nil || !found {
		t.Fatalf("CDI Secret reference not set: found=%t err=%v", found, err)
	}
	expectedName := cdiRegistrySecretName(source.UID, "quay.io")
	if secretRef != expectedName || secretRef == source.Name {
		t.Fatalf("CDI Secret reference = %q, want generated Secret %q", secretRef, expectedName)
	}
	converted := &corev1.Secret{}
	if err = r.Client.Get(context.Background(), types.NamespacedName{Namespace: "sno-images", Name: secretRef}, converted); err != nil {
		t.Fatal(err)
	}
	if converted.Type != corev1.SecretTypeOpaque || string(converted.Data["accessKeyId"]) != "robot" || string(converted.Data["secretKey"]) != "pull-token" {
		t.Fatalf("CDI credential conversion has wrong type or data keys")
	}
	if !ownedBySecret(converted, source) {
		t.Fatal("converted credential Secret is not owned by the Docker config Secret")
	}
	if string(source.Data[corev1.DockerConfigJsonKey]) != string(dockerConfig) {
		t.Fatal("source Docker config Secret was modified")
	}
}

func TestDockerConfigCredentialErrorsDoNotIncludeCredentialValues(t *testing.T) {
	config := []byte(`{"auths":{"quay.io":{"identitytoken":"private-token"}}}`)
	_, _, err := dockerRegistryCredentials(config, "quay.io")
	if err == nil || !strings.Contains(err.Error(), "identity token") || strings.Contains(err.Error(), "private-token") {
		t.Fatalf("unexpected credential error: %v", err)
	}
}
