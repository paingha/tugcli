package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/paingha/tugcli/internal/plugin"
)

func TestSettingsReadOnlyLabels(t *testing.T) {
	home := isolate(t)

	// The first label set keeps env=prod, which tug's default holds and a
	// list of the user's own would otherwise drop.
	out := mustTug(t, "settings", "add", "guard.readOnlyLabels", "team=payments", "tier=critical")
	for _, want := range []string{
		"Kept env=prod in guard.readOnlyLabels: tug's default, which a list of your own replaces.\n",
		"Added team=payments tier=critical to guard.readOnlyLabels in ",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("settings add lacks %q:\n%s", want, out)
		}
	}
	if got, want := readConfig(t, home), "guard:\n  readOnlyLabels:\n    - env: prod\n    - team: payments\n      tier: critical\n"; got != want {
		t.Errorf("config.yaml =\n%s\nwant\n%s", got, want)
	}
	if out := mustTug(t, "settings", "get", "guard.readOnlyLabels"); out != "env=prod\nteam=payments tier=critical\n" {
		t.Errorf("settings get = %q", out)
	}

	// Removing protection is shown and, without a terminal, needs --yes.
	before := readConfig(t, home)
	r := tug(t, "settings", "remove", "guard.readOnlyLabels", "env=prod")
	if r.code != 1 || !strings.Contains(r.stderr, "This change:\n  - stops opening clusters labelled env=prod read-only\n") ||
		!strings.Contains(r.stderr, "tug: this change trusts something or protects less; pass --yes to make it without a terminal") {
		t.Errorf("remove without --yes: %+v", r)
	}
	if readConfig(t, home) != before {
		t.Error("a refused remove wrote config.yaml")
	}
	if r := tug(t, "settings", "unset", "guard.readOnlyLabels"); r.code != 1 || !strings.Contains(r.stderr, "stops opening clusters labelled team=payments,tier=critical read-only") {
		t.Errorf("unset of the list without --yes: %+v", r)
	}
	mustTug(t, "settings", "remove", "guard.readOnlyLabels", "env=prod", "--yes")
	if got := readConfig(t, home); strings.Contains(got, "env: prod") || !strings.Contains(got, "team: payments") {
		t.Errorf("config.yaml after removing env=prod =\n%s", got)
	}
	if out := mustTug(t, "settings", "remove", "guard.readOnlyLabels", "env=dev"); !strings.Contains(out, "has no env=dev") {
		t.Errorf("remove of a missing label set = %q", out)
	}
}

func TestSettingsTrustAndSandbox(t *testing.T) {
	home := isolate(t)
	_, key, err := plugin.NewIndexKey()
	if err != nil {
		t.Fatal(err)
	}

	for _, tt := range []struct {
		args   []string
		effect string
	}{
		{[]string{"add", "plugins.indexes", "name=acme", "url=https://plugins.acme.example/index.yaml", "publicKey=" + key},
			"trusts the plugin index acme at https://plugins.acme.example/index.yaml, signed by the key " + key[:12] + "…: tug plugin install installs its plugins by name"},
		{[]string{"add", "plugins.sandbox.exclude", "acme"}, "runs the acme plugin without the sandbox"},
		{[]string{"set", "plugins.sandbox.enabled", "false"}, "runs every plugin without the sandbox"},
		{[]string{"add", "plugins.allowOverride", "aws"}, "lets a plugin replace the built-in aws provider"},
	} {
		args := append([]string{"settings"}, tt.args...)
		r := tug(t, args...)
		if r.code != 1 || !strings.Contains(r.stderr, "  - "+tt.effect+"\n") || !strings.Contains(r.stderr, "pass --yes") {
			t.Errorf("tug %v without --yes: %+v", args, r)
		}
		if r := tug(t, append(args, "--yes")...); r.code != 0 || !strings.Contains(r.stderr, tt.effect) {
			t.Errorf("tug %v --yes: %+v", args, r)
		}
	}
	// A setting that already holds changes nothing, and asks nothing.
	if r := tug(t, "settings", "add", "plugins.indexes", "name=acme", "url=https://plugins.acme.example/index.yaml", "publicKey="+key); r.code != 0 || r.stderr != "" || !strings.Contains(r.stdout, "plugins.indexes already has") {
		t.Errorf("adding the same index again: %+v", r)
	}
	// Turning signatures off once they are required asks too.
	mustTug(t, "settings", "set", "plugins.requireSignatures", "true")
	if r := tug(t, "settings", "set", "plugins.requireSignatures", "false"); r.code != 1 || !strings.Contains(r.stderr, "lets tug plugin install install plugins that have no Sigstore signature") {
		t.Errorf("turning signatures off: %+v", r)
	}
	got := readConfig(t, home)
	for _, want := range []string{"name: acme", "publicKey: " + key, "- acme", "enabled: false", "- aws", "requireSignatures: true"} {
		if !strings.Contains(got, want) {
			t.Errorf("config.yaml lacks %q:\n%s", want, got)
		}
	}
	// Each trusted index's key must be one tug can use.
	if r := tug(t, "settings", "add", "plugins.indexes", "name=other", "url=https://x.example/i.yaml", "publicKey=nope", "--yes"); r.code != 1 ||
		!strings.Contains(r.stderr, "plugins.indexes other: the index public key must be a base64 ed25519 key") {
		t.Errorf("an index with a bad key: %+v", r)
	}
	if out := mustTug(t, "settings", "remove", "plugins.indexes", "acme"); !strings.HasPrefix(out, "Removed acme from plugins.indexes in ") {
		t.Errorf("remove an index = %q", out)
	}
}

func TestSettingsEntriesAndMaps(t *testing.T) {
	home := isolate(t)
	sha := strings.Repeat("ab", 32)

	mustTug(t, "settings", "add", "intelligence.prices", "model=llama4", "input=0", "output=0")
	if out := mustTug(t, "settings", "add", "intelligence.prices", "model=llama4", "input=0.1", "output=0.2"); !strings.HasPrefix(out, "Replaced the entry in intelligence.prices with model=llama4 input=0.1 output=0.2") {
		t.Errorf("replacing a price = %q", out)
	}
	mustTug(t, "settings", "add", "policy.packs", "url=https://policy.acme.example/pack.yaml", "sha256="+sha)
	mustTug(t, "settings", "add", "policy.default", "acme")
	mustTug(t, "settings", "set", "plugins.timeouts.list", "60s")
	mustTug(t, "settings", "set", "plugins.pins.digitalocean", "0.4.1")
	mustTug(t, "settings", "add", "oidc.listenAddresses", "127.0.0.1:9000")
	want := `intelligence:
  prices:
    - model: llama4
      input: 0.1
      output: 0.2
policy:
  packs:
    - url: https://policy.acme.example/pack.yaml
      sha256: ` + sha + `
  default:
    - acme
plugins:
  timeouts:
    list: 60s
  pins:
    digitalocean: 0.4.1
oidc:
  listenAddresses:
    - 127.0.0.1:9000
`
	if got := readConfig(t, home); got != want {
		t.Errorf("config.yaml =\n%s\nwant\n%s", got, want)
	}
	// The price applies.
	var st intelligenceStatus
	mustTug(t, "settings", "set", "intelligence.model", "llama4:70b")
	if err := json.Unmarshal([]byte(mustTug(t, "intelligence", "status", "-o", "json")), &st); err != nil || st.Price == nil || st.Price.Output != 0.2 {
		t.Errorf("status after adding a price = %+v, %v", st, err)
	}

	var views []settingView
	if err := json.Unmarshal([]byte(mustTug(t, "settings", "list", "-o", "json")), &views); err != nil {
		t.Fatal(err)
	}
	for _, v := range views {
		switch v.Key {
		case "plugins.timeouts":
			if strings.Join(v.Values, " ") != "list=1m0s" || v.Source != "config.yaml" {
				t.Errorf("plugins.timeouts = %+v", v)
			}
		case "guard.readOnlyLabels":
			if strings.Join(v.Values, " ") != "env=prod" || v.Source != "default" {
				t.Errorf("guard.readOnlyLabels = %+v", v)
			}
		}
	}

	mustTug(t, "settings", "unset", "plugins.pins.digitalocean")
	mustTug(t, "settings", "remove", "intelligence.prices", "llama4")
	if got := readConfig(t, home); strings.Contains(got, "pins") || strings.Contains(got, "prices") {
		t.Errorf("config.yaml after unpinning and removing the price =\n%s", got)
	}

	before := readConfig(t, home)
	for _, tt := range []struct {
		args []string
		want string
	}{
		{[]string{"add", "policy.packs", "url=http://policy.example/p.yaml", "sha256=" + sha}, "policy.packs: refusing http://policy.example/p.yaml: packs are downloaded over https only"},
		{[]string{"add", "policy.packs", "url=https://policy.example/p.yaml"}, "policy.packs: https://policy.example/p.yaml needs a sha256"},
		{[]string{"set", "plugins.timeouts.serve", "10s"}, "plugins.timeouts.serve is not an operation; use describe, accounts, list, credential, rotate, checkAccess"},
		{[]string{"add", "oidc.listenAddresses", "10.0.0.1:8000"}, `oidc.listenAddresses "10.0.0.1:8000" must be a loopback address and port`},
		{[]string{"set", "guard.readOnlyLabels", "env=prod"}, "guard.readOnlyLabels is a list; change it with `tug settings add` and `tug settings remove`"},
		{[]string{"add", "intelligence.model", "x"}, "intelligence.model is not a list; change it with `tug settings set`"},
		{[]string{"add", "plugins.sandbox.exclude", "a", "b"}, "plugins.sandbox.exclude takes one item at a time"},
		{[]string{"set", "teams", "x"}, "team catalogs are followed with `tug team add` and `tug team remove`"},
	} {
		r := tug(t, append([]string{"settings"}, tt.args...)...)
		if r.code != 1 || !strings.Contains(r.stderr, tt.want) {
			t.Errorf("settings %v = %+v, want an error with %q", tt.args, r, tt.want)
		}
	}
	if readConfig(t, home) != before {
		t.Error("a refused change wrote config.yaml")
	}
}
