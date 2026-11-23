package tui

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// exitStatus is a shell that exited non-zero, as ops.ExitCode reads it.
type exitStatus int

func (e exitStatus) Error() string {
	return fmt.Sprintf("command terminated with exit code %d", int(e))
}
func (e exitStatus) ExitStatus() int { return int(e) }

// errNoShell is what a container runtime says for an image without one.
var errNoShell = errors.New(`OCI runtime exec failed: exec failed: unable to start container process: exec: "/bin/sh": stat /bin/sh: no such file or directory: unknown`)

func prompted(m *model) string {
	if m.prompt == nil {
		return ""
	}
	return m.prompt.question
}

func flashed(m *model) string { return m.flash.text }

func TestRestartAsksFirst(t *testing.T) {
	b := newFakeBackend(shop()...).connectedTo("staging", "shop", false)
	h := newHarness(t, b)
	selectWorkload(t, h, "deploy/checkout")

	h.press("r")
	if got := prompted(h.m); got != "Restart deploy/checkout? [y/N]" {
		t.Fatalf("r asked %q", got)
	}
	h.press("enter") // no is the default
	if got := flashed(h.m); got != "Nothing changed" || len(b.patches()) != 0 {
		t.Fatalf("after no: flash %q, patches %v", got, b.patches())
	}

	h.press("r", "y")
	h.waitFor("the restart", func(m *model) bool { return flashed(m) == "Restarted deploy/checkout" })
	if p := b.patches(); len(p) != 1 || !strings.HasPrefix(p[0], "deployments/checkout ") || !strings.Contains(p[0], "kubectl.kubernetes.io/restartedAt") {
		t.Errorf("patches = %v, want the restart annotation on deploy/checkout", p)
	}
	// The write had a client of its own, which did not need to allow
	// writes on a read-write session.
	if _, _, kube := b.calls(); !slices.Equal(kube, []bool{false, false}) {
		t.Errorf("Kube calls = %v, want the read client and the write's", kube)
	}
}

func TestWritesOnAReadOnlySession(t *testing.T) {
	tests := []struct {
		name   string
		keys   []string // up to the guard's question
		action string
		patch  string // what the write patches
	}{
		{"restart", []string{"r"}, "restart deploy/checkout", "deployments/checkout"},
		{"scale", []string{"S", "backspace", "5", "enter"}, "scale deploy/checkout", `deployments/checkout {"spec":{"replicas":5}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := newFakeBackend(shop()...).connectedTo("staging", "shop", true)
			h := newHarness(t, b)
			selectWorkload(t, h, "deploy/checkout")

			h.press(tt.keys...)
			want := fmt.Sprintf("staging is read-only. %s anyway? [y/N]", capitalise(tt.action))
			if got := prompted(h.m); got != want {
				t.Fatalf("asked %q, want %q", got, want)
			}
			h.press("n")
			refused := fmt.Sprintf("refused %s: staging is read-only; run `tug unlock 15m` to allow writes", tt.action)
			if got := flashed(h.m); got != refused {
				t.Fatalf("after no, flash = %q, want %q", got, refused)
			}
			if p := b.patches(); len(p) != 0 {
				t.Fatalf("a refused write reached the cluster: %v", p)
			}

			h.press(tt.keys...)
			h.press("y")
			h.waitFor("the write", func(m *model) bool { return len(b.patches()) == 1 })
			if p := b.patches()[0]; !strings.HasPrefix(p, tt.patch) {
				t.Errorf("patch = %q, want %q", p, tt.patch)
			}
			if _, _, kube := b.calls(); kube[len(kube)-1] != true {
				t.Errorf("Kube calls = %v; the write's client should allow writes", kube)
			}
		})
	}
}

// TestRefusalSuggestsTheUnlockAllowed checks that a write refused in the
// TUI suggests the unlock a team policy allows, as tug's commands do.
func TestRefusalSuggestsTheUnlockAllowed(t *testing.T) {
	b := newFakeBackend(shop()...).connectedTo("staging", "shop", true)
	b.unlock = 10 * time.Minute
	h := newHarness(t, b)
	selectWorkload(t, h, "deploy/checkout")

	h.press("r", "n")
	want := "refused restart deploy/checkout: staging is read-only; run `tug unlock 10m` to allow writes"
	if got := flashed(h.m); got != want {
		t.Fatalf("after no, flash = %q, want %q", got, want)
	}
}

func TestWritesTheKindCannotTake(t *testing.T) {
	b := newFakeBackend(shop()...).connectedTo("staging", "shop", false)
	h := newHarness(t, b)
	selectWorkload(t, h, "pod/debug")
	h.press("r")
	if prompted(h.m) != "" || !strings.Contains(flashed(h.m), "cannot restart pod/debug") {
		t.Errorf("r on a pod: prompt %q, flash %q", prompted(h.m), flashed(h.m))
	}
	h.press("S")
	if prompted(h.m) != "" || !strings.Contains(flashed(h.m), "cannot scale pod/debug") {
		t.Errorf("S on a pod: prompt %q, flash %q", prompted(h.m), flashed(h.m))
	}
}

func TestScale(t *testing.T) {
	b := newFakeBackend(shop()...).connectedTo("staging", "shop", false)
	h := newHarness(t, b)
	selectWorkload(t, h, "deploy/checkout")

	h.press("S")
	if got := prompted(h.m); got != "Scale deploy/checkout to how many replicas?" {
		t.Fatalf("S asked %q", got)
	}
	if v := h.m.prompt.values(); !slices.Equal(v, []string{"2"}) {
		t.Fatalf("the prompt starts from %v, want the current 2", v)
	}
	// Keys type into the prompt instead of acting.
	h.press("backspace")
	h.typeText("lots")
	h.press("enter")
	if h.m.prompt == nil || !strings.Contains(h.screen(), `replicas must be a number from 0, got "lots"`) {
		t.Fatalf("a bad count closed the prompt or said nothing:\n%s", h.screen())
	}
	for range 4 {
		h.press("backspace")
	}
	h.typeText("0")
	h.press("enter")
	h.waitFor("the scale", func(m *model) bool { return flashed(m) == "Scaled deploy/checkout to 0" })
	if p := b.patches(); len(p) != 1 || p[0] != `deployments/checkout {"spec":{"replicas":0}}` {
		t.Errorf("patches = %v", p)
	}

	// esc cancels without a word.
	h.press("S", "esc")
	if h.m.prompt != nil || len(b.patches()) != 1 {
		t.Errorf("esc: prompt %v, patches %v", h.m.prompt, b.patches())
	}
}

func TestShell(t *testing.T) {
	b := newFakeBackend(shop()...).connectedTo("staging", "shop", false)
	h := newHarness(t, b)
	selectWorkload(t, h, "deploy/checkout")

	h.press("s")
	h.waitFor("the shell", func(m *model) bool { return strings.HasPrefix(flashed(m), "Left the shell") })
	want := ShellRequest{Namespace: "shop", Pod: "checkout-7d9f-aaaaa", Container: "main"}
	if got := b.shellCalls(); !slices.Equal(got, []ShellRequest{want}) {
		t.Fatalf("Shell calls = %+v, want %+v", got, want)
	}

	b.shellErr = func(ShellRequest) error { return exitStatus(3) }
	h.press("s")
	h.waitFor("the shell", func(m *model) bool { return strings.Contains(flashed(m), "exited with status 3") })
}

func TestShellOnAReadOnlySession(t *testing.T) {
	b := newFakeBackend(shop()...).connectedTo("staging", "shop", true)
	h := newHarness(t, b)
	selectWorkload(t, h, "deploy/checkout")

	h.press("s")
	question := "staging is read-only. Run a command in pod/checkout-7d9f-aaaaa anyway? [y/N]"
	h.waitFor("the guard's question", func(m *model) bool { return prompted(m) == question })
	h.press("n")
	if got := flashed(h.m); !strings.HasPrefix(got, "refused Run a command in pod/checkout-7d9f-aaaaa: staging is read-only") {
		t.Fatalf("after no, flash = %q", got)
	}
	if len(b.shellCalls()) != 0 {
		t.Fatal("a refused shell ran")
	}

	h.press("s")
	h.waitFor("the guard's question", func(m *model) bool { return prompted(m) == question })
	h.press("y")
	h.waitFor("the shell", func(m *model) bool { return strings.HasPrefix(flashed(m), "Left the shell") })
	if got := b.shellCalls(); len(got) != 1 || !got[0].AllowWrites {
		t.Fatalf("Shell calls = %+v, want one that may write", got)
	}
}

func TestShellOffersADebugContainer(t *testing.T) {
	b := newFakeBackend(shop()...).connectedTo("staging", "shop", true)
	b.shellErr = func(r ShellRequest) error {
		if r.Debug {
			return nil
		}
		return errNoShell
	}
	h := newHarness(t, b)
	selectWorkload(t, h, "pod/debug")

	h.press("s")
	h.waitFor("the guard's question", func(m *model) bool { return strings.Contains(prompted(m), "Run a command in pod/debug anyway?") })
	h.press("y")
	h.waitFor("the offer", func(m *model) bool {
		return prompted(m) == "Container main of pod/debug has no shell. Add an ephemeral debug container (busybox:1.36) to the pod? [y/N]"
	})
	// Adding the container is a write of its own, which the guard asks about.
	h.press("y")
	if got := prompted(h.m); got != "staging is read-only. Add a debug container to pod/debug anyway? [y/N]" {
		t.Fatalf("asked %q", got)
	}
	h.press("y")
	h.waitFor("the debug shell", func(m *model) bool { return strings.HasPrefix(flashed(m), "Left the shell") })
	got := b.shellCalls()
	if len(got) != 2 || !got[1].Debug || !got[1].AllowWrites || got[1].Container != "main" {
		t.Fatalf("Shell calls = %+v, want a debug shell beside main that may write", got)
	}

	// Saying no to the offer says how to get one later.
	h.press("s")
	h.waitFor("the guard's question", func(m *model) bool { return strings.Contains(prompted(m), "anyway?") })
	h.press("y")
	h.waitFor("the offer", func(m *model) bool { return strings.Contains(prompted(m), "has no shell") })
	h.press("n")
	if got := flashed(h.m); !strings.Contains(got, "container main of pod/debug has no shell") {
		t.Fatalf("after no, flash = %q", got)
	}
}

func TestForward(t *testing.T) {
	// A port-forward is a read, so a read-only session does not ask.
	b := newFakeBackend(shop()...).connectedTo("staging", "shop", true)
	h := newHarness(t, b)
	selectWorkload(t, h, "deploy/checkout")

	h.press("p")
	if got := prompted(h.m); !strings.HasPrefix(got, "Port-forward deploy/checkout through tugd") {
		t.Fatalf("p asked %q", got)
	}
	if v := h.m.prompt.values(); !slices.Equal(v, []string{"", "checkout"}) {
		t.Fatalf("the prompt starts from %v", v)
	}
	h.typeText("8080 9000:http")
	h.press("enter") // to the name
	h.press("enter")
	h.waitFor("the forward", func(m *model) bool { return strings.HasPrefix(flashed(m), "Forwarding") })
	want := "Forwarding localhost:8080 -> 8080, localhost:9000 -> 8080 to deploy/checkout (pod/checkout-7d9f-aaaaa) as checkout; tug pf ls shows it and tug pf stop checkout ends it"
	if got := flashed(h.m); got != want {
		t.Errorf("flash = %q, want %q", got, want)
	}
	if got := b.forwardCalls(); len(got) != 1 || got[0].name != "checkout" || !slices.Equal(got[0].ports, []string{"8080", "9000:http"}) {
		t.Errorf("Forward calls = %+v", got)
	}

	// A forward needs a name; tugd's own errors show as they come.
	h.press("p", "tab")
	for range len("checkout") {
		h.press("backspace")
	}
	h.press("enter")
	if h.m.prompt == nil || !strings.Contains(h.screen(), "name the forward") {
		t.Fatalf("an empty name closed the prompt:\n%s", h.screen())
	}
	h.press("esc")
	b.forwardErr = errors.New(`starting the forward checkout: a forward with that name is running`)
	h.press("p", "tab", "enter")
	h.waitFor("the error", func(m *model) bool { return strings.Contains(flashed(m), "a forward with that name is running") })
}

func TestActionsFromAPane(t *testing.T) {
	b := newFakeBackend(shop()...).connectedTo("staging", "shop", false)
	h := newHarness(t, b)
	selectWorkload(t, h, "deploy/checkout")
	h.press("w")
	h.waitForView("Why deploy/checkout")
	h.press("r")
	if got := prompted(h.m); got != "Restart deploy/checkout? [y/N]" {
		t.Fatalf("r in a pane asked %q", got)
	}
	h.press("y")
	h.waitFor("the restart", func(m *model) bool { return flashed(m) == "Restarted deploy/checkout" })
	if h.m.pane == nil {
		t.Error("the write closed the pane")
	}
}

func TestHelpListsEveryAction(t *testing.T) {
	b := newFakeBackend(shop()...).connectedTo("staging", "shop", false)
	h := newHarness(t, b)
	h.waitFor("the workloads", func(m *model) bool { return m.workloads.loaded })
	title := h.screen()
	h.press("?")
	help := h.screen()
	for _, kb := range h.m.keys.actions() {
		k := kb.Help().Key
		if !strings.Contains(help, "  "+k+" ") {
			t.Errorf("help lacks %s:\n%s", k, help)
		}
		if !strings.Contains(title, k+" "+hintNames[k]) {
			t.Errorf("the title line lacks %s %s:\n%s", k, hintNames[k], title)
		}
	}
	for _, want := range []string{"read-only session", "ask first"} {
		if !strings.Contains(help, want) {
			t.Errorf("help lacks %q", want)
		}
	}
}
