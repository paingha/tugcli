package cli

import (
	"crypto/sha1" // the WebSocket handshake is defined with SHA-1
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
)

// fakeResource is how the fake API server lists one resource.
type fakeResource struct{ groupVersion, listKind string }

var fakeResources = map[string]fakeResource{
	"pods":                   {"v1", "PodList"},
	"events":                 {"v1", "EventList"},
	"services":               {"v1", "ServiceList"},
	"configmaps":             {"v1", "ConfigMapList"},
	"secrets":                {"v1", "SecretList"},
	"persistentvolumeclaims": {"v1", "PersistentVolumeClaimList"},
	"deployments":            {"apps/v1", "DeploymentList"},
	"replicasets":            {"apps/v1", "ReplicaSetList"},
	"statefulsets":           {"apps/v1", "StatefulSetList"},
	"daemonsets":             {"apps/v1", "DaemonSetList"},
	"controllerrevisions":    {"apps/v1", "ControllerRevisionList"},
	"jobs":                   {"batch/v1", "JobList"},
}

// resourceOf returns the resource a typed object is served as.
func resourceOf(o runtime.Object) string {
	switch o.(type) {
	case *corev1.Pod:
		return "pods"
	case *corev1.Event:
		return "events"
	case *corev1.Service:
		return "services"
	case *corev1.ConfigMap:
		return "configmaps"
	case *corev1.Secret:
		return "secrets"
	case *corev1.PersistentVolumeClaim:
		return "persistentvolumeclaims"
	case *appsv1.Deployment:
		return "deployments"
	case *appsv1.ReplicaSet:
		return "replicasets"
	case *appsv1.StatefulSet:
		return "statefulsets"
	case *appsv1.DaemonSet:
		return "daemonsets"
	case *appsv1.ControllerRevision:
		return "controllerrevisions"
	case *batchv1.Job:
		return "jobs"
	default:
		panic(fmt.Sprintf("fake API server cannot serve %T", o))
	}
}

// fakeNamespace serves typed objects in one namespace of the fake API
// server. Lists honour label selectors, as the real API server does, so
// several workloads can share the namespace.
type fakeNamespace struct {
	t  *testing.T
	mu sync.Mutex
	// objects are keyed by resource, such as "pods".
	objects map[string][]runtime.Object
}

func newFakeNamespace(t *testing.T, objs ...runtime.Object) *fakeNamespace {
	f := &fakeNamespace{t: t, objects: map[string][]runtime.Object{}}
	for _, o := range objs {
		r := resourceOf(o)
		f.objects[r] = append(f.objects[r], o)
	}
	return f
}

// get returns the named object of a resource, or nil.
func (f *fakeNamespace) get(resource, name string) runtime.Object {
	for _, o := range f.objects[resource] {
		if m, err := meta.Accessor(o); err == nil && m.GetName() == name {
			return o
		}
	}
	return nil
}

// serve answers a request for /api[s]/<gv>/namespaces/<ns>/<resource>[/<name>[/<sub>]],
// split into its parts after the namespace.
func (f *fakeNamespace) serve(w http.ResponseWriter, r *http.Request, parts []string) {
	if len(parts) == 3 && parts[0] == "pods" && (parts[2] == "exec" || parts[2] == "attach") {
		f.stream(w, r, parts[1], parts[2])
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	resource := parts[0]
	res, ok := fakeResources[resource]
	if !ok {
		writeStatus(w, http.StatusNotFound, "NotFound", resource+" not served")
		return
	}
	switch {
	case len(parts) == 1 && r.Method == http.MethodGet:
		sel, err := labels.Parse(r.URL.Query().Get("labelSelector"))
		if err != nil {
			writeStatus(w, http.StatusBadRequest, "BadRequest", err.Error())
			return
		}
		items := []runtime.Object{}
		for _, o := range f.objects[resource] {
			if m, err := meta.Accessor(o); err == nil && sel.Matches(labels.Set(m.GetLabels())) {
				items = append(items, o)
			}
		}
		writeJSON(f.t, w, map[string]any{"kind": res.listKind, "apiVersion": res.groupVersion, "metadata": map[string]any{}, "items": items})
	case len(parts) == 2 && r.Method == http.MethodGet:
		o := f.get(resource, parts[1])
		if o == nil {
			writeStatus(w, http.StatusNotFound, "NotFound", fmt.Sprintf("%s %q not found", resource, parts[1]))
			return
		}
		writeJSON(f.t, w, o)
	case len(parts) == 3 && resource == "pods" && parts[2] == "log":
		w.Header().Set("Content-Type", "text/plain")
		container := r.URL.Query().Get("container")
		if p, ok := f.get("pods", parts[1]).(*corev1.Pod); ok && ephemeral(p, container) != nil {
			fmt.Fprintf(w, "log of %s\n", describeEphemeral(ephemeral(p, container)))
			return
		}
		fmt.Fprintf(w, "log of %s/%s\n", parts[1], container)
	case len(parts) == 3 && resource == "pods" && parts[2] == "ephemeralcontainers" && r.Method == http.MethodPatch:
		f.addEphemeral(w, r, parts[1])
	default:
		writeStatus(w, http.StatusMethodNotAllowed, "MethodNotAllowed", r.Method+" "+strings.Join(parts, "/"))
	}
}

// noShell is what containerd says when a distroless image has no /bin/sh.
const noShell = `OCI runtime exec failed: exec failed: unable to start container process: exec: "/bin/sh": stat /bin/sh: no such file or directory: unknown`

// stream answers an exec or attach. Containers whose image is distroless have
// no shell; others run the command, which here means saying what was run.
func (f *fakeNamespace) stream(w http.ResponseWriter, r *http.Request, pod, sub string) {
	f.mu.Lock()
	p, _ := f.get("pods", pod).(*corev1.Pod)
	f.mu.Unlock()
	q := r.URL.Query()
	container := q.Get("container")
	if p == nil {
		writeStatus(w, http.StatusNotFound, "NotFound", "pod "+pod+" not found")
		return
	}
	if sub == "attach" {
		ec := ephemeral(p, container)
		if ec == nil {
			writeStatus(w, http.StatusBadRequest, "BadRequest", "fake API server attaches only to ephemeral containers")
			return
		}
		serveStream(f.t, w, r, "attached to "+describeEphemeral(ec)+"\n", metav1.Status{Status: metav1.StatusSuccess})
		return
	}
	for _, c := range p.Spec.Containers {
		if c.Name == container && strings.Contains(c.Image, "distroless") {
			serveStream(f.t, w, r, "", metav1.Status{Status: metav1.StatusFailure, Reason: metav1.StatusReasonInternalError, Message: noShell})
			return
		}
	}
	command := q["command"]
	out := fmt.Sprintf("ran in %s/%s: %s\n", pod, container, strings.Join(command, " "))
	if len(command) == 3 && command[0] == "/bin/sh" {
		out = fmt.Sprintf("shell in %s/%s\n", pod, container)
	}
	serveStream(f.t, w, r, out, metav1.Status{Status: metav1.StatusSuccess})
}

// addEphemeral applies a patch to a pod's ephemeralcontainers subresource.
// A container with a command has already finished when the patch returns;
// one without runs its shell.
func (f *fakeNamespace) addEphemeral(w http.ResponseWriter, r *http.Request, pod string) {
	p, _ := f.get("pods", pod).(*corev1.Pod)
	if p == nil {
		writeStatus(w, http.StatusNotFound, "NotFound", "pod "+pod+" not found")
		return
	}
	if ct := r.Header.Get("Content-Type"); ct != string(types.StrategicMergePatchType) {
		writeStatus(w, http.StatusUnsupportedMediaType, "UnsupportedMediaType", ct)
		return
	}
	var patch struct {
		Spec struct {
			EphemeralContainers []corev1.EphemeralContainer `json:"ephemeralContainers"`
		} `json:"spec"`
	}
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		writeStatus(w, http.StatusBadRequest, "BadRequest", err.Error())
		return
	}
	for _, ec := range patch.Spec.EphemeralContainers {
		p.Spec.EphemeralContainers = append(p.Spec.EphemeralContainers, ec)
		state := corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
		if len(ec.Command) > 0 {
			state = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0, Reason: "Completed"}}
		}
		p.Status.EphemeralContainerStatuses = append(p.Status.EphemeralContainerStatuses, corev1.ContainerStatus{Name: ec.Name, State: state})
	}
	writeJSON(f.t, w, p)
}

func ephemeral(p *corev1.Pod, name string) *corev1.EphemeralContainer {
	for i := range p.Spec.EphemeralContainers {
		if p.Spec.EphemeralContainers[i].Name == name {
			return &p.Spec.EphemeralContainers[i]
		}
	}
	return nil
}

// describeEphemeral says what a debug container was asked to be, so tests
// can check it in tug's output.
func describeEphemeral(ec *corev1.EphemeralContainer) string {
	s := fmt.Sprintf("%s (image %s, target %s, stdin %v, tty %v", ec.Name, ec.Image, ec.TargetContainerName, ec.Stdin, ec.TTY)
	if len(ec.Command) > 0 {
		s += ", command " + strings.Join(ec.Command, " ")
	}
	return s + ")"
}

// streamProtocol is the remote command protocol the fake API server speaks.
const streamProtocol = "v5.channel.k8s.io"

// serveStream answers an exec or attach over WebSockets as the kubelet does:
// out on the stdout channel (1), the status on the error channel (3), then a
// normal close. What the client sends is read and dropped until it hangs up.
func serveStream(t *testing.T, w http.ResponseWriter, r *http.Request, out string, status metav1.Status) {
	if !strings.Contains(r.Header.Get("Sec-WebSocket-Protocol"), streamProtocol) {
		writeStatus(w, http.StatusBadRequest, "BadRequest", "want a WebSocket upgrade to "+streamProtocol)
		return
	}
	accept := sha1.Sum([]byte(r.Header.Get("Sec-WebSocket-Key") + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	conn, rw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		t.Errorf("fake API server: hijacking the connection: %v", err)
		return
	}
	defer conn.Close()
	fmt.Fprintf(rw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"+
		"Sec-WebSocket-Accept: %s\r\nSec-WebSocket-Protocol: %s\r\n\r\n", base64.StdEncoding.EncodeToString(accept[:]), streamProtocol)
	frame := func(opcode byte, payload []byte) {
		_ = rw.WriteByte(0x80 | opcode) // final fragment
		switch n := len(payload); {
		case n < 126:
			_ = rw.WriteByte(byte(n))
		default:
			_ = rw.WriteByte(127)
			_ = binary.Write(rw, binary.BigEndian, uint64(n))
		}
		_, _ = rw.Write(payload)
	}
	const binaryFrame, closeFrame = 2, 8
	if out != "" {
		frame(binaryFrame, append([]byte{1}, out...))
	}
	b, _ := json.Marshal(status)
	frame(binaryFrame, append([]byte{3}, b...))
	frame(closeFrame, []byte{0x03, 0xe8}) // 1000, normal closure
	if err := rw.Flush(); err != nil {
		t.Errorf("fake API server: writing the stream: %v", err)
		return
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, _ = io.Copy(io.Discard, rw) // until the client closes, so it reads everything first
}

// namespacedParts splits a namespaced API path into its namespace and the
// parts after it: "/api/v1/namespaces/shop/pods/x" is "shop", [pods x].
func namespacedParts(path string) (string, []string, bool) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	for i := 0; i+2 < len(parts); i++ {
		if parts[i] == "namespaces" {
			return parts[i+1], parts[i+2:], true
		}
	}
	return "", nil, false
}

func writeJSON(t *testing.T, w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Errorf("fake API server: encoding %T: %v", v, err)
	}
}

func writeStatus(w http.ResponseWriter, code int, reason, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	b, _ := json.Marshal(map[string]any{
		"kind": "Status", "apiVersion": "v1", "status": "Failure", "code": code, "reason": reason, "message": msg,
	})
	_, _ = w.Write(b)
}
