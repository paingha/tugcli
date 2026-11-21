package tui

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/paingha/tugcli/internal/catalog"
	"github.com/paingha/tugcli/internal/daemon"
	"github.com/paingha/tugcli/internal/guard"
	"github.com/paingha/tugcli/internal/resolve"
	"github.com/paingha/tugcli/internal/session"
)

// harness runs a model as Bubble Tea would, without a terminal: commands
// run on goroutines of their own and their messages come back to Update on
// the test's goroutine, one at a time.
type harness struct {
	t     *testing.T
	m     *model
	tasks *tasks
	msgs  chan tea.Msg
	done  chan struct{}
	quit  bool
}

func newHarness(t *testing.T, b *fakeBackend, opts ...Option) *harness {
	t.Helper()
	st, err := b.Session()
	if err != nil {
		t.Fatal(err)
	}
	tk := newTasks(context.Background())
	h := &harness{t: t, tasks: tk, msgs: make(chan tea.Msg, 64), done: make(chan struct{})}
	// The ticker stays off unless a test turns it on.
	h.m = newModel(b, tk, st, newStyles(io.Discard), append([]Option{WithRefresh(0)}, opts...)...)
	// Bubble Tea would hand the command the terminal; here it gets none.
	h.m.exec = func(c tea.ExecCommand, fn tea.ExecCallback) tea.Cmd {
		return func() tea.Msg {
			c.SetStdin(strings.NewReader(""))
			c.SetStdout(io.Discard)
			c.SetStderr(io.Discard)
			return fn(c.Run())
		}
	}
	t.Cleanup(h.stop)
	h.update(tea.WindowSizeMsg{Width: 140, Height: 30})
	h.run(h.m.Init())
	return h
}

// stop ends the program's background work, as Run does, and fails the test
// when some of it does not stop.
func (h *harness) stop() {
	stopped := make(chan struct{})
	go func() {
		h.tasks.stop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		h.t.Error("background work did not stop when the program ended")
	}
	close(h.done)
}

// run starts a command, as Bubble Tea does.
func (h *harness) run(cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	go func() {
		msg := cmd()
		select {
		case h.msgs <- msg:
		case <-h.done:
		}
	}()
}

func (h *harness) update(msg tea.Msg) {
	switch msg := msg.(type) {
	case nil:
	case tea.BatchMsg:
		for _, cmd := range msg {
			h.run(cmd)
		}
	case tea.QuitMsg:
		h.quit = true
	default:
		_, cmd := h.m.Update(msg)
		h.run(cmd)
	}
}

// waitFor hands messages to the model until cond holds.
func (h *harness) waitFor(what string, cond func(m *model) bool) {
	h.t.Helper()
	deadline := time.After(5 * time.Second)
	for !cond(h.m) {
		select {
		case msg := <-h.msgs:
			h.update(msg)
		case <-deadline:
			h.t.Fatalf("waiting for %s; the screen is:\n%s", what, h.m.View())
		}
	}
}

// settle hands messages to the model until none has come for a while, to
// check that something did not happen.
func (h *harness) settle() {
	for {
		select {
		case msg := <-h.msgs:
			h.update(msg)
		case <-time.After(50 * time.Millisecond):
			return
		}
	}
}

// waitForQuit hands messages to the model until it quits.
func (h *harness) waitForQuit() {
	h.t.Helper()
	h.waitFor("the program to quit", func(*model) bool { return h.quit })
}

// waitForView hands messages to the model until its view shows text.
func (h *harness) waitForView(text string) {
	h.t.Helper()
	h.waitFor("the view to show "+text, func(m *model) bool { return strings.Contains(m.View(), text) })
}

// press sends keys: names such as enter, esc and ctrl+c, or a rune each.
func (h *harness) press(keys ...string) {
	for _, k := range keys {
		h.update(keyMsg(k))
	}
}

// typeText sends each rune of s as a key.
func (h *harness) typeText(s string) {
	for _, r := range s {
		h.update(keyMsg(string(r)))
	}
}

func keyMsg(k string) tea.KeyMsg {
	names := map[string]tea.KeyType{
		"enter": tea.KeyEnter, "esc": tea.KeyEsc, "up": tea.KeyUp, "down": tea.KeyDown,
		"backspace": tea.KeyBackspace, "tab": tea.KeyTab, "shift+tab": tea.KeyShiftTab,
		"ctrl+c": tea.KeyCtrlC, "ctrl+r": tea.KeyCtrlR, "pgdown": tea.KeyPgDown, "end": tea.KeyEnd,
		" ": tea.KeySpace,
	}
	if t, ok := names[k]; ok {
		if t == tea.KeySpace {
			return tea.KeyMsg{Type: t, Runes: []rune{' '}}
		}
		return tea.KeyMsg{Type: t}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
}

// fakeBackend stands in for internal/cli. Its clusters' kube client is a
// fake clientset whose guard refuses writes on a read-only session, unless
// the last client asked for allowed them, as each write gets a client of
// its own.
type fakeBackend struct {
	mu         sync.Mutex
	clusters   []catalog.Cluster
	state      session.State
	cs         *fake.Clientset
	namespaces []string
	nsErr      error
	kubeErr    error
	catalogErr error

	connects   []string
	setNS      []string
	kubeCalls  []bool
	lastAllow  bool
	sessionErr error

	forwards   []forwardCall
	forwardErr error
	shells     []ShellRequest
	// shellErr, when set, says how a shell ends.
	shellErr func(ShellRequest) error
}

var testClusters = []catalog.Cluster{
	{
		ID: "kubeconfig/work/prod-eu", Name: "prod-eu", Provider: "kubeconfig", Region: "eu-west-1",
		Labels: map[string]string{"env": "prod", "region": "eu-west-1", "team": "payments"},
	},
	{
		ID: "kubeconfig/work/staging", Name: "staging", Provider: "kubeconfig", Region: "eu-west-1",
		Labels: map[string]string{"env": "staging", "region": "eu-west-1"}, DefaultNamespace: "shop",
	},
	{
		ID: "aws/123/gpu-us", Name: "gpu-us", Provider: "aws", Region: "us-east-1", Stale: true,
		Labels: map[string]string{"gpu": "true", "region": "us-east-1"}, Tunnel: "ssh://alice:hunter2@bastion.example.com",
	},
}

func newFakeBackend(objects ...runtime.Object) *fakeBackend {
	b := &fakeBackend{
		clusters:   testClusters,
		state:      session.State{ID: "abc123"},
		cs:         fake.NewSimpleClientset(objects...),
		namespaces: []string{"default", "kube-system", "shop"},
	}
	b.cs.PrependReactor("*", "*", func(a k8stesting.Action) (bool, runtime.Object, error) {
		switch a.GetVerb() {
		case "get", "list", "watch":
			return false, nil, nil
		}
		b.mu.Lock()
		defer b.mu.Unlock()
		if b.state.Writable(time.Now()) || b.lastAllow {
			return false, nil, nil
		}
		return true, nil, &guard.DeniedError{Cluster: b.state.ClusterName, Action: strings.ToUpper(a.GetVerb()) + " " + a.GetResource().Resource}
	})
	return b
}

// connectedTo points the fake session at a cluster.
func (b *fakeBackend) connectedTo(name, ns string, readOnly bool) *fakeBackend {
	for _, cl := range b.clusters {
		if cl.Name == name {
			b.state = session.State{ID: "abc123", ClusterID: cl.ID, ClusterName: cl.Name, Namespace: ns, ReadOnly: readOnly}
		}
	}
	return b
}

func (b *fakeBackend) Catalog() (*catalog.Catalog, error) {
	if b.catalogErr != nil {
		return nil, b.catalogErr
	}
	return &catalog.Catalog{APIVersion: catalog.APIVersion, Clusters: b.clusters}, nil
}

func (b *fakeBackend) Session() (session.State, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state, b.sessionErr
}

func (b *fakeBackend) Connect(_ context.Context, clusterID string) (session.State, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.connects = append(b.connects, clusterID)
	for _, cl := range b.clusters {
		if cl.ID != clusterID {
			continue
		}
		ns := cl.DefaultNamespace
		if ns == "" {
			ns = "default"
		}
		b.state = session.State{ID: b.state.ID, ClusterID: cl.ID, ClusterName: cl.Name, Namespace: ns, ReadOnly: cl.Labels["env"] == "prod"}
		if cl.Tunnel != "" {
			b.state.ProxyURL = "https://127.0.0.1:7443"
		}
		return b.state, nil
	}
	return session.State{}, catalog.ErrNotFound
}

func (b *fakeBackend) Namespaces(context.Context) ([]string, error) {
	return b.namespaces, b.nsErr
}

func (b *fakeBackend) SetNamespace(ns string) (session.State, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.setNS = append(b.setNS, ns)
	b.state.Namespace = ns
	return b.state, nil
}

func (b *fakeBackend) Kube(_ context.Context, allowWrites bool) (kubernetes.Interface, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.kubeCalls = append(b.kubeCalls, allowWrites)
	b.lastAllow = allowWrites
	if b.kubeErr != nil {
		return nil, b.kubeErr
	}
	return b.cs, nil
}

// forwardCall is one call of Forward.
type forwardCall struct {
	target, name string
	ports        []string
}

func (b *fakeBackend) Forward(_ context.Context, t resolve.Target, name string, ports []string) (daemon.ForwardState, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.forwards = append(b.forwards, forwardCall{target: t.String(), name: name, ports: ports})
	if b.forwardErr != nil {
		return daemon.ForwardState{}, b.forwardErr
	}
	return daemon.ForwardState{
		Forward: daemon.Forward{Name: name, Target: t.String(), Ports: ports},
		State:   daemon.ForwardUp, Bound: []string{"8080:8080", "9000:8080"}, Pod: t.Name + "-7d9f-aaaaa",
	}, nil
}

func (b *fakeBackend) Shell(_ context.Context, r ShellRequest, _ io.Reader, _, _ io.Writer) error {
	b.mu.Lock()
	b.shells = append(b.shells, r)
	end := b.shellErr
	b.mu.Unlock()
	if end != nil {
		return end(r)
	}
	return nil
}

func (b *fakeBackend) shellCalls() []ShellRequest {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]ShellRequest(nil), b.shells...)
}

func (b *fakeBackend) forwardCalls() []forwardCall {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]forwardCall(nil), b.forwards...)
}

// patches lists the patches that reached the fake cluster, as
// resource/name.
func (b *fakeBackend) patches() []string {
	var out []string
	for _, a := range b.cs.Actions() {
		if p, ok := a.(k8stesting.PatchAction); ok {
			out = append(out, p.GetResource().Resource+"/"+p.GetName()+" "+string(p.GetPatch()))
		}
	}
	return out
}

func (b *fakeBackend) calls() (connects, setNS []string, kube []bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.connects...), append([]string(nil), b.setNS...), append([]bool(nil), b.kubeCalls...)
}

// shop is a namespace with a healthy deployment, a crashing one and a bare
// pod.
func shop() []runtime.Object {
	two, one := int32(2), int32(1)
	deploy := func(name string, replicas *int32, ready int32) *appsv1.Deployment {
		return &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "shop", Generation: 1, UID: types.UID("uid-" + name),
				CreationTimestamp: metav1.NewTime(time.Now().Add(-3 * time.Hour))},
			Spec: appsv1.DeploymentSpec{
				Replicas: replicas,
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": name}},
					Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "main"}}},
				},
			},
			Status: appsv1.DeploymentStatus{ObservedGeneration: 1, Replicas: *replicas, UpdatedReplicas: *replicas,
				ReadyReplicas: ready, AvailableReplicas: ready},
		}
	}
	pod := func(name, app string, ready bool, waiting string) *corev1.Pod {
		yes := true
		p := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "shop", Labels: map[string]string{"app": app},
				OwnerReferences: []metav1.OwnerReference{{Kind: "ReplicaSet", Name: app + "-7d9f", Controller: &yes}}},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Ports: []corev1.ContainerPort{{Name: "http", ContainerPort: 8080}}}}},
			Status: corev1.PodStatus{
				Phase:      corev1.PodRunning,
				Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
				ContainerStatuses: []corev1.ContainerStatus{{Name: "main", Ready: ready,
					State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}},
			},
		}
		if !ready {
			p.Status.Conditions[0].Status = corev1.ConditionFalse
			p.Status.ContainerStatuses[0].RestartCount = 7
			p.Status.ContainerStatuses[0].State = corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: waiting}}
		}
		return p
	}
	bare := pod("debug", "debug", true, "")
	bare.OwnerReferences = nil
	return []runtime.Object{
		deploy("checkout", &two, 2), pod("checkout-7d9f-aaaaa", "checkout", true, ""), pod("checkout-7d9f-bbbbb", "checkout", true, ""),
		deploy("billing", &one, 0), pod("billing-7d9f-ccccc", "billing", false, "CrashLoopBackOff"),
		bare,
	}
}

var errNoCredentials = errors.New("getting credentials for prod-eu: token expired")
