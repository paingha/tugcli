package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func readConfig(t *testing.T, home string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(home, "config", "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestSettingsCommands(t *testing.T) {
	home := isolate(t)
	writeConfig(t, home, "# my settings\nkubectl: kubectl # the one on PATH\n")

	if out := mustTug(t, "settings", "path"); out != filepath.Join(home, "config", "config.yaml")+"\n" {
		t.Errorf("settings path = %q", out)
	}

	out := mustTug(t, "settings", "set", "intelligence.monthlySpendCapUSD", "20")
	if !strings.HasPrefix(out, "Set intelligence.monthlySpendCapUSD to 20 in ") {
		t.Errorf("settings set = %q", out)
	}
	mustTug(t, "settings", "set", "Intelligence.Model", "claude-sonnet-5-5") // keys in any case
	if got, want := readConfig(t, home), "# my settings\nkubectl: kubectl # the one on PATH\nintelligence:\n  monthlySpendCapUSD: 20\n  model: claude-sonnet-5-5\n"; got != want {
		t.Errorf("config.yaml =\n%s\nwant\n%s", got, want)
	}
	if out := mustTug(t, "settings", "get", "intelligence.model"); out != "claude-sonnet-5-5\n" {
		t.Errorf("settings get = %q", out)
	}
	// get gives the value tug uses, a default included.
	if out := mustTug(t, "settings", "get", "intelligence.provider"); out != "anthropic\n" {
		t.Errorf("settings get of a default = %q", out)
	}

	var views []settingView
	if err := json.Unmarshal([]byte(mustTug(t, "settings", "list", "-o", "json")), &views); err != nil {
		t.Fatal(err)
	}
	got := map[string]settingView{}
	for _, v := range views {
		got[v.Key] = v
	}
	for key, want := range map[string]settingView{
		"keyring.backend":                 {Kind: "string", Value: "file", Source: "TUG_KEYRING_BACKEND"},
		"kubectl":                         {Kind: "string", Value: "kubectl", Source: "config.yaml"},
		"plugins.sandbox.enabled":         {Kind: "bool", Value: "true", Source: "default"},
		"intelligence.enabled":            {Kind: "bool", Value: "false"},
		"intelligence.provider":           {Kind: "string", Value: "anthropic", Source: "default"},
		"intelligence.model":              {Kind: "string", Value: "claude-sonnet-5-5", Source: "config.yaml"},
		"intelligence.monthlySpendCapUSD": {Kind: "number", Value: "20", Source: "config.yaml"},
	} {
		want.Key = key
		if !reflect.DeepEqual(got[key], want) {
			t.Errorf("settings list %s = %+v, want %+v", key, got[key], want)
		}
	}
	if table := mustTug(t, "settings", "list"); !strings.Contains(squash(table), "SETTING VALUE FROM") || !strings.Contains(squash(table), "intelligence.model claude-sonnet-5-5 config.yaml") {
		t.Errorf("settings list:\n%s", table)
	}

	if out := mustTug(t, "settings", "unset", "intelligence.model"); !strings.HasPrefix(out, "Removed intelligence.model from ") {
		t.Errorf("settings unset = %q", out)
	}
	if out := mustTug(t, "settings", "unset", "intelligence.model"); !strings.HasPrefix(out, "intelligence.model was not set in ") {
		t.Errorf("settings unset of an unset setting = %q", out)
	}

	// A value tug could not use is refused, and config.yaml is left as it
	// was.
	before := readConfig(t, home)
	for _, tt := range []struct {
		args []string
		want string
	}{
		{[]string{"intelligence.provider", "gemini"}, `intelligence.provider: "gemini" is not a provider; use one of anthropic, openai, mistral, custom`},
		{[]string{"intelligence.baseURL", "http://llm.example.com"}, "must use https, so the API key is not sent in clear text"},
		{[]string{"intelligence.effort", "huge"}, `intelligence.effort "huge" is not one of low, medium, high, xhigh, max`},
		{[]string{"intelligence.enabled", "maybe"}, `intelligence.enabled takes true or false, not "maybe"`},
		{[]string{"keyring.backend", "vault"}, `keyring.backend "vault" is not one of auto, keychain, file`},
		{[]string{"intelligence.apiKey", "sk-ant-not-in-config"}, "secrets never go in config.yaml; store a model provider's API key with `tug intelligence key set <provider>`"},
		{[]string{"plugins.bogus", "x"}, `"plugins.bogus" is not a setting tug settings can change; see ` + "`tug settings list`"},
	} {
		r := tug(t, append([]string{"settings", "set"}, tt.args...)...)
		if r.code != 1 || !strings.Contains(r.stderr, tt.want) {
			t.Errorf("settings set %v = %+v, want an error with %q", tt.args, r, tt.want)
		}
	}
	if after := readConfig(t, home); after != before || strings.Contains(after, "sk-ant") {
		t.Errorf("refused changes wrote config.yaml:\n%s", after)
	}

	// A variable that overrides a setting is pointed out.
	t.Setenv("TUG_KUBECTL", "/opt/kubectl")
	if r := tug(t, "settings", "set", "kubectl", "oc"); r.code != 0 || !strings.Contains(r.stderr, "warning: TUG_KUBECTL is set, and overrides kubectl") {
		t.Errorf("settings set with an override: %+v", r)
	}
}
