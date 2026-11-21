package tui

import (
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/paingha/tugcli/internal/ops"
	"github.com/paingha/tugcli/internal/resolve"
)

func pickObjects() []ops.ObjectState {
	obj := func(kind, ns, name, status, owner string, h ops.Health) ops.ObjectState {
		return ops.ObjectState{
			Object: resolve.Object{Kind: kind, Name: name, Namespace: ns},
			Health: h, Status: status, Owner: owner, Created: pickNow.Add(-3 * time.Hour),
		}
	}
	return []ops.ObjectState{
		obj("Deployment", "shop", "api-server", "1/1 ready", "", ops.HealthReady),
		obj("Deployment", "shop", "billing", "0/1 ready", "", ops.HealthNotReady),
		obj("Pod", "shop", "api-server-7d9f-x2x4z", "Running", "rs/api-server-7d9f", ops.HealthReady),
		obj("Pod", "shop", "billing-5d4f-x2x4z", "CrashLoopBackOff", "rs/billing-5d4f", ops.HealthNotReady),
		obj("Pod", "payments", "rapid-importer", "Completed", "job/rapid-importer", ops.HealthReady),
		obj("ConfigMap", "shop", "api-config", "", "", ops.HealthNone),
	}
}

var pickNow = time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)

func TestFilterObjects(t *testing.T) {
	objects := pickObjects()
	names := func(idx []int) []string {
		out := make([]string, len(idx))
		for i, j := range idx {
			out[i] = ObjectLabel(objects[j])
		}
		return out
	}
	tests := []struct {
		query string
		want  []string
	}{
		{"", []string{"deploy/api-server", "deploy/billing", "pod/api-server-7d9f-x2x4z", "pod/billing-5d4f-x2x4z", "pod/rapid-importer", "cm/api-config"}},
		// Prefixes of the name beat fuzzy matches such as "rapid-importer".
		{"api", []string{"deploy/api-server", "pod/api-server-7d9f-x2x4z", "cm/api-config", "pod/rapid-importer"}},
		{"api-server", []string{"deploy/api-server", "pod/api-server-7d9f-x2x4z"}},
		{"deploy/", []string{"deploy/api-server", "deploy/billing"}},
		// Words narrow: each must match the name or another column.
		{"billing crash", []string{"pod/billing-5d4f-x2x4z"}},
		{"payments", []string{"pod/rapid-importer"}},
		{"rs/billing", []string{"pod/billing-5d4f-x2x4z"}},
		{"API CONFIG", []string{"cm/api-config"}},
		{"nothing-like-this", []string{}},
	}
	for _, tt := range tests {
		if got := names(FilterObjects(objects, tt.query)); !slices.Equal(got, tt.want) {
			t.Errorf("FilterObjects(%q) = %v, want %v", tt.query, got, tt.want)
		}
	}
}

var pickActions = []PickAction{{"w", "why"}, {"t", "tree"}, {"l", "logs"}}

func pressAll(m *pickModel, keys ...string) tea.Cmd {
	var cmd tea.Cmd
	for _, k := range keys {
		_, cmd = m.Update(keyMsg(k))
	}
	return cmd
}

func typeInto(m *pickModel, s string) {
	for _, r := range s {
		m.Update(keyMsg(string(r)))
	}
}

func TestPickModelChoosesObjectThenAction(t *testing.T) {
	m := newPickModel(PickOptions{Cluster: "payments-prod", Namespace: "shop", Objects: pickObjects(), Actions: pickActions, Now: func() time.Time { return pickNow }}, newStyles())
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 12})

	typeInto(m, "billing")
	if len(m.shown) != 2 {
		t.Fatalf("after typing billing, %d rows shown, want 2", len(m.shown))
	}
	screen := plain(m.screen())
	for _, want := range []string{"payments-prod", "namespace shop", "2 of 6", "Filter: billing", "KIND/NAME", "OWNER", "rs/billing-5d4f", "CrashLoopBackOff", "3h"} {
		if !strings.Contains(screen, want) {
			t.Errorf("screen lacks %q:\n%s", want, screen)
		}
	}

	pressAll(m, "down", "enter")
	if !m.menu || m.picked != nil {
		t.Fatalf("enter should open the action menu, not choose: menu=%v picked=%v", m.menu, m.picked)
	}
	if help := plain(m.help()); !strings.Contains(help, "pod/billing-5d4f-x2x4z") || !strings.Contains(help, "l logs") {
		t.Errorf("menu help = %q", help)
	}
	// esc goes back to the list with the filter kept.
	pressAll(m, "esc")
	if m.menu || m.filter != "billing" {
		t.Errorf("esc in the menu: menu=%v filter=%q", m.menu, m.filter)
	}
	if cmd := pressAll(m, "enter", "l"); cmd == nil || m.picked == nil {
		t.Fatal("l in the menu did not choose logs")
	}
	if got := ObjectLabel(m.picked.Object); got != "pod/billing-5d4f-x2x4z" || m.picked.Action != "logs" {
		t.Errorf("picked %s, %s; want pod/billing-5d4f-x2x4z, logs", got, m.picked.Action)
	}
}

func TestPickModelOneActionAndCancel(t *testing.T) {
	m := newPickModel(PickOptions{Objects: pickObjects(), Actions: pickActions[:1], Query: "api-config"}, newStyles())
	if !strings.Contains(plain(m.screen()), "every namespace") {
		t.Errorf("a picker over every namespace should say so:\n%s", m.screen())
	}
	pressAll(m, "enter")
	if m.picked == nil || m.picked.Action != "why" || m.picked.Object.Object.Name != "api-config" {
		t.Fatalf("with one action, enter should choose it: %+v", m.picked)
	}

	m = newPickModel(PickOptions{Objects: pickObjects(), Actions: pickActions}, newStyles())
	typeInto(m, "zzz")
	pressAll(m, "enter") // nothing to choose
	if m.menu || m.picked != nil {
		t.Fatal("enter with nothing shown chose something")
	}
	if !strings.Contains(plain(m.screen()), "Nothing matches") {
		t.Errorf("empty list:\n%s", m.screen())
	}
	if cmd := pressAll(m, "esc"); cmd != nil || m.filter != "" {
		t.Errorf("first esc should clear the filter, not quit")
	}
	if cmd := pressAll(m, "esc"); cmd == nil || m.picked != nil {
		t.Errorf("esc with no filter should quit without choosing")
	}
}
