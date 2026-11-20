package ops

import (
	"context"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/paingha/tugcli/internal/resolve"
)

var apiTarget = resolve.Target{
	Kind: resolve.KindDeployment, Name: "api", Namespace: "web",
	Selector: labels.SelectorFromSet(labels.Set{"app": "api"}),
}

func apiDeployment(revision string, image string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name: "api", Namespace: "web", UID: "dep-uid",
			Annotations: map[string]string{revisionAnnotation: revision},
		},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "api"}},
			Template: podTemplate(image, ""),
		},
	}
}

func podTemplate(image, hash string) corev1.PodTemplateSpec {
	l := map[string]string{"app": "api"}
	if hash != "" {
		l[podTemplateHashLabel] = hash
	}
	return corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Labels: l},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "api", Image: image}}},
	}
}

func replicaSet(name, revision, image string) *appsv1.ReplicaSet {
	yes := true
	return &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "web",
			Labels:      map[string]string{"app": "api"},
			Annotations: map[string]string{revisionAnnotation: revision},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1", Kind: "Deployment", Name: "api", UID: "dep-uid", Controller: &yes,
			}},
		},
		Spec: appsv1.ReplicaSetSpec{Template: podTemplate(image, name)},
	}
}

func TestRestart(t *testing.T) {
	cs := fake.NewSimpleClientset(apiDeployment("1", "api:v1"))
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	if err := Restart(context.Background(), cs, apiTarget, now); err != nil {
		t.Fatal(err)
	}
	d, _ := cs.AppsV1().Deployments("web").Get(context.Background(), "api", metav1.GetOptions{})
	if got := d.Spec.Template.Annotations[restartedAtAnnotation]; got != "2026-11-20T12:00:00Z" {
		t.Fatalf("restartedAt = %q", got)
	}
	if err := Restart(context.Background(), cs, resolve.Target{Kind: resolve.KindPod, Name: "x"}, now); err == nil {
		t.Fatal("restarting a pod should fail")
	}
}

func TestScale(t *testing.T) {
	cs := fake.NewSimpleClientset(apiDeployment("1", "api:v1"))
	if err := Scale(context.Background(), cs, apiTarget, 5); err != nil {
		t.Fatal(err)
	}
	d, _ := cs.AppsV1().Deployments("web").Get(context.Background(), "api", metav1.GetOptions{})
	if d.Spec.Replicas == nil || *d.Spec.Replicas != 5 {
		t.Fatalf("replicas = %v", d.Spec.Replicas)
	}
	if err := Scale(context.Background(), cs, apiTarget, -1); err == nil {
		t.Fatal("negative replicas accepted")
	}
}

func TestRollback(t *testing.T) {
	ctx := context.Background()
	cs := fake.NewSimpleClientset(
		apiDeployment("3", "api:v3"),
		replicaSet("api-aaa", "1", "api:v1"),
		replicaSet("api-bbb", "2", "api:v2"),
		replicaSet("api-ccc", "3", "api:v3"),
	)
	rev, err := Rollback(ctx, cs, apiTarget, 0)
	if err != nil {
		t.Fatal(err)
	}
	if rev != 2 {
		t.Fatalf("rolled back to %d, want 2", rev)
	}
	d, _ := cs.AppsV1().Deployments("web").Get(ctx, "api", metav1.GetOptions{})
	if img := d.Spec.Template.Spec.Containers[0].Image; img != "api:v2" {
		t.Fatalf("image = %q", img)
	}
	if _, ok := d.Spec.Template.Labels[podTemplateHashLabel]; ok {
		t.Fatal("pod-template-hash copied into the deployment")
	}

	if rev, err := Rollback(ctx, cs, apiTarget, 1); err != nil || rev != 1 {
		t.Fatalf("--to-revision 1: %d %v", rev, err)
	}
	if _, err := Rollback(ctx, cs, apiTarget, 9); err == nil || !strings.Contains(err.Error(), "no revision 9") {
		t.Fatalf("missing revision: %v", err)
	}
}

func TestRollbackWithNoHistory(t *testing.T) {
	cs := fake.NewSimpleClientset(apiDeployment("1", "api:v1"), replicaSet("api-aaa", "1", "api:v1"))
	if _, err := Rollback(context.Background(), cs, apiTarget, 0); err == nil || !strings.Contains(err.Error(), "no earlier revision") {
		t.Fatalf("got %v", err)
	}
}

func TestLogsFromEveryPodAndContainer(t *testing.T) {
	mk := func(name string) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "web", Labels: map[string]string{"app": "api"}},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "api"}, {Name: "proxy"}}},
			Status:     corev1.PodStatus{Phase: corev1.PodRunning},
		}
	}
	cs := fake.NewSimpleClientset(mk("api-1"), mk("api-2"))

	var mu sync.Mutex
	var got []string
	err := Logs(context.Background(), cs, apiTarget, LogOptions{Tail: -1}, LogSink{
		Line: func(l LogLine) {
			mu.Lock()
			defer mu.Unlock()
			got = append(got, l.Pod+"/"+l.Container+": "+l.Text)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(got)
	want := []string{"api-1/api: fake logs", "api-1/proxy: fake logs", "api-2/api: fake logs", "api-2/proxy: fake logs"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("lines = %v", got)
	}

	got = nil
	if err := Logs(context.Background(), cs, apiTarget, LogOptions{Container: "proxy", Tail: -1}, LogSink{Line: func(l LogLine) {
		got = append(got, l.Container)
	}}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "proxy" {
		t.Fatalf("container filter: %v", got)
	}
	if err := Logs(context.Background(), cs, apiTarget, LogOptions{Container: "nope"}, LogSink{Line: func(LogLine) {}}); err == nil {
		t.Fatal("unknown container should fail")
	}
}

func TestResolvePorts(t *testing.T) {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "api-1"},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Ports: []corev1.ContainerPort{
			{Name: "http", ContainerPort: 8080},
			{Name: "metrics", ContainerPort: 9090},
			{Name: "dns", ContainerPort: 53, Protocol: corev1.ProtocolUDP},
		}}}},
	}
	tests := []struct {
		args    []string
		want    string
		wantErr bool
	}{
		{nil, "8080:8080,9090:9090", false},
		{[]string{"8080"}, "8080:8080", false},
		{[]string{"9000:http"}, "9000:8080", false},
		{[]string{"http"}, "8080:8080", false},
		{[]string{":metrics"}, ":9090", false},
		{[]string{"grpc"}, "", true},
		{[]string{"x:80"}, "", true},
	}
	for _, tt := range tests {
		got, err := ResolvePorts(p, tt.args)
		if (err != nil) != tt.wantErr {
			t.Errorf("ResolvePorts(%v) error = %v", tt.args, err)
			continue
		}
		if !tt.wantErr && strings.Join(got, ",") != tt.want {
			t.Errorf("ResolvePorts(%v) = %v, want %s", tt.args, got, tt.want)
		}
	}
}

func TestShellCommandPrefersBash(t *testing.T) {
	cmd := ShellCommand()
	if cmd[0] != "/bin/sh" || !strings.Contains(cmd[2], "exec bash") || !strings.Contains(cmd[2], "exec sh") {
		t.Fatalf("shell command = %v", cmd)
	}
}
