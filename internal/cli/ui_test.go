package cli

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/labels"

	"github.com/paingha/tugcli/internal/guard"
	"github.com/paingha/tugcli/internal/ops"
	"github.com/paingha/tugcli/internal/resolve"
	"github.com/paingha/tugcli/internal/tui"
)

func TestUIRefusesWithoutATerminal(t *testing.T) {
	isolate(t)
	srv, _ := fakeAPIServer(t)
	mustTug(t, "import", "kubeconfig", writeKubeconfig(t, srv))
	newSession(t)
	mustTug(t, "connect", "payments-prod")

	if r := tug(t, "ui"); r.code != 1 || !strings.Contains(r.stderr, "tug ui needs a terminal") {
		t.Errorf("tug ui without a terminal: %+v", r)
	}
	if r := tug(t, "ui", "--", "--readonly"); r.code != 1 || !strings.Contains(r.stderr, "tug ui --k9s -- <arguments>") {
		t.Errorf("tug ui with k9s's arguments: %+v", r)
	}
}

func TestUIK9sNeedsAConnectedSession(t *testing.T) {
	isolate(t)
	if r := tug(t, "ui", "--k9s"); r.code != 1 || !strings.Contains(r.stderr, "no tug session") {
		t.Errorf("tug ui --k9s without a session: %+v", r)
	}
	newSession(t)
	if r := tug(t, "ui", "--k9s"); r.code != 1 || !strings.Contains(r.stderr, "not connected") {
		t.Errorf("tug ui --k9s without a connection: %+v", r)
	}
}

// uiBackend is the TUI's backend on an app set up as tug's commands are,
// for a session connected to payments-prod, which is read-only.
func uiBackend(t *testing.T) (*tuiBackend, func() int32) {
	t.Helper()
	isolate(t)
	srv, writes := fakeAPIServer(t)
	mustTug(t, "import", "kubeconfig", writeKubeconfig(t, srv))
	id := newSession(t)
	mustTug(t, "connect", "payments-prod")

	in := strings.NewReader("")
	a := &app{stdin: in, in: bufio.NewReader(in), stdout: io.Discard, stderr: io.Discard, now: time.Now}
	if err := a.setup(false); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.closeTunnels)
	t.Cleanup(a.closePlugins)
	return &tuiBackend{a: a, session: id}, writes.Load
}

func TestTUIBackendGuardsWrites(t *testing.T) {
	b, writes := uiBackend(t)
	ctx := context.Background()

	st, err := b.Session()
	if err != nil || st.ClusterName != "payments-prod" || !st.ReadOnly {
		t.Fatalf("Session() = %+v, %v, want read-only payments-prod", st, err)
	}

	api := resolve.Target{Kind: resolve.KindDeployment, Name: "api-server", Namespace: "default",
		Selector: labels.SelectorFromSet(labels.Set{"app": "api-server"})}
	kube, err := b.Kube(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	err = ops.Restart(ctx, kube, api, time.Now())
	var denied *guard.DeniedError
	if !errors.As(err, &denied) || writes() != 0 {
		t.Fatalf("restart without the TUI's yes: %v, %d writes; want the guard's refusal", err, writes())
	}

	kube, err = b.Kube(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := ops.Restart(ctx, kube, api, time.Now()); err != nil || writes() != 1 {
		t.Fatalf("restart after the TUI's yes: %v, %d writes", err, writes())
	}
	// Each write's client is its own: a read client made after one that
	// allowed writes still refuses them.
	kube, err = b.Kube(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := ops.Restart(ctx, kube, api, time.Now()); !errors.As(err, &denied) || writes() != 1 {
		t.Fatalf("restart on a later read client: %v, %d writes", err, writes())
	}
}

func TestTUIBackendNamespacesAndWorkloads(t *testing.T) {
	b, _ := uiBackend(t)
	ctx := context.Background()

	names, err := b.Namespaces(ctx)
	if err != nil || !slices.Equal(names, []string{"default", "kube-system", "payments"}) {
		t.Fatalf("Namespaces() = %v, %v", names, err)
	}
	st, err := b.SetNamespace("shop")
	if err != nil || st.Namespace != "shop" {
		t.Fatalf("SetNamespace(shop) = %+v, %v", st, err)
	}
	if out := mustTug(t, "status", "-o", "name"); strings.TrimSpace(out) != "payments-prod" {
		t.Fatalf("status after SetNamespace: %q", out)
	}
	kube, err := b.Kube(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	ws, err := ops.Workloads(ctx, kube, st.Namespace)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, w := range ws {
		got = append(got, w.Target.String()+" "+w.Status)
	}
	for _, want := range []string{"deploy/checkout Ready", "deploy/billing CrashLoopBackOff", "sts/ledger Pending", "pod/scratch Running"} {
		if !slices.Contains(got, want) {
			t.Errorf("Workloads(shop) = %v, want %q among them", got, want)
		}
	}
}

func TestTUIBackendShell(t *testing.T) {
	b, writes := uiBackend(t)
	ctx := context.Background()
	if _, err := b.SetNamespace("shop"); err != nil {
		t.Fatal(err)
	}
	shell := func(r tui.ShellRequest) (string, error) {
		var out bytes.Buffer
		err := b.Shell(ctx, r, strings.NewReader(""), &out, io.Discard)
		return out.String(), err
	}

	checkout := tui.ShellRequest{Namespace: "shop", Pod: "checkout-6b8c-aaaaa", Container: "main"}
	var denied *guard.DeniedError
	if _, err := shell(checkout); !errors.As(err, &denied) {
		t.Fatalf("a shell on a read-only session without the TUI's yes: %v", err)
	}
	checkout.AllowWrites = true
	if out, err := shell(checkout); err != nil || out != "shell in checkout-6b8c-aaaaa/main\n" {
		t.Fatalf("a shell after the TUI's yes = %q, %v", out, err)
	}

	scratch := tui.ShellRequest{Namespace: "shop", Pod: "scratch", Container: "app", AllowWrites: true}
	if _, err := shell(scratch); !ops.NoShell(err) {
		t.Fatalf("a shell in a distroless container: %v, want no shell", err)
	}
	scratch.Debug = true
	out, err := shell(scratch)
	attached := regexp.MustCompile(`^attached to debugger-[a-z0-9]{5} \(image busybox:1\.36, target app, stdin true, tty false\)\n$`)
	if err != nil || !attached.MatchString(out) || writes() != 1 {
		t.Fatalf("a debug shell = %q, %v, %d writes", out, err, writes())
	}
}

func TestTUIBackendConnectAndForward(t *testing.T) {
	b, _ := uiBackend(t)
	ctx := context.Background()

	// Ports are checked before tugd is asked.
	api := resolve.Target{Kind: resolve.KindDeployment, Name: "api-server", Namespace: "default",
		Selector: labels.SelectorFromSet(labels.Set{"app": "api-server"})}
	if _, err := b.Forward(ctx, api, "api", []string{"grpc"}); err == nil || !strings.Contains(err.Error(), `has no port named "grpc"`) {
		t.Fatalf("Forward with a port the pod lacks: %v", err)
	}

	cat, err := b.Catalog()
	if err != nil {
		t.Fatal(err)
	}
	staging, err := cat.Get("payments-staging")
	if err != nil {
		t.Fatal(err)
	}
	st, err := b.Connect(ctx, staging.ID)
	if err != nil || st.ClusterName != "payments-staging" || st.ReadOnly {
		t.Fatalf("Connect(payments-staging) = %+v, %v, want read-write by its labels", st, err)
	}
	if out := mustTug(t, "status", "-o", "name"); strings.TrimSpace(out) != "payments-staging" {
		t.Fatalf("this terminal's session after Connect: %q", out)
	}
}
