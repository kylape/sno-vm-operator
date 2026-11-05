package provision

import (
	"context"
	"testing"
	"time"

	api "github.com/kylape/sno-vm-operator/api/v1alpha1"
	"github.com/kylape/sno-vm-operator/internal/cache"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func instanceReconciler(t *testing.T, objects ...runtime.Object) *InstanceReconciler {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := api.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	for _, gvk := range []schema.GroupVersionKind{cache.DataVolumeGVK, VirtualMachineGVK, VirtualMachineInstanceGVK, RouteGVK} {
		scheme.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
	}
	clientBuilder := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&api.SNOVirtualMachine{})
	clientBuilder = clientBuilder.WithRuntimeObjects(objects...)
	return &InstanceReconciler{Client: clientBuilder.Build(), Config: cache.Config{Namespace: "sno-vm-system", Instances: cache.InstancePolicy{Enabled: false}}}
}

func TestDisabledPolicyDoesNotCreateResourcesOrFinalizers(t *testing.T) {
	request := &api.SNOVirtualMachine{ObjectMeta: metav1.ObjectMeta{Name: "sno-one", Namespace: "env-one", UID: types.UID("instance-uid")}}
	r := instanceReconciler(t, request)
	if err := r.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	actual := &api.SNOVirtualMachine{}
	if err := r.Client.Get(context.Background(), types.NamespacedName{Namespace: request.Namespace, Name: request.Name}, actual); err != nil {
		t.Fatal(err)
	}
	if actual.Status.Phase != "Pending" || len(actual.Finalizers) != 0 {
		t.Fatalf("disabled request changed unexpectedly: phase=%q finalizers=%v", actual.Status.Phase, actual.Finalizers)
	}
	if len(actual.Status.Conditions) != 1 || actual.Status.Conditions[0].Reason != "InstancesDisabled" {
		t.Fatalf("disabled condition missing: %+v", actual.Status.Conditions)
	}
}

func TestDeletionCleanupRemovesOnlyOperatorFinalizer(t *testing.T) {
	request := &api.SNOVirtualMachine{ObjectMeta: metav1.ObjectMeta{Name: "sno-one", Namespace: "env-one", UID: types.UID("instance-uid"), Finalizers: []string{instanceFinalizer}}}
	r := instanceReconciler(t, request)
	if err := r.cleanup(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	actual := &api.SNOVirtualMachine{}
	err := r.Client.Get(context.Background(), types.NamespacedName{Namespace: request.Namespace, Name: request.Name}, actual)
	if err != nil {
		t.Fatal(err)
	}
	if len(actual.Finalizers) != 0 {
		t.Fatalf("operator finalizer remains after cleanup: %v", actual.Finalizers)
	}
}

func TestRouteReadinessRequiresAdmittedExpectedHost(t *testing.T) {
	route := &unstructured.Unstructured{Object: map[string]interface{}{
		"status": map[string]interface{}{"ingress": []interface{}{map[string]interface{}{
			"host":       "api.sno-one.apps.example.test",
			"conditions": []interface{}{map[string]interface{}{"type": "Admitted", "status": "True"}},
		}}},
	}}
	if !routeAdmitted(route, "api.sno-one.apps.example.test") {
		t.Fatal("admitted route was not recognized")
	}
	if routeAdmitted(route, "api.peer.apps.example.test") {
		t.Fatal("peer route was accepted")
	}
	if routeAdmitted(&unstructured.Unstructured{}, "api.sno-one.apps.example.test") {
		t.Fatal("route without admission was accepted")
	}
}

func TestSetMilestoneIsStable(t *testing.T) {
	request := &api.SNOVirtualMachine{}
	setMilestone(request, "Allocated")
	first := request.Status.Milestones["Allocated"]
	time.Sleep(time.Millisecond)
	setMilestone(request, "Allocated")
	again := request.Status.Milestones["Allocated"]
	if !again.Equal(&first) {
		t.Fatal("milestone timestamp changed on retry")
	}
}
