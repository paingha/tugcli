package config

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// clearPathEnv unsets every variable DefaultPaths reads, so the developer's
// environment cannot leak into a test.
func clearPathEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"TUG_HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME", "XDG_RUNTIME_DIR"} {
		t.Setenv(k, "")
	}
}

// setHome points os.UserHomeDir at dir on every platform.
func setHome(t *testing.T, dir string) {
	t.Helper()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
}

func TestDefaultPathsTUGHome(t *testing.T) {
	clearPathEnv(t)
	home := t.TempDir()
	t.Setenv("TUG_HOME", home)
	// TUG_HOME wins over the XDG variables.
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(t.TempDir(), "ignored"))

	got, err := DefaultPaths()
	if err != nil {
		t.Fatal(err)
	}
	want := Paths{
		Config:  filepath.Join(home, "config"),
		Data:    filepath.Join(home, "data"),
		Cache:   filepath.Join(home, "cache"),
		Runtime: filepath.Join(home, "run"),
	}
	if got != want {
		t.Fatalf("DefaultPaths() = %+v, want %+v", got, want)
	}
}

func TestDefaultPathsXDG(t *testing.T) {
	clearPathEnv(t)
	base := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(base, "cfg"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(base, "data"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(base, "cache"))
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(base, "run"))

	got, err := DefaultPaths()
	if err != nil {
		t.Fatal(err)
	}
	want := Paths{
		Config:  filepath.Join(base, "cfg", "tug"),
		Data:    filepath.Join(base, "data", "tug"),
		Cache:   filepath.Join(base, "cache", "tug"),
		Runtime: filepath.Join(base, "run", "tug"),
	}
	if got != want {
		t.Fatalf("DefaultPaths() = %+v, want %+v", got, want)
	}
}

func TestDefaultPathsIgnoresRelativeXDG(t *testing.T) {
	clearPathEnv(t)
	home := t.TempDir()
	setHome(t, home)
	// The XDG spec says relative paths are invalid and must be ignored. They
	// are non-empty, so the Windows overrides do not apply either.
	t.Setenv("XDG_CONFIG_HOME", "relative/config")
	t.Setenv("XDG_DATA_HOME", "relative/data")
	t.Setenv("XDG_CACHE_HOME", "relative/cache")

	got, err := DefaultPaths()
	if err != nil {
		t.Fatal(err)
	}
	want := Paths{
		Config: filepath.Join(home, ".config", "tug"),
		Data:   filepath.Join(home, ".local", "share", "tug"),
		Cache:  filepath.Join(home, ".cache", "tug"),
	}
	if got.Config != want.Config || got.Data != want.Data || got.Cache != want.Cache {
		t.Fatalf("DefaultPaths() = %+v, want Config/Data/Cache of %+v", got, want)
	}
}

func TestDefaultPathsHomeFallback(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows uses %AppData% and %LocalAppData%; see TestDefaultPathsWindows")
	}
	clearPathEnv(t)
	home := t.TempDir()
	setHome(t, home)

	got, err := DefaultPaths()
	if err != nil {
		t.Fatal(err)
	}
	want := Paths{
		Config: filepath.Join(home, ".config", "tug"),
		Data:   filepath.Join(home, ".local", "share", "tug"),
		Cache:  filepath.Join(home, ".cache", "tug"),
	}
	if got.Config != want.Config || got.Data != want.Data || got.Cache != want.Cache {
		t.Fatalf("DefaultPaths() = %+v, want Config/Data/Cache of %+v", got, want)
	}
}

func TestDefaultPathsWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows only")
	}
	clearPathEnv(t)
	setHome(t, t.TempDir())
	appData := t.TempDir()
	local := t.TempDir()
	t.Setenv("APPDATA", appData)
	t.Setenv("LOCALAPPDATA", local)

	got, err := DefaultPaths()
	if err != nil {
		t.Fatal(err)
	}
	want := Paths{
		Config: filepath.Join(appData, "tug"),
		Data:   filepath.Join(local, "tug"),
		Cache:  filepath.Join(local, "cache", "tug"),
	}
	if got.Config != want.Config || got.Data != want.Data || got.Cache != want.Cache {
		t.Fatalf("DefaultPaths() = %+v, want Config/Data/Cache of %+v", got, want)
	}
}

func TestDefaultPathsNoHome(t *testing.T) {
	clearPathEnv(t)
	setHome(t, "")
	if _, err := DefaultPaths(); err == nil || !strings.Contains(err.Error(), "home directory") {
		t.Fatalf("DefaultPaths() without a home error = %v, want one about the home directory", err)
	}
}

func TestDefaultPathsRuntimeFallback(t *testing.T) {
	clearPathEnv(t)
	setHome(t, t.TempDir())
	tag := strconv.Itoa(os.Getuid())
	if runtime.GOOS == "windows" {
		// os.Getuid is -1 on Windows, so the user name is used instead.
		t.Setenv("USERNAME", "alice")
		tag = "alice"
	}

	got, err := DefaultPaths()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(os.TempDir(), "tug-"+tag); got.Runtime != want {
		t.Fatalf("Runtime = %q, want %q", got.Runtime, want)
	}
}

// clearConfigEnv unsets the environment overrides Load honours.
func clearConfigEnv(t *testing.T) {
	t.Helper()
	t.Setenv("TUG_KEYRING_BACKEND", "")
	t.Setenv("TUG_KUBECTL", "")
}

func writeConfig(t *testing.T, yaml string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestLoadMissingFileGivesDefaults(t *testing.T) {
	clearConfigEnv(t)
	c, err := Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if c.Keyring.Backend != "auto" {
		t.Errorf("Keyring.Backend = %q, want %q", c.Keyring.Backend, "auto")
	}
	if c.Kubectl != "kubectl" {
		t.Errorf("Kubectl = %q, want %q", c.Kubectl, "kubectl")
	}
	if want := []map[string]string{{"env": "prod"}}; !reflect.DeepEqual(c.Guard.ReadOnlyLabels, want) {
		t.Errorf("Guard.ReadOnlyLabels = %v, want %v", c.Guard.ReadOnlyLabels, want)
	}
	if want := 7 * 24 * time.Hour; c.Sessions.MaxAge.Duration != want {
		t.Errorf("Sessions.MaxAge = %v, want %v", c.Sessions.MaxAge.Duration, want)
	}
}

func TestLoadReadsFile(t *testing.T) {
	clearConfigEnv(t)
	dir := writeConfig(t, `
keyring:
  backend: file
kubectl: /opt/bin/kubectl
guard:
  readOnlyLabels:
  - {tier: critical}
plugins:
  allowOverride: [kubeconfig]
  timeouts:
    list: 90s
sessions:
  maxAge: 24h
`)
	c, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if c.Keyring.Backend != "file" {
		t.Errorf("Keyring.Backend = %q, want %q", c.Keyring.Backend, "file")
	}
	if c.Kubectl != "/opt/bin/kubectl" {
		t.Errorf("Kubectl = %q, want %q", c.Kubectl, "/opt/bin/kubectl")
	}
	if want := []map[string]string{{"tier": "critical"}}; !reflect.DeepEqual(c.Guard.ReadOnlyLabels, want) {
		t.Errorf("Guard.ReadOnlyLabels = %v, want %v (defaults must not be added)", c.Guard.ReadOnlyLabels, want)
	}
	if want := []string{"kubeconfig"}; !reflect.DeepEqual(c.Plugins.AllowOverride, want) {
		t.Errorf("Plugins.AllowOverride = %v, want %v", c.Plugins.AllowOverride, want)
	}
	if got := c.Plugins.Timeouts["list"].Duration; got != 90*time.Second {
		t.Errorf("Plugins.Timeouts[list] = %v, want 90s", got)
	}
	if c.Sessions.MaxAge.Duration != 24*time.Hour {
		t.Errorf("Sessions.MaxAge = %v, want 24h", c.Sessions.MaxAge.Duration)
	}
}

func TestLoadErrors(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want []string // substrings of the error
	}{
		{"unknown top-level field", "kubeclt: kubectl\n", []string{"config.yaml", "kubeclt"}},
		{"unknown nested field", "keyring:\n  backnd: file\n", []string{"config.yaml", "backnd"}},
		{"duration that is not a string", "sessions:\n  maxAge: 30\n", []string{"config.yaml", "duration must be a string"}},
		{"bad duration", "sessions:\n  maxAge: soon\n", []string{"config.yaml", "soon"}},
		{"not YAML", "keyring: [\n", []string{"config.yaml"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearConfigEnv(t)
			_, err := Load(writeConfig(t, tt.yaml))
			if err == nil {
				t.Fatal("Load succeeded, want an error")
			}
			for _, s := range tt.want {
				if !strings.Contains(err.Error(), s) {
					t.Errorf("error %q does not mention %q", err, s)
				}
			}
		})
	}
}

func TestLoadUnreadableFile(t *testing.T) {
	clearConfigEnv(t)
	dir := t.TempDir()
	// A directory where the file should be cannot be read.
	if err := os.Mkdir(filepath.Join(dir, "config.yaml"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil {
		t.Fatal("Load succeeded, want an error")
	}
}

func TestLoadEnvOverrides(t *testing.T) {
	tests := []struct {
		name        string
		yaml        string // "" means no file
		backend     string
		kubectl     string
		wantBackend string
		wantKubectl string
	}{
		{
			name:        "env over defaults",
			backend:     "file",
			kubectl:     "/usr/local/bin/kubectl",
			wantBackend: "file",
			wantKubectl: "/usr/local/bin/kubectl",
		},
		{
			name:        "env over file",
			yaml:        "keyring: {backend: keychain}\nkubectl: /opt/kubectl\n",
			backend:     "file",
			kubectl:     "/env/kubectl",
			wantBackend: "file",
			wantKubectl: "/env/kubectl",
		},
		{
			name:        "empty env keeps file",
			yaml:        "keyring: {backend: keychain}\nkubectl: /opt/kubectl\n",
			wantBackend: "keychain",
			wantKubectl: "/opt/kubectl",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("TUG_KEYRING_BACKEND", tt.backend)
			t.Setenv("TUG_KUBECTL", tt.kubectl)
			dir := t.TempDir()
			if tt.yaml != "" {
				dir = writeConfig(t, tt.yaml)
			}
			c, err := Load(dir)
			if err != nil {
				t.Fatal(err)
			}
			if c.Keyring.Backend != tt.wantBackend {
				t.Errorf("Keyring.Backend = %q, want %q", c.Keyring.Backend, tt.wantBackend)
			}
			if c.Kubectl != tt.wantKubectl {
				t.Errorf("Kubectl = %q, want %q", c.Kubectl, tt.wantKubectl)
			}
		})
	}
}

func TestDurationJSON(t *testing.T) {
	tests := []struct {
		d    time.Duration
		json string
	}{
		{30 * time.Second, `"30s"`},
		{90 * time.Second, `"1m30s"`},
		{7 * 24 * time.Hour, `"168h0m0s"`},
		{0, `"0s"`},
	}
	for _, tt := range tests {
		t.Run(tt.json, func(t *testing.T) {
			b, err := json.Marshal(Duration{tt.d})
			if err != nil {
				t.Fatal(err)
			}
			if string(b) != tt.json {
				t.Fatalf("Marshal(%v) = %s, want %s", tt.d, b, tt.json)
			}
			var got Duration
			if err := json.Unmarshal(b, &got); err != nil {
				t.Fatal(err)
			}
			if got.Duration != tt.d {
				t.Fatalf("round trip = %v, want %v", got.Duration, tt.d)
			}
		})
	}
}

func TestDurationUnmarshalErrors(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{`30`, "must be a string"},
		{`true`, "must be a string"},
		{`"soon"`, "soon"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			var d Duration
			err := json.Unmarshal([]byte(tt.in), &d)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Unmarshal(%s) error = %v, want one containing %q", tt.in, err, tt.want)
			}
		})
	}
}

func TestPluginTimeouts(t *testing.T) {
	c := &Config{Plugins: PluginsConfig{Timeouts: map[string]Duration{
		"list":     {90 * time.Second},
		"rotate":   {2 * time.Minute},
		"describe": {5 * time.Second},
		"zero":     {0},
	}}}
	want := map[string]time.Duration{
		"list":     90 * time.Second,
		"rotate":   2 * time.Minute,
		"describe": 5 * time.Second,
	}
	if got := c.PluginTimeouts(); !maps.Equal(got, want) {
		t.Fatalf("PluginTimeouts() = %v, want %v", got, want)
	}
	if got := (&Config{}).PluginTimeouts(); len(got) != 0 {
		t.Fatalf("PluginTimeouts() with none configured = %v, want empty", got)
	}
}

func TestDefaultsKeepsSetValues(t *testing.T) {
	c := &Config{
		Keyring:  KeyringConfig{Backend: "file"},
		Kubectl:  "k",
		Guard:    GuardConfig{ReadOnlyLabels: []map[string]string{{"tier": "1"}}},
		Sessions: SessionsConfig{MaxAge: Duration{time.Hour}},
	}
	want := *c
	c.Defaults()
	if !reflect.DeepEqual(*c, want) {
		t.Fatalf("Defaults changed set values: got %+v, want %+v", *c, want)
	}
}

func TestWriteFileAtomic(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "dir") // created on demand
	path := filepath.Join(dir, "catalog.yaml")

	if err := WriteFileAtomic(path, []byte("first"), 0o600); err != nil {
		t.Fatal(err)
	}
	assertFile(t, path, "first", 0o600)

	// Replacing keeps readers from seeing a partial file and applies perm.
	if err := WriteFileAtomic(path, []byte("second, longer"), 0o644); err != nil {
		t.Fatal(err)
	}
	assertFile(t, path, "second, longer", 0o644)

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "catalog.yaml" {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("directory holds %v, want only catalog.yaml (temp files left behind?)", names)
	}
}

func assertFile(t *testing.T, path, want string, perm os.FileMode) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != want {
		t.Errorf("%s = %q, want %q", filepath.Base(path), b, want)
	}
	if runtime.GOOS == "windows" {
		return // Windows has no Unix permission bits.
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != perm {
		t.Errorf("%s mode = %v, want %v", filepath.Base(path), got, perm)
	}
}

func TestWriteFileAtomicErrors(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// The parent "directory" is a regular file.
	if err := WriteFileAtomic(filepath.Join(blocker, "x.yaml"), []byte("x"), 0o600); err == nil {
		t.Fatal("WriteFileAtomic under a file succeeded, want an error")
	}

	// The target is a non-empty directory, so the final rename fails and the
	// temporary file must still be removed.
	target := filepath.Join(dir, "target")
	if err := os.MkdirAll(filepath.Join(target, "child"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileAtomic(target, []byte("x"), 0o600); err == nil {
		t.Fatal("WriteFileAtomic over a directory succeeded, want an error")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Fatalf("temporary file %s left behind after a failed rename", e.Name())
		}
	}
}
