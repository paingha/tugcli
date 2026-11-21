package sandbox

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func TestExpand(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix paths; Windows has no sandbox")
	}
	lookup := lookupFunc([]string{"HOME=/home/ada", "TELEPORT_HOME=/srv/tsh", "EMPTY="})
	tests := []struct {
		in   string
		want string
		ok   bool
	}{
		{"/etc/acme", "/etc/acme", true},
		{"~", "/home/ada", true},
		{"~/.tsh", "/home/ada/.tsh", true},
		{"$TELEPORT_HOME", "/srv/tsh", true},
		{"${TELEPORT_HOME}/keys/", "/srv/tsh/keys", true},
		{"$UNSET/keys", "", false},
		{"$EMPTY/keys", "", false},
		{"~ada/.tsh", "", false}, // another user's home is not supported
		{"relative", "", false},
	}
	for _, tt := range tests {
		got, ok := expand(tt.in, lookup)
		if got != tt.want || ok != tt.ok {
			t.Errorf("expand(%q) = %q, %v, want %q, %v", tt.in, got, ok, tt.want, tt.ok)
		}
	}
}

func TestProxyPort(t *testing.T) {
	tests := []struct {
		in   string
		want uint16
	}{
		{"", 0},
		{"http://proxy.example:3128", 3128},
		{"proxy.example:8080", 8080},
		{"https://proxy.example", 443},
		{"http://proxy.example", 80},
		{"socks5://proxy.example", 1080},
		{"http://proxy.example:99999", 0},
		{"://", 0},
	}
	for _, tt := range tests {
		if got := proxyPort(tt.in); got != tt.want {
			t.Errorf("proxyPort(%q) = %d, want %d", tt.in, got, tt.want)
		}
	}
}

func writeExecutable(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o700); err != nil {
		t.Fatal(err)
	}
}

func TestShebang(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix interpreter paths; Windows has no sandbox")
	}
	dir := t.TempDir()
	tests := []struct {
		content string
		interp  string
		args    []string
	}{
		{"#!/bin/sh\necho hi\n", "/bin/sh", nil},
		{"#! /usr/bin/env -S python3 -u\n", "/usr/bin/env", []string{"-S", "python3", "-u"}},
		{"#!/usr/bin/env python3", "/usr/bin/env", []string{"python3"}},
		{"#!relative/sh\n", "", nil},
		{"\x7fELF\x02\x01\x01", "", nil},
		{"", "", nil},
	}
	for i, tt := range tests {
		path := filepath.Join(dir, strings.Repeat("x", i+1))
		writeExecutable(t, path, tt.content)
		interp, args := shebang(path)
		if interp != tt.interp || !slices.Equal(args, tt.args) {
			t.Errorf("shebang(%q) = %q, %q, want %q, %q", tt.content, interp, args, tt.interp, tt.args)
		}
	}
}

func TestResolve(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("scripts and Unix paths")
	}
	root := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved // macOS's temporary directory is behind a link
	}
	home := filepath.Join(root, "home")
	pluginDir := filepath.Join(root, "plugin")
	bin := filepath.Join(pluginDir, "tug-provider-acme")
	tools := filepath.Join(root, "tools")
	// A script plugin whose interpreter env finds on the plugin's PATH.
	writeExecutable(t, bin, "#!/usr/bin/env acme-python\n")
	writeExecutable(t, filepath.Join(tools, "acme-python"), "binary")
	writeExecutable(t, filepath.Join(tools, "acme"), "#!/bin/sh\n")
	if err := os.Symlink(filepath.Join(tools, "acme"), filepath.Join(tools, "acme-link")); err != nil {
		t.Fatal(err)
	}
	env := []string{
		"HOME=" + home, "PATH=relative:" + tools, "ACME_HOME=" + filepath.Join(root, "acme-home"),
		"HTTPS_PROXY=http://proxy.example:3128", "SSL_CERT_FILE=" + filepath.Join(root, "ca.pem"),
		"KUBECONFIG=" + filepath.Join(root, "kube-a") + ":" + filepath.Join(root, "kube-b"),
	}
	p := Policy{
		Name:    "acme",
		Dir:     pluginDir,
		Tools:   []string{"acme-link", "missing-tool", filepath.Join(tools, "acme")},
		Read:    []string{"~/.acme", "$ACME_HOME", "$UNSET/x", "$KUBECONFIG"},
		Write:   []string{"~/.acme/cache"},
		Sockets: []string{"/run/acme.sock"},
		Ports:   []uint16{3080, 443},
	}
	tmp := filepath.Join(root, "tmp")
	r := resolve(p, bin, env, tmp)

	for _, want := range []string{bin, "/usr/bin/env", filepath.Join(tools, "acme-python"), filepath.Join(tools, "acme-link"), filepath.Join(tools, "acme"), "/bin/sh"} {
		if !slices.Contains(r.Programs, want) {
			t.Errorf("Programs = %v, want %s among them", r.Programs, want)
		}
	}
	if slices.ContainsFunc(r.Programs, func(p string) bool { return strings.Contains(p, "missing-tool") }) {
		t.Errorf("Programs = %v, want no missing tool", r.Programs)
	}
	if !slices.Contains(r.Libraries, pluginDir) {
		t.Errorf("Libraries = %v, want the plugin directory %s", r.Libraries, pluginDir)
	}
	for _, want := range []string{
		filepath.Join(home, ".acme"), filepath.Join(root, "acme-home"), tools, filepath.Join(root, "ca.pem"),
		filepath.Join(root, "kube-a"), filepath.Join(root, "kube-b"), // a list in one variable
	} {
		if !slices.Contains(r.Read, want) {
			t.Errorf("Read = %v, want %s among them", r.Read, want)
		}
	}
	if slices.ContainsFunc(r.Read, func(p string) bool { return strings.HasSuffix(p, "/x") }) {
		t.Errorf("Read = %v, want no path with an unset variable", r.Read)
	}
	for _, want := range []string{tmp, filepath.Join(home, ".acme", "cache")} {
		if !slices.Contains(r.Write, want) {
			t.Errorf("Write = %v, want %s among them", r.Write, want)
		}
	}
	if !slices.Contains(r.Sockets, "/run/acme.sock") {
		t.Errorf("Sockets = %v, want /run/acme.sock", r.Sockets)
	}
	if want := []uint16{53, 443, 3080, 3128}; !slices.Equal(r.Ports, want) {
		t.Errorf("Ports = %v, want %v", r.Ports, want)
	}
	if !slices.IsSorted(r.Read) || len(slices.Compact(slices.Clone(r.Read))) != len(r.Read) {
		t.Errorf("Read = %v, want it sorted without repeats", r.Read)
	}
}

func TestCommandSetsUpTheTemporaryDirectory(t *testing.T) {
	if !Check().Any() {
		t.Skipf("no sandbox here: %s", Check().Missing)
	}
	root := t.TempDir()
	s := &Sandbox{Policy: Policy{Name: "acme/../x"}, Self: "/path/to/tug", TempRoot: filepath.Join(root, "plugins")}
	c, err := s.Command("/path/to/tug-provider-acme", []string{"list"}, []string{"TMPDIR=/elsewhere", "HOME=/home/ada"})
	if err != nil {
		t.Fatal(err)
	}
	if !c.Confined || c.Path == "/path/to/tug-provider-acme" || !slices.Contains(c.Args, "/path/to/tug-provider-acme") || c.Args[len(c.Args)-1] != "list" {
		t.Errorf("Command = %+v, want the plugin started through the sandbox", c)
	}
	tmp := lookupFunc(c.Env)("TMPDIR")
	if fi, err := os.Stat(tmp); err != nil || !fi.IsDir() || !strings.Contains(filepath.Base(tmp), "tug-plugin-acmex-") {
		t.Fatalf("TMPDIR = %q (%v), want a new directory named for the plugin", tmp, err)
	}
	if n := strings.Count(strings.Join(c.Env, "\n"), "TMPDIR="); n != 1 {
		t.Errorf("Env sets TMPDIR %d times, want once", n)
	}
	if runtime.GOOS != "windows" {
		if fi, _ := os.Stat(tmp); fi.Mode().Perm() != 0o700 {
			t.Errorf("temporary directory mode = %v, want 0700", fi.Mode().Perm())
		}
	}
	c.Cleanup()
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Errorf("after Cleanup, the temporary directory is still there: %v", err)
	}
}

func TestCommandWithoutSandbox(t *testing.T) {
	if Check().Any() {
		t.Skip("this platform has a sandbox")
	}
	s := &Sandbox{Policy: Policy{Name: "acme"}, TempRoot: t.TempDir()}
	c, err := s.Command("tug-provider-acme", []string{"list"}, []string{"HOME=/home/ada"})
	if err != nil {
		t.Fatal(err)
	}
	if c.Confined || c.Path != "tug-provider-acme" || !slices.Equal(c.Args, []string{"list"}) || !slices.Equal(c.Env, []string{"HOME=/home/ada"}) {
		t.Errorf("Command = %+v, want the plugin run as it is", c)
	}
	c.Cleanup()
}
