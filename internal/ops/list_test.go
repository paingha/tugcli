package ops

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"

	"github.com/paingha/tugcli/internal/resolve"
)

func mapping(gvr schema.GroupVersionResource, kind string, scope meta.RESTScope) *meta.RESTMapping {
	return &meta.RESTMapping{Resource: gvr, GroupVersionKind: gvr.GroupVersion().WithKind(kind), Scope: scope}
}

func TestListObjects(t *testing.T) {
	yes := true
	one := int32(1)
	created := metav1.NewTime(whyNow.Add(-90 * time.Minute))
	api := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "web", Generation: 1, CreationTimestamp: created},
		Spec:       appsv1.DeploymentSpec{Replicas: &one},
		Status:     appsv1.DeploymentStatus{ObservedGeneration: 1, Replicas: 1, UpdatedReplicas: 1, ReadyReplicas: 1, AvailableReplicas: 1},
	}
	crashing := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "api-7d9f-x2x4z", Namespace: "web", CreationTimestamp: created,
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "api-7d9f", Controller: &yes}}},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main"}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{
			Name: "main", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
		}}},
	}
	bare := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "debug", Namespace: "web"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "main"}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning,
			Conditions:        []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
			ContainerStatuses: []corev1.ContainerStatus{{Name: "main", Ready: true, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}},
	}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "api-key", Namespace: "web"}, Data: map[string][]byte{"key": []byte("hunter2")}}
	elsewhere := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: "batch"}}
	orders := custom("db.example.com/v1", "Database", "web", "orders", "Ready=False BackupFailing: the latest backup failed")

	c, _ := fakeClients(t, []runtime.Object{api, crashing, bare, secret, elsewhere}, orders)
	ctx := context.Background()
	podsV1 := schema.GroupVersionResource{Version: "v1", Resource: "pods"}

	tests := []struct {
		name string
		m    *meta.RESTMapping
		want []ObjectState
	}{
		{"pods", mapping(podsV1, "Pod", meta.RESTScopeNamespace), []ObjectState{
			{Object: resolve.Object{Resource: podsV1, Kind: "Pod", Name: "api-7d9f-x2x4z", Namespace: "web"},
				Health: HealthNotReady, Status: "CrashLoopBackOff", Owner: "rs/api-7d9f", Created: created.Time},
			{Object: resolve.Object{Resource: podsV1, Kind: "Pod", Name: "debug", Namespace: "web"},
				Health: HealthReady, Status: "Running"},
		}},
		{"deployments", mapping(deploymentsV1, "Deployment", meta.RESTScopeNamespace), []ObjectState{
			{Object: resolve.Object{Resource: deploymentsV1, Kind: "Deployment", Name: "api", Namespace: "web"},
				Health: HealthReady, Status: "1/1 ready", Created: created.Time},
		}},
		{"secrets", mapping(secretsV1, "Secret", meta.RESTScopeNamespace), []ObjectState{
			{Object: resolve.Object{Resource: secretsV1, Kind: "Secret", Name: "api-key", Namespace: "web"}, Health: HealthNone},
		}},
		{"databases", mapping(databases, "Database", meta.RESTScopeNamespace), []ObjectState{
			{Object: resolve.Object{Resource: databases, Kind: "Database", Name: "orders", Namespace: "web"},
				Health: HealthNotReady, Status: "BackupFailing: the latest backup failed"},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ListObjects(ctx, c, tt.m, "web")
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("ListObjects(%s) = %+v, want %+v", tt.name, got, tt.want)
			}
			for i := range got {
				if !got[i].Created.Equal(tt.want[i].Created) {
					t.Errorf("ListObjects(%s)[%d].Created = %v, want %v", tt.name, i, got[i].Created, tt.want[i].Created)
				}
				got[i].Created = tt.want[i].Created // compared above, as times
				if got[i] != tt.want[i] {
					t.Errorf("ListObjects(%s)[%d] = %+v, want %+v", tt.name, i, got[i], tt.want[i])
				}
			}
		})
	}
	for _, a := range c.Dynamic.(*dynamicfake.FakeDynamicClient).Actions() {
		if a.GetResource().Resource == "secrets" {
			t.Errorf("ListObjects read Secrets through the dynamic client: %v", a)
		}
	}
}

var (
	deploymentsV1 = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
	secretsV1     = schema.GroupVersionResource{Version: "v1", Resource: "secrets"}
)
