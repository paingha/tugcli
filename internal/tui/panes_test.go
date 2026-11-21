package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/paingha/tugcli/internal/ops"
)

// selectWorkload moves the workloads cursor to a target.
func selectWorkload(t *testing.T, h *harness, target string) {
	t.Helper()
	h.waitFor("the workloads", func(m *model) bool { return m.workloads.loaded })
	for i, idx := range h.m.workloads.shown {
		if h.m.workloads.items[idx].Target.String() == target {
			h.m.workloads.cursor = i
			return
		}
	}
	t.Fatalf("%s is not listed:\n%s", target, h.m.View())
}

func TestLogsPane(t *testing.T) {
	b := newFakeBackend(shop()...).connectedTo("staging", "shop", false)
	h := newHarness(t, b)
	selectWorkload(t, h, "deploy/checkout")

	h.press("l")
	if h.m.pane == nil || h.m.pane.kind != paneLogs {
		t.Fatalf("l opened %+v, want a log pane", h.m.pane)
	}
	// Every pod of the workload, each with its pod and container.
	h.waitForView("checkout-7d9f-aaaaa/main fake logs")
	h.waitForView("checkout-7d9f-bbbbb/main fake logs")
	v := h.m.View()
	for _, want := range []string{"Logs of deploy/checkout", "following new lines and new pods"} {
		if !strings.Contains(v, want) {
			t.Errorf("View() lacks %q:\n%s", want, v)
		}
	}

	// The stream follows new pods until the pane closes, and closing it
	// ends the stream.
	events := h.m.pane.events
	h.press("esc")
	if h.m.pane != nil {
		t.Fatal("esc left the pane open")
	}
	deadline := time.After(5 * time.Second)
	for {
		select {
		case _, ok := <-events:
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("the log stream outlived its pane")
		}
	}
}

func TestLogsPaneFollowsUntilScrolledUp(t *testing.T) {
	b := newFakeBackend(shop()...).connectedTo("staging", "shop", false)
	h := newHarness(t, b)
	selectWorkload(t, h, "deploy/checkout")
	h.press("l")
	h.waitForView("checkout-7d9f-aaaaa/main fake logs")

	var events []logEvent
	for i := range 100 {
		events = append(events, logEvent{line: ops.LogLine{Pod: "debug", Container: "main", Text: fmt.Sprintf("line %d", i)}})
	}
	h.update(logsMsg{id: h.m.pane.id, events: events})
	if v := h.m.View(); !strings.Contains(v, "line 99") || strings.Contains(v, "line 50") {
		t.Fatalf("a following pane does not show the newest lines:\n%s", v)
	}
	h.press("up")
	if h.m.pane.follow || !strings.Contains(h.m.View(), "paused") {
		t.Fatalf("scrolling up did not pause following:\n%s", h.m.View())
	}
	h.update(logsMsg{id: h.m.pane.id, events: []logEvent{{line: ops.LogLine{Pod: "debug", Container: "main", Text: "line 100"}}}})
	if strings.Contains(h.m.View(), "line 100") {
		t.Fatalf("a paused pane jumped to a new line:\n%s", h.m.View())
	}
	h.press("G")
	if !h.m.pane.follow || !strings.Contains(h.m.View(), "line 100") {
		t.Fatalf("G did not follow again:\n%s", h.m.View())
	}

	// A warning from one container shows in the stream; an error ends it.
	h.update(logsMsg{id: h.m.pane.id, events: []logEvent{{warn: "debug/main: unexpected EOF"}, {err: errNoCredentials}}, done: true})
	v := h.m.View()
	if !strings.Contains(v, "warning: debug/main: unexpected EOF") || !strings.Contains(v, "token expired") {
		t.Fatalf("warning and error:\n%s", v)
	}
	// Lines for a pane that has closed are dropped.
	h.press("esc")
	if cmd := h.m.logs(logsMsg{id: 99}); cmd != nil || h.m.pane != nil {
		t.Fatal("lines for a closed pane opened or fed one")
	}
}

func TestWhyAndTreePanes(t *testing.T) {
	b := newFakeBackend(shop()...).connectedTo("staging", "shop", false)
	h := newHarness(t, b)
	selectWorkload(t, h, "deploy/billing")

	h.press("w")
	h.waitForView("deploy/billing in shop is not ready")
	v := h.m.View()
	for _, want := range []string{"Why deploy/billing", "billing-7d9f-ccccc", "CrashLoopBackOff", "next:"} {
		if !strings.Contains(v, want) {
			t.Errorf("why pane lacks %q:\n%s", want, v)
		}
	}

	// The workload's other panes open from this one.
	h.press("t")
	h.waitForView("Tree of deploy/billing")
	h.waitForView("pod/billing-7d9f-ccccc")
	v = h.m.View()
	for _, want := range []string{"deploy/billing", "└── pod/billing-7d9f-ccccc", "container main", "✗"} {
		if !strings.Contains(v, want) {
			t.Errorf("tree pane lacks %q:\n%s", want, v)
		}
	}
	h.press("l")
	h.waitForView("billing-7d9f-ccccc/main fake logs")
	h.press("q")
	h.waitForQuit()
	if h.m.pane != nil {
		t.Error("quitting left the pane and its stream open")
	}
}

func TestPaneShowsWhyItFailed(t *testing.T) {
	b := newFakeBackend(shop()...).connectedTo("staging", "shop", false)
	h := newHarness(t, b)
	selectWorkload(t, h, "deploy/checkout")
	if err := b.cs.AppsV1().Deployments("shop").Delete(t.Context(), "checkout", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	h.press("w")
	h.waitForView(`deployments.apps "checkout" not found`)
}

func TestClean(t *testing.T) {
	tests := []struct{ in, want string }{
		{"plain", "plain"},
		{"\x1b[31mred\x1b[0m", "red"},
		{"a\tb", "a    b"},
		{"\x1b]0;title\x07after", "after"},
		{"cr\r\x1b[2J\x1b[Hgone", "crgone"},
	}
	for _, tt := range tests {
		if got := clean(tt.in); got != tt.want {
			t.Errorf("clean(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
