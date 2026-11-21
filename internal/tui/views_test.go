package tui

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func shownClusters(m *model) []string {
	var names []string
	for _, i := range m.clusters.shown {
		names = append(names, m.clusters.all[i].Name)
	}
	return names
}

func TestStartsWithClustersWhenNotConnected(t *testing.T) {
	h := newHarness(t, newFakeBackend())
	h.waitFor("the catalog", func(m *model) bool { return m.clusters.loaded })
	if h.m.view != viewClusters {
		t.Fatalf("view = %v, want the clusters view", h.m.view)
	}
	v := h.screen()
	for _, want := range []string{"Clusters", "prod-eu", "staging", "gpu-us", "stale", "not connected", "team=payments"} {
		if !strings.Contains(v, want) {
			t.Errorf("View() lacks %q:\n%s", want, v)
		}
	}
}

func TestClustersFilterByTyping(t *testing.T) {
	tests := []struct {
		typed string
		want  []string
	}{
		{"", []string{"prod-eu", "staging", "gpu-us"}},
		{"stag", []string{"staging"}},
		{"eu", []string{"prod-eu", "staging"}}, // a name first, then a region label
		{"env=prod", []string{"prod-eu"}},
		{"env=staging eu", []string{"staging"}},
		{"payments", []string{"prod-eu"}}, // a label value
		{"zzz", nil},
	}
	for _, tt := range tests {
		t.Run(tt.typed, func(t *testing.T) {
			h := newHarness(t, newFakeBackend())
			h.waitFor("the catalog", func(m *model) bool { return m.clusters.loaded })
			h.typeText(tt.typed)
			if got := shownClusters(h.m); !slices.Equal(got, tt.want) {
				t.Errorf("after typing %q, shown = %v, want %v", tt.typed, got, tt.want)
			}
		})
	}
}

func TestClustersQuitHelpAndSlash(t *testing.T) {
	h := newHarness(t, newFakeBackend())
	h.waitFor("the catalog", func(m *model) bool { return m.clusters.loaded })

	h.press("?")
	if !h.m.showHelp || !strings.Contains(h.screen(), "filter: names, label values, label=value") {
		t.Fatalf("? did not show help:\n%s", h.screen())
	}
	h.press("x") // any key closes help
	if h.m.showHelp || h.m.clusters.filter != "" {
		t.Fatalf("after help, showHelp = %v and filter = %q", h.m.showHelp, h.m.clusters.filter)
	}

	// After /, q types instead of quitting.
	h.press("/")
	h.typeText("q")
	h.settle()
	if h.quit || h.m.clusters.filter != "q" {
		t.Fatalf("/q: quit = %v, filter = %q", h.quit, h.m.clusters.filter)
	}
	h.press("esc")
	if h.m.clusters.filter != "" || len(h.m.clusters.shown) != 3 {
		t.Fatalf("esc left filter %q and %d clusters", h.m.clusters.filter, len(h.m.clusters.shown))
	}
	h.press("q")
	h.waitForQuit()
}

func TestConnectFromClusters(t *testing.T) {
	b := newFakeBackend(shop()...)
	h := newHarness(t, b)
	h.waitFor("the catalog", func(m *model) bool { return m.clusters.loaded })
	h.typeText("stag")
	h.press("enter")
	h.waitFor("the workloads", func(m *model) bool { return m.view == viewWorkloads && m.workloads.loaded })

	if connects, _, _ := b.calls(); !slices.Equal(connects, []string{"kubeconfig/work/staging"}) {
		t.Errorf("Connect calls = %v, want the staging cluster", connects)
	}
	v := h.screen()
	for _, want := range []string{"Workloads in shop", "staging", "namespace shop", "read-write", "direct", "Connected to staging, namespace shop, read-write"} {
		if !strings.Contains(v, want) {
			t.Errorf("View() lacks %q:\n%s", want, v)
		}
	}
}

func TestConnectToTunnelledStaleCluster(t *testing.T) {
	b := newFakeBackend()
	h := newHarness(t, b)
	h.waitFor("the catalog", func(m *model) bool { return m.clusters.loaded })
	h.typeText("gpu")
	h.press("enter")
	h.waitFor("the connection", func(m *model) bool { return m.state.Connected() })
	v := h.screen()
	for _, want := range []string{"tunnel ssh://alice:xxxxx@bastion.example.com via tugd", "not listed by its provider at the last sync"} {
		if !strings.Contains(v, want) {
			t.Errorf("View() lacks %q:\n%s", want, v)
		}
	}
	if strings.Contains(v, "hunter2") {
		t.Errorf("View() shows the tunnel's password:\n%s", v)
	}
	// esc goes back to the clusters view, and from there to the workloads.
	h.press("c")
	if h.m.view != viewClusters {
		t.Fatalf("c: view = %v, want clusters", h.m.view)
	}
	h.press("esc")
	if h.m.view != viewWorkloads {
		t.Fatalf("esc in clusters: view = %v, want workloads", h.m.view)
	}
}

func TestStatusBarModes(t *testing.T) {
	now := time.Date(2026, 11, 20, 14, 0, 0, 0, time.Local)
	tests := []struct {
		name     string
		readOnly bool
		unlocked time.Time
		want     string
	}{
		{"read-write", false, time.Time{}, "read-write"},
		{"read-only", true, time.Time{}, "read-only"},
		{"unlocked", true, now.Add(15 * time.Minute), "unlocked until 2:15PM"},
		{"unlock expired", true, now.Add(-time.Minute), "read-only"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := newFakeBackend().connectedTo("prod-eu", "web", tt.readOnly)
			b.state.UnlockedUntil = tt.unlocked
			h := newHarness(t, b, WithNow(func() time.Time { return now }))
			if bar := plain(h.m.statusBar()); !strings.Contains(bar, tt.want) || !strings.Contains(bar, "prod-eu") || !strings.Contains(bar, "namespace web") {
				t.Errorf("statusBar() = %q, want %q", bar, tt.want)
			}
		})
	}
}

func TestWorkloadsView(t *testing.T) {
	b := newFakeBackend(shop()...).connectedTo("staging", "shop", false)
	h := newHarness(t, b)
	h.waitFor("the workloads", func(m *model) bool { return m.workloads.loaded })
	v := h.screen()
	for _, want := range []string{"deploy/billing", "0/1", "CrashLoopBackOff", "deploy/checkout", "2/2", "Ready", "pod/debug", "3 workloads on staging"} {
		if !strings.Contains(v, want) {
			t.Errorf("View() lacks %q:\n%s", want, v)
		}
	}
	if w := h.m.workloads.selected(); w == nil || w.Target.String() != "deploy/billing" {
		t.Fatalf("selected = %v, want deploy/billing first", w)
	}
	h.press("down")
	if w := h.m.workloads.selected(); w.Target.String() != "deploy/checkout" {
		t.Fatalf("after down, selected = %s", w.Target)
	}

	// Filtering by name keeps the selection where it can.
	h.press("/")
	h.typeText("chk")
	h.press("enter")
	if n := len(h.m.workloads.shown); n != 1 || h.m.workloads.selected().Target.Name != "checkout" {
		t.Fatalf("filter chk shows %d, selected %v", n, h.m.workloads.selected())
	}
	h.press("esc")
	if n := len(h.m.workloads.shown); n != 3 {
		t.Fatalf("esc left %d workloads shown", n)
	}
	if h.m.workloads.selected().Target.Name != "checkout" {
		t.Fatalf("clearing the filter moved the selection to %s", h.m.workloads.selected().Target)
	}
}

func TestWorkloadsRefresh(t *testing.T) {
	b := newFakeBackend(shop()...).connectedTo("staging", "shop", false)
	h := newHarness(t, b, WithRefresh(time.Millisecond))
	h.waitFor("the workloads", func(m *model) bool { return m.workloads.loaded })

	one := int32(1)
	_, err := b.cs.AppsV1().Deployments("shop").Create(context.Background(), &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "pricing", Namespace: "shop"},
		Spec:       appsv1.DeploymentSpec{Replicas: &one, Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "pricing"}}},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	h.waitForView("deploy/pricing")

	// One client serves every reload.
	if _, _, kube := b.calls(); !slices.Equal(kube, []bool{false}) {
		t.Errorf("Kube calls = %v, want one read client", kube)
	}
}

// Reloads that bring fewer items keep the selection when they can.
func TestReloadsThatShrinkTheLists(t *testing.T) {
	b := newFakeBackend(shop()...).connectedTo("staging", "shop", false)
	h := newHarness(t, b)
	h.waitFor("the workloads", func(m *model) bool { return len(m.workloads.items) == 3 })
	h.press("end")
	if w := h.m.workloads.selected(); w.Target.String() != "pod/debug" {
		t.Fatalf("end selected %s", w.Target)
	}
	for _, name := range []string{"billing", "checkout"} {
		if err := b.cs.AppsV1().Deployments("shop").Delete(t.Context(), name, metav1.DeleteOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	h.press("ctrl+r")
	h.waitFor("the smaller list", func(m *model) bool { return !m.workloads.loading && len(m.workloads.items) == 1 })
	if w := h.m.workloads.selected(); w == nil || w.Target.String() != "pod/debug" {
		t.Fatalf("after the reload, selected = %v, want pod/debug still", w)
	}

	h.press("c")
	h.waitFor("the catalog", func(m *model) bool { return m.clusters.loaded })
	h.press("end")
	h.update(catalogMsg{clusters: testClusters[:1]})
	if got := shownClusters(h.m); !slices.Equal(got, []string{"prod-eu"}) || h.m.clusters.selected().Name != "prod-eu" {
		t.Fatalf("after the catalog shrank, shown = %v, selected %v", got, h.m.clusters.selected())
	}
}

func TestTickerStopsWithItsView(t *testing.T) {
	b := newFakeBackend(shop()...).connectedTo("staging", "shop", false)
	h := newHarness(t, b, WithRefresh(time.Hour))
	h.waitFor("the workloads", func(m *model) bool { return m.workloads.loaded })

	tick := h.m.nextTick()
	gen := h.m.ticker.gen
	returned := make(chan any, 1)
	go func() { returned <- tick() }()

	h.press("c") // the clusters view has no ticker
	select {
	case msg := <-returned:
		if msg != nil {
			t.Fatalf("the stopped ticker sent %#v", msg)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the ticker's timer outlived its view")
	}
	// A tick from the ended run changes nothing.
	if cmd := h.m.tick(tickMsg{gen: gen}); cmd != nil {
		t.Error("a tick from a stopped ticker started work")
	}
}

func TestNamespacePicker(t *testing.T) {
	b := newFakeBackend(shop()...).connectedTo("staging", "default", false)
	h := newHarness(t, b)
	h.waitFor("the workloads", func(m *model) bool { return m.workloads.loaded })
	if !strings.Contains(h.screen(), "Nothing runs in namespace default") {
		t.Fatalf("empty namespace:\n%s", h.screen())
	}

	h.press("n")
	h.waitFor("the namespaces", func(m *model) bool { return m.picker != nil && m.picker.loaded })
	if !strings.Contains(h.screen(), "default  (current)") {
		t.Errorf("the picker does not mark the current namespace:\n%s", h.screen())
	}
	h.typeText("sh")
	if !slices.Equal(h.m.picker.shown, []string{"shop"}) {
		t.Fatalf("typing sh shows %v", h.m.picker.shown)
	}
	h.press("enter")
	h.waitFor("the shop workloads", func(m *model) bool { return m.state.Namespace == "shop" && m.workloads.loaded })
	if _, setNS, _ := b.calls(); !slices.Equal(setNS, []string{"shop"}) {
		t.Errorf("SetNamespace calls = %v", setNS)
	}
	if v := h.screen(); !strings.Contains(v, "deploy/checkout") || !strings.Contains(v, "namespace shop") {
		t.Errorf("after switching namespace:\n%s", v)
	}
}

func TestNamespacePickerTrustsATypedName(t *testing.T) {
	b := newFakeBackend().connectedTo("staging", "default", false)
	b.nsErr = errNoCredentials
	h := newHarness(t, b)
	h.press("n")
	h.waitForView("use it anyway")
	h.typeText("payments")
	h.press("enter")
	h.waitFor("the namespace", func(m *model) bool { return m.state.Namespace == "payments" })
}

func TestTableLayout(t *testing.T) {
	tb := table{
		columns: []column{{"NAME", 8}, {"STATUS", 5}, {"LABELS", 0}},
		rows:    [][]string{{"a-very-long-name", "stale", "team=payments,gpu=true"}},
	}
	ws := tb.widths(30)
	if got, want := tb.headingLine(ws), "NAME      STATUS  LABELS      "; got != want {
		t.Errorf("headingLine() = %q, want %q", got, want)
	}
	if got, want := tb.line(ws, 0, tb.rows[0]), "a-very-…  stale   team=paymen…"; got != want {
		t.Errorf("line(0) = %q, want %q", got, want)
	}
}

func TestWindowWithoutASize(t *testing.T) {
	h := newHarness(t, newFakeBackend())
	h.update(tea.WindowSizeMsg{})
	if h.m.width != 80 || h.m.height != 24 || h.screen() == "" {
		t.Fatalf("a window without a size is %dx%d", h.m.width, h.m.height)
	}
}

func TestClustersShowWhyTheCatalogCannotLoad(t *testing.T) {
	b := newFakeBackend()
	b.catalogErr = errors.New("reading catalog.yaml: permission denied")
	h := newHarness(t, b)
	h.waitFor("the error", func(m *model) bool { return m.clusters.err != nil })
	if v := h.screen(); strings.Contains(v, "Loading the catalog") || !strings.Contains(v, "permission denied") {
		t.Fatalf("the clusters view does not say why the catalog is missing:\n%s", v)
	}
}

func TestWorkloadsShowWhyTheyCannotLoad(t *testing.T) {
	b := newFakeBackend().connectedTo("prod-eu", "web", true)
	b.kubeErr = errNoCredentials
	h := newHarness(t, b)
	h.waitForView("token expired")
}
