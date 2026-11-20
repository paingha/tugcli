package ops

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	utilexec "k8s.io/client-go/util/exec"

	"github.com/paingha/tugcli/internal/resolve"
)

func TestAddDebugContainer(t *testing.T) {
	tests := []struct {
		name    string
		opts    DebugOptions
		image   string
		command []string
	}{
		{"defaults", DebugOptions{TTY: true}, DefaultDebugImage, nil},
		{"image and command", DebugOptions{Image: "nicolaka/netshoot:v0.13", Command: []string{"tcpdump", "-i", "any"}}, "nicolaka/netshoot:v0.13", []string{"tcpdump", "-i", "any"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cs := fake.NewSimpleClientset(whyPod("api-1"))
			o := tt.opts
			o.Namespace, o.Pod, o.Target = "web", "api-1", "api"
			name, err := AddDebugContainer(context.Background(), cs, o)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(name, "debugger-") || len(name) != len("debugger-")+5 {
				t.Errorf("AddDebugContainer() = %q, want debugger-xxxxx", name)
			}
			var patched bool
			for _, a := range cs.Actions() {
				if a.GetVerb() == "patch" && a.GetSubresource() == "ephemeralcontainers" {
					patched = true
				}
			}
			if !patched {
				t.Fatalf("no patch of pods/ephemeralcontainers in %v", cs.Actions())
			}
			p, _ := cs.CoreV1().Pods("web").Get(context.Background(), "api-1", metav1.GetOptions{})
			if len(p.Spec.EphemeralContainers) != 1 {
				t.Fatalf("ephemeral containers = %+v", p.Spec.EphemeralContainers)
			}
			ec := p.Spec.EphemeralContainers[0]
			if ec.Name != name || ec.Image != tt.image || ec.TargetContainerName != "api" || !ec.Stdin || !ec.StdinOnce ||
				ec.TTY != tt.opts.TTY || strings.Join(ec.Command, " ") != strings.Join(tt.command, " ") {
				t.Errorf("ephemeral container = %+v, want %s targeting api", ec, tt.image)
			}
			if c := p.Spec.Containers; len(c) != 1 || c[0].Name != "api" {
				t.Errorf("the patch changed the pod's containers: %+v", c)
			}
		})
	}
}

func TestAddDebugContainerToAMissingPod(t *testing.T) {
	cs := fake.NewSimpleClientset()
	_, err := AddDebugContainer(context.Background(), cs, DebugOptions{Namespace: "web", Pod: "gone"})
	if err == nil || !strings.Contains(err.Error(), "getting pod/gone") {
		t.Fatalf("AddDebugContainer(gone) = %v, want a not found error", err)
	}
}

func TestDebugContainerNameIsFree(t *testing.T) {
	p := whyPod("api-1")
	for range 50 {
		name := debugContainerName(p)
		for _, c := range p.Spec.EphemeralContainers {
			if c.Name == name {
				t.Fatalf("debugContainerName() = %q, which the pod has", name)
			}
		}
		p.Spec.EphemeralContainers = append(p.Spec.EphemeralContainers, corev1.EphemeralContainer{
			EphemeralContainerCommon: corev1.EphemeralContainerCommon{Name: name},
		})
	}
}

// debugStates returns a pod whose debug container goes through the states,
// one per read of the pod.
func debugStates(t *testing.T, states ...corev1.ContainerState) *fake.Clientset {
	t.Helper()
	old := debugPoll
	debugPoll = time.Millisecond
	t.Cleanup(func() { debugPoll = old })

	cs := fake.NewSimpleClientset()
	reads := 0
	cs.PrependReactor("get", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		p := whyPod("api-1")
		p.Status.EphemeralContainerStatuses = []corev1.ContainerStatus{{Name: "debugger-abcde", State: states[min(reads, len(states)-1)]}}
		reads++
		return true, p, nil
	})
	return cs
}

func TestWaitForEphemeralContainer(t *testing.T) {
	creating := corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}}
	running := corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
	exited := corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 3}}
	pullFailed := corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ErrImagePull", Message: "manifest unknown"}}

	tests := []struct {
		name      string
		states    []corev1.ContainerState
		want      string // running, terminated
		imageFail bool
	}{
		{"starts", []corev1.ContainerState{creating, creating, running}, "running", false},
		{"already done", []corev1.ContainerState{exited}, "terminated", false},
		{"bad image", []corev1.ContainerState{creating, pullFailed}, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cs := debugStates(t, tt.states...)
			state, err := WaitForEphemeralContainer(context.Background(), cs, "web", "api-1", "debugger-abcde")
			if tt.imageFail {
				var se *StartError
				if !errors.As(err, &se) || !se.Image() || !strings.Contains(err.Error(), "manifest unknown") {
					t.Fatalf("WaitForEphemeralContainer = %v, want an image StartError", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			got := "running"
			if state.Terminated != nil {
				got = "terminated"
			}
			if got != tt.want {
				t.Errorf("WaitForEphemeralContainer = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestWaitForEphemeralContainerStopsWhenCancelled(t *testing.T) {
	cs := debugStates(t, corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := WaitForEphemeralContainer(ctx, cs, "web", "api-1", "debugger-abcde"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("WaitForEphemeralContainer = %v, want the context's error", err)
	}
}

func TestNoShell(t *testing.T) {
	tests := []struct {
		err  error
		want bool
	}{
		{errors.New(`OCI runtime exec failed: exec failed: unable to start container process: exec: "/bin/sh": stat /bin/sh: no such file or directory: unknown`), true},
		{errors.New("executable file `/bin/sh` not found: No such file or directory"), true},
		{fmt.Errorf("streaming: %w", errors.New(`exec: "/bin/sh": executable file not found in $PATH`)), true},
		{utilexec.CodeExitError{Err: errors.New("command terminated with exit code 127"), Code: 127}, false},
		{errors.New("error dialing backend: dial tcp 10.0.0.5:10250: connect: connection refused"), false},
		{nil, false},
	}
	for _, tt := range tests {
		if got := NoShell(tt.err); got != tt.want {
			t.Errorf("NoShell(%v) = %v, want %v", tt.err, got, tt.want)
		}
	}
}

func TestLogsOfAnEphemeralContainer(t *testing.T) {
	p := whyPod("api-1", func(p *corev1.Pod) {
		p.Spec.EphemeralContainers = []corev1.EphemeralContainer{{EphemeralContainerCommon: corev1.EphemeralContainerCommon{Name: "debugger-abcde"}}}
	})
	cs := fake.NewSimpleClientset(p)
	target := resolve.Target{Kind: resolve.KindPod, Name: "api-1", Namespace: "web"}
	var got []string
	err := Logs(context.Background(), cs, target, LogOptions{Container: "debugger-abcde", Tail: -1}, LogSink{Line: func(l LogLine) {
		got = append(got, l.Container)
	}})
	if err != nil || len(got) != 1 || got[0] != "debugger-abcde" {
		t.Fatalf("Logs(-c debugger-abcde) = %v, %v", got, err)
	}

	got = nil
	if err := Logs(context.Background(), cs, target, LogOptions{Tail: -1}, LogSink{Line: func(l LogLine) {
		got = append(got, l.Container)
	}}); err != nil || strings.Join(got, ",") != "api" {
		t.Fatalf("Logs() = %v, %v; want only the app container", got, err)
	}
}
