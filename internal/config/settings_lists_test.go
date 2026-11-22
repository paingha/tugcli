package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestListAndMapSettings(t *testing.T) {
	tests := []struct {
		key, kind, elem string
		fields          []string
	}{
		{key: "guard.readOnlyLabels", kind: KindLabels},
		{key: "plugins.allowOverride", kind: KindList},
		{key: "plugins.sandbox.exclude", kind: KindList},
		{key: "oidc.listenAddresses", kind: KindList},
		{key: "policy.default", kind: KindList},
		{key: "plugins.indexes", kind: KindEntries, fields: []string{"name", "url", "publicKey"}},
		{key: "intelligence.prices", kind: KindEntries, fields: []string{"model", "input", "output", "cacheRead"}},
		{key: "policy.packs", kind: KindEntries, fields: []string{"path", "url", "sha256"}},
		{key: "plugins.timeouts", kind: KindMap, elem: KindDuration},
		{key: "plugins.pins", kind: KindMap, elem: KindString},
	}
	for _, tt := range tests {
		s, ok := LookupSetting(tt.key)
		if !ok || s.Kind != tt.kind || s.Elem != tt.elem || !reflect.DeepEqual(s.Fields, tt.fields) {
			t.Errorf("LookupSetting(%s) = %+v, %v; want kind %s, elem %q, fields %v", tt.key, s, ok, tt.kind, tt.elem, tt.fields)
		}
	}
	// An entry of a map is a setting of its own, its key in its own case.
	s, ok := LookupSetting("Plugins.Timeouts.checkAccess")
	if !ok || s.Key != "plugins.timeouts.checkAccess" || s.Kind != KindDuration || s.MapKey() != "checkAccess" {
		t.Errorf("LookupSetting(plugins.timeouts.checkAccess) = %+v, %v", s, ok)
	}
	if _, ok := LookupSetting("plugins.timeouts."); ok {
		t.Error("a map with an empty key is a setting")
	}
}

func TestValues(t *testing.T) {
	c := &Config{}
	c.Guard.ReadOnlyLabels = []map[string]string{{"env": "prod"}, {"tier": "critical", "team": "payments"}}
	c.Plugins.Indexes = []PluginIndex{{Name: "acme", URL: "https://x", PublicKey: "k"}}
	c.Plugins.Timeouts = map[string]Duration{"list": {Duration: time.Minute}, "credential": {Duration: 15 * time.Second}}
	c.Intelligence.Prices = []ModelPrice{{Model: "llama4", Input: 0.2}}
	for key, want := range map[string][]string{
		"guard.readOnlyLabels":      {"env=prod", "team=payments tier=critical"},
		"plugins.indexes":           {"name=acme url=https://x publicKey=k"},
		"plugins.timeouts":          {"credential=15s", "list=1m0s"},
		"plugins.timeouts.list":     {"1m0s"},
		"plugins.timeouts.rotate":   nil,
		"intelligence.prices":       {"model=llama4 input=0.2"},
		"plugins.sandbox.exclude":   nil,
		"intelligence.enabled":      {"false"},
		"intelligence.provider":     nil,
		"plugins.pins.digitalocean": nil,
	} {
		s, _ := LookupSetting(key)
		if got := s.Values(c); !reflect.DeepEqual(got, want) {
			t.Errorf("Values(%s) = %q, want %q", key, got, want)
		}
	}
}

func TestEditLists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	read := func() string {
		t.Helper()
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	edit := func(want string, changes ...Change) {
		t.Helper()
		effects, err := EditSettings(dir, changes, nil)
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(effects, " "); got != want {
			t.Errorf("effects of %+v = %s, want %s", changes, got, want)
		}
	}
	change := func(op, key string, item ...string) Change {
		s, ok := LookupSetting(key)
		if !ok {
			t.Fatalf("no setting %s", key)
		}
		if op == OpSet {
			return Change{Setting: s, Op: op, Value: item[0]}
		}
		return Change{Setting: s, Op: op, Item: item}
	}

	edit("added added", change(OpAdd, "plugins.sandbox.exclude", "acme"), change(OpAdd, "guard.readOnlyLabels", "env=prod"))
	edit("unchanged", change(OpAdd, "plugins.sandbox.exclude", "acme"))
	edit("added", change(OpAdd, "guard.readOnlyLabels", "team=payments", "tier=critical"))
	edit("unchanged", change(OpAdd, "guard.readOnlyLabels", "tier=critical", "team=payments")) // the same set, in any order
	edit("added", change(OpAdd, "plugins.indexes", "name=acme", "url=https://plugins.acme.example/index.yaml", "publicKey=k1"))
	edit("replaced", change(OpAdd, "plugins.indexes", "name=acme", "url=https://plugins.acme.example/index.yaml", "publicKey=k2"))
	edit("added", change(OpAdd, "intelligence.prices", "model=llama4", "input=0.2", "output=0.4"))
	edit("set set", change(OpSet, "plugins.timeouts.list", "60s"), change(OpSet, "plugins.pins.digitalocean", "0.4.1"))
	want := `plugins:
  sandbox:
    exclude:
      - acme
  indexes:
    - name: acme
      url: https://plugins.acme.example/index.yaml
      publicKey: k2
  timeouts:
    list: 60s
  pins:
    digitalocean: 0.4.1
guard:
  readOnlyLabels:
    - env: prod
    - team: payments
      tier: critical
intelligence:
  prices:
    - model: llama4
      input: 0.2
      output: 0.4
`
	if got := read(); got != want {
		t.Errorf("config.yaml =\n%s\nwant\n%s", got, want)
	}
	c, err := Load(dir)
	if err != nil || c.Plugins.Timeouts["list"].Duration != time.Minute || c.Intelligence.Prices[0].Output != 0.4 || c.Plugins.Pins["digitalocean"] != "0.4.1" {
		t.Fatalf("Load = %+v, %v", c, err)
	}

	// Remove: by value, by label set, by an entry's name or its fields; a
	// list or map left empty goes.
	edit("unchanged", change(OpRemove, "plugins.indexes", "other"))
	edit("removed removed removed", change(OpRemove, "plugins.sandbox.exclude", "acme"),
		change(OpRemove, "guard.readOnlyLabels", "tier=critical", "team=payments"), change(OpRemove, "plugins.indexes", "acme"))
	edit("removed removed removed", change(OpRemove, "intelligence.prices", "model=llama4"),
		change(OpUnset, "plugins.timeouts.list"), change(OpUnset, "plugins.pins.digitalocean"))
	if got, want := read(), "guard:\n  readOnlyLabels:\n    - env: prod\n"; got != want {
		t.Errorf("config.yaml after removing =\n%s\nwant\n%s", got, want)
	}
	edit("removed", change(OpRemove, "guard.readOnlyLabels", "env=prod"))
	if got := read(); got != "{}\n" {
		t.Errorf("config.yaml after emptying the list = %q", got)
	}
	// Nothing to do writes nothing.
	before, _ := os.Stat(path)
	edit("unchanged", change(OpRemove, "guard.readOnlyLabels", "env=prod"))
	if after, _ := os.Stat(path); !after.ModTime().Equal(before.ModTime()) {
		t.Error("a change with no effect wrote config.yaml")
	}
}

func TestEditListsRefuses(t *testing.T) {
	dir := t.TempDir()
	for _, tt := range []struct {
		change Change
		want   string
	}{
		{Change{Op: OpSet, Value: "x"}, "guard.readOnlyLabels in %s: it is a list; add or remove one item at a time"},
		{Change{Op: OpAdd, Item: []string{"env"}}, `guard.readOnlyLabels in %s: a label set takes key=value arguments, not "env"`},
		{Change{Op: OpAdd, Item: []string{"env=prod", "env=dev"}}, "guard.readOnlyLabels in %s: a label set names env twice"},
	} {
		tt.change.Setting, _ = LookupSetting("guard.readOnlyLabels")
		checkRefusal(t, dir, tt.change, tt.want)
	}
	for _, tt := range []struct {
		key    string
		change Change
		want   string
	}{
		{"plugins.indexes", Change{Op: OpAdd, Item: []string{"name=acme", "key=x"}}, "plugins.indexes in %s: an entry has no field key; its fields are name, url, publicKey"},
		{"intelligence.prices", Change{Op: OpAdd, Item: []string{"model=x", "input=cheap"}}, `intelligence.prices in %s: input takes a number, not "cheap"`},
		{"plugins.sandbox.exclude", Change{Op: OpAdd, Item: []string{"a", "b"}}, "plugins.sandbox.exclude in %s: give one item"},
		{"plugins.timeouts", Change{Op: OpSet, Value: "1m"}, "plugins.timeouts in %s: it is a map; set one of its entries, as plugins.timeouts.<key>"},
		{"plugins.timeouts.list", Change{Op: OpAdd, Item: []string{"x"}}, "plugins.timeouts.list in %s: it is not a list; use set or unset"},
		{"plugins.timeouts.list", Change{Op: OpSet, Value: "soon"}, `plugins.timeouts.list in %s: plugins.timeouts.list takes a duration such as "30s" or "5m", not "soon"`},
	} {
		tt.change.Setting, _ = LookupSetting(tt.key)
		checkRefusal(t, dir, tt.change, tt.want)
	}
}

func checkRefusal(t *testing.T, dir string, c Change, want string) {
	t.Helper()
	want = strings.ReplaceAll(want, "%s", filepath.Join(dir, "config.yaml"))
	if _, err := EditSettings(dir, []Change{c}, nil); err == nil || err.Error() != want {
		t.Errorf("EditSettings(%+v) = %v, want %q", c, err, want)
	}
	if _, err := os.Stat(filepath.Join(dir, "config.yaml")); !os.IsNotExist(err) {
		t.Errorf("a refused change wrote config.yaml: %v", err)
	}
}
