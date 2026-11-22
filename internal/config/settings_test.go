package config

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func TestSettings(t *testing.T) {
	var keys []string
	for _, s := range Settings() {
		keys = append(keys, s.Key+" "+s.Kind)
	}
	for _, want := range []string{
		"keyring.backend string", "kubectl string", "plugins.sandbox.enabled bool", "plugins.wasm.memoryLimitMiB int",
		"sessions.maxAge duration", "intelligence.enabled bool", "intelligence.monthlySpendCapUSD number",
	} {
		if !slices.Contains(keys, want) {
			t.Errorf("Settings lacks %q: %v", want, keys)
		}
	}
	// Lists and maps are not settings.
	for _, not := range []string{"guard.readOnlyLabels", "plugins.indexes", "plugins.timeouts", "plugins.pins", "intelligence.prices", "teams"} {
		if _, ok := LookupSetting(not); ok {
			t.Errorf("%s is listed as a setting", not)
		}
	}
	s, ok := LookupSetting("Intelligence.MonthlySpendCapUSD")
	if !ok || s.Key != "intelligence.monthlySpendCapUSD" {
		t.Errorf("LookupSetting is not case-insensitive: %+v, %v", s, ok)
	}
	if s, _ := LookupSetting("kubectl"); s.Env != "TUG_KUBECTL" {
		t.Errorf("kubectl's Env = %q", s.Env)
	}
}

func mustSetting(t *testing.T, key string) Setting {
	t.Helper()
	s, ok := LookupSetting(key)
	if !ok {
		t.Fatalf("no setting %s", key)
	}
	return s
}

func TestEditSettings(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	write := func(s string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	read := func() string {
		t.Helper()
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	set := func(key, value string) Change { return Change{Setting: mustSetting(t, key), Value: value} }

	// A new file.
	if _, err := EditSettings(dir, []Change{set("intelligence.enabled", "yes"), set("intelligence.monthlySpendCapUSD", "20")}, nil); err != nil {
		t.Fatal(err)
	}
	if got, want := read(), "intelligence:\n  enabled: true\n  monthlySpendCapUSD: 20\n"; got != want {
		t.Errorf("new file =\n%s\nwant\n%s", got, want)
	}
	if runtime.GOOS != "windows" {
		if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
			t.Errorf("config.yaml mode = %v, want 0600", fi.Mode().Perm())
		}
	}

	// Comments and other settings stay (blank lines between them do not);
	// a value is replaced in place, and a string that reads as a number
	// stays a string.
	write("# my settings\nkubectl: kubectl # the one on PATH\n\nintelligence:\n  # for --deep\n  provider: anthropic # mine\n  enabled: true\n")
	if _, err := EditSettings(dir, []Change{set("intelligence.provider", "openai"), set("intelligence.model", "1.10"), set("sessions.maxAge", "90s")}, nil); err != nil {
		t.Fatal(err)
	}
	want := "# my settings\nkubectl: kubectl # the one on PATH\nintelligence:\n  # for --deep\n  provider: openai # mine\n  enabled: true\n  model: \"1.10\"\nsessions:\n  maxAge: 90s\n"
	if got := read(); got != want {
		t.Errorf("edited file =\n%s\nwant\n%s", got, want)
	}
	c, err := Load(dir)
	if err != nil || c.Intelligence.Model != "1.10" || c.Sessions.MaxAge.String() != "1m30s" || c.Kubectl != "kubectl" {
		t.Errorf("Load after the edit = %+v, %v", c, err)
	}

	// Unset removes a key and a mapping it leaves empty, but not one with a
	// comment of its own.
	removed, err := EditSettings(dir, []Change{
		{Setting: mustSetting(t, "sessions.maxAge"), Unset: true},
		{Setting: mustSetting(t, "intelligence.model"), Unset: true},
		{Setting: mustSetting(t, "intelligence.effort"), Unset: true},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 2 || removed[0].Key != "sessions.maxAge" || removed[1].Key != "intelligence.model" {
		t.Errorf("removed = %v, want sessions.maxAge and intelligence.model", removed)
	}
	if got, want := read(), "# my settings\nkubectl: kubectl # the one on PATH\nintelligence:\n  # for --deep\n  provider: openai # mine\n  enabled: true\n"; got != want {
		t.Errorf("file after unset =\n%s\nwant\n%s", got, want)
	}
	inFile, err := SetInFile(dir)
	if err != nil || !inFile["kubectl"] || !inFile["intelligence.provider"] || inFile["sessions.maxAge"] {
		t.Errorf("SetInFile = %v, %v", inFile, err)
	}

	// A file of comments alone keeps them.
	write("# nothing set yet\n")
	if _, err := EditSettings(dir, []Change{set("kubectl", "/usr/local/bin/kubectl")}, nil); err != nil {
		t.Fatal(err)
	}
	if got := read(); got != "# nothing set yet\nkubectl: /usr/local/bin/kubectl\n" {
		t.Errorf("comments-only file after set =\n%s", got)
	}
}

func TestEditSettingsRefuses(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	const before = "kubectl: kubectl\n"
	if err := os.WriteFile(path, []byte(before), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		change Change
		check  func(*Config) error
		want   string
	}{
		{name: "a bool", change: Change{Setting: mustSetting(t, "intelligence.enabled"), Value: "maybe"}, want: `intelligence.enabled takes true or false, not "maybe"`},
		{name: "an int", change: Change{Setting: mustSetting(t, "intelligence.maxOutputTokens"), Value: "lots"}, want: `intelligence.maxOutputTokens takes a whole number, not "lots"`},
		{name: "a number", change: Change{Setting: mustSetting(t, "intelligence.monthlySpendCapUSD"), Value: "$20"}, want: `intelligence.monthlySpendCapUSD takes a number, not "$20"`},
		{name: "a duration", change: Change{Setting: mustSetting(t, "sessions.maxAge"), Value: "a week"}, want: `sessions.maxAge takes a duration such as "30s" or "5m", not "a week"`},
		{name: "an empty string", change: Change{Setting: mustSetting(t, "kubectl"), Value: " "}, want: "kubectl cannot be set to nothing; use tug settings unset kubectl"},
		{
			name: "what check refuses", change: Change{Setting: mustSetting(t, "kubectl"), Value: "oc"},
			check: func(c *Config) error {
				if c.Kubectl == "oc" {
					return errors.New("no oc")
				}
				return nil
			},
			want: "no oc",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := EditSettings(dir, []Change{tt.change}, tt.check)
			if err == nil || err.Error() != tt.want {
				t.Errorf("EditSettings = %v, want %q", err, tt.want)
			}
			if b, _ := os.ReadFile(path); string(b) != before {
				t.Errorf("a refused change wrote config.yaml:\n%s", b)
			}
		})
	}
	// A setting under a value that is not a mapping, and a file that is not
	// a mapping, are left for the user to fix.
	if err := os.WriteFile(path, []byte("intelligence: on\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := EditSettings(dir, []Change{{Setting: mustSetting(t, "intelligence.model"), Value: "x"}}, nil); err == nil ||
		err.Error() != "intelligence.model in "+path+": intelligence is not a mapping; edit it by hand" {
		t.Errorf("EditSettings under a scalar = %v", err)
	}
	if err := os.WriteFile(path, []byte("- a list\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := EditSettings(dir, []Change{{Setting: mustSetting(t, "kubectl"), Value: "oc"}}, nil); err == nil || !strings.Contains(err.Error(), "is not a mapping of settings") {
		t.Errorf("EditSettings on a list = %v", err)
	}
}
