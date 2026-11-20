package ops

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	utilrand "k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/client-go/kubernetes"
)

// DefaultDebugImage is the image of a debug container when none is chosen:
// small, and with a shell and the usual tools.
const DefaultDebugImage = "busybox:1.36"

// debugPoll is how often WaitForEphemeralContainer looks at the pod.
var debugPoll = 500 * time.Millisecond

// DebugOptions describe an ephemeral debug container to add to a pod.
type DebugOptions struct {
	Namespace string
	Pod       string
	// Target is the container whose process namespace the debug container
	// joins, so that its processes, and its files under /proc/1/root, can
	// be seen.
	Target string
	// Image defaults to DefaultDebugImage.
	Image string
	// Command defaults to the image's entrypoint: a shell, for busybox.
	Command []string
	// TTY gives the container a terminal, for an interactive shell.
	TTY bool
}

// AddDebugContainer adds an ephemeral container to a running pod through
// the pod's ephemeralcontainers subresource, as kubectl debug does, and
// returns the container's name. Its stdin closes when the first client to
// attach leaves, so its shell exits then; the stopped container stays in the
// pod until the pod is replaced.
func AddDebugContainer(ctx context.Context, cs kubernetes.Interface, o DebugOptions) (string, error) {
	p, err := cs.CoreV1().Pods(o.Namespace).Get(ctx, o.Pod, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("getting pod/%s: %w", o.Pod, err)
	}
	name := debugContainerName(p)
	ec := corev1.EphemeralContainer{
		EphemeralContainerCommon: corev1.EphemeralContainerCommon{
			Name:                     name,
			Image:                    cmp.Or(o.Image, DefaultDebugImage),
			Command:                  o.Command,
			Stdin:                    true,
			StdinOnce:                true,
			TTY:                      o.TTY,
			TerminationMessagePolicy: corev1.TerminationMessageReadFile,
		},
		TargetContainerName: o.Target,
	}
	// ephemeralContainers merges by name, so the patch adds one.
	patch, err := json.Marshal(map[string]any{"spec": map[string]any{"ephemeralContainers": []corev1.EphemeralContainer{ec}}})
	if err != nil {
		return "", err
	}
	_, err = cs.CoreV1().Pods(o.Namespace).Patch(ctx, o.Pod, types.StrategicMergePatchType, patch,
		metav1.PatchOptions{FieldManager: "tug"}, "ephemeralcontainers")
	switch {
	case apierrors.IsNotFound(err):
		return "", fmt.Errorf("adding a debug container to pod/%s: %w; ephemeral containers need Kubernetes 1.25 or later", o.Pod, err)
	case err != nil:
		return "", fmt.Errorf("adding a debug container to pod/%s: %w", o.Pod, err)
	}
	return name, nil
}

// debugContainerName returns a name no container of the pod has yet, such as
// "debugger-x2x4z".
func debugContainerName(p *corev1.Pod) string {
	var taken []string
	for _, c := range append(slices.Clone(p.Spec.InitContainers), p.Spec.Containers...) {
		taken = append(taken, c.Name)
	}
	for _, c := range p.Spec.EphemeralContainers {
		taken = append(taken, c.Name)
	}
	for {
		name := "debugger-" + utilrand.String(5)
		if !slices.Contains(taken, name) {
			return name
		}
	}
}

// StartError says why a container cannot start.
type StartError struct {
	Container string
	Reason    string
	Message   string
}

func (e *StartError) Error() string {
	msg := fmt.Sprintf("container %s cannot start: %s", e.Container, e.Reason)
	if e.Message != "" {
		msg += ": " + e.Message
	}
	return msg
}

// Image reports whether the container's image is what stops it starting.
func (e *StartError) Image() bool {
	switch e.Reason {
	case "ErrImagePull", "ImagePullBackOff", "InvalidImageName", "ErrImageNeverPull":
		return true
	default:
		return false
	}
}

// WaitForEphemeralContainer waits until a pod's ephemeral container runs or
// has stopped, and returns its state. It returns a *StartError when the
// container cannot start, for example because its image cannot be pulled.
func WaitForEphemeralContainer(ctx context.Context, cs kubernetes.Interface, namespace, pod, name string) (corev1.ContainerState, error) {
	tick := time.NewTicker(debugPoll)
	defer tick.Stop()
	for {
		p, err := cs.CoreV1().Pods(namespace).Get(ctx, pod, metav1.GetOptions{})
		if err != nil {
			return corev1.ContainerState{}, fmt.Errorf("waiting for container %s of pod/%s: %w", name, pod, err)
		}
		for _, st := range p.Status.EphemeralContainerStatuses {
			if st.Name != name {
				continue
			}
			s := st.State
			if s.Running != nil || s.Terminated != nil {
				return s, nil
			}
			if w := s.Waiting; w != nil && w.Reason != "" && w.Reason != "ContainerCreating" && w.Reason != "PodInitializing" {
				return s, &StartError{Container: name, Reason: w.Reason, Message: w.Message}
			}
		}
		select {
		case <-ctx.Done():
			return corev1.ContainerState{}, ctx.Err()
		case <-tick.C:
		}
	}
}
