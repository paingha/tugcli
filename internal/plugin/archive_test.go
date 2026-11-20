package plugin

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// examplePluginDir builds the example plugin with per-platform binary paths
// so it can be packaged.
func examplePluginDir(t *testing.T) string {
	t.Helper()
	return buildExample(t)
}

func TestPackageInstallRemove(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary")
	}
	src := examplePluginDir(t)
	out := t.TempDir()
	res, err := Package(src, out)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(res.Path, "tug-provider-example_0.1.0.tar.gz") || len(res.SHA256) != 64 {
		t.Fatalf("package result: %+v", res)
	}
	plat, err := res.Manifest.Platform()
	if err != nil || plat.SHA256 == "" {
		t.Fatalf("this platform should be packaged with a checksum: %+v %v", plat, err)
	}
	// The example points every platform at one binary built for this machine;
	// only the platforms it can run on may be packaged.
	for _, p := range res.Manifest.Spec.Platforms {
		if p.OS != runtime.GOOS {
			t.Errorf("packaged %s/%s with a %s binary", p.OS, p.Arch, runtime.GOOS)
		}
	}
	if len(res.Mismatched) == 0 {
		t.Error("other platforms should be reported as mismatched")
	}

	d := NewDirs(t.TempDir())
	if _, err := d.Stage(res.Path, strings.Repeat("0", 64)); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("wrong archive sha256 accepted: %v", err)
	}
	staged, err := d.Stage(res.Path, res.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	p, err := d.Commit(staged, false)
	if err != nil {
		t.Fatal(err)
	}
	if p.Dev || filepath.Base(p.Dir) != "0.1.0" {
		t.Fatalf("installed plugin: %+v", p)
	}

	t.Setenv(PathEnv, "")
	plugins, problems := d.Discover()
	if len(problems) != 0 || len(plugins) != 1 || plugins[0].Name() != "example" || plugins[0].Dev {
		t.Fatalf("discover after install: %+v %v", plugins, problems)
	}
	entries, _ := os.ReadDir(d.Installed)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".staging") {
			t.Fatalf("staging directory left behind: %s", e.Name())
		}
	}

	again, err := d.Stage(res.Path, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Commit(again, false); err == nil || !strings.Contains(err.Error(), "already installed") {
		t.Fatalf("reinstall without --force: %v", err)
	}
	again, _ = d.Stage(res.Path, "")
	if _, err := d.Commit(again, true); err != nil {
		t.Fatalf("reinstall with --force: %v", err)
	}

	versions, err := d.Remove("example")
	if err != nil || len(versions) != 1 || versions[0] != "0.1.0" {
		t.Fatalf("remove: %v %v", versions, err)
	}
	if _, err := d.Remove("example"); err == nil {
		t.Fatal("removing twice should fail")
	}
}

func TestFetch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("archive-bytes"))
	}))
	defer srv.Close()
	path, err := Fetch(context.Background(), srv.URL+"/p.tar.gz", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); string(b) != "archive-bytes" || !strings.HasSuffix(path, ".tar.gz") {
		t.Fatalf("fetched %q to %s", b, path)
	}
	if _, err := Fetch(context.Background(), "http://example.com/p.tar.gz", t.TempDir()); err == nil {
		t.Fatal("plain http to a remote host accepted")
	}
}

type member struct {
	name     string
	body     string
	typeflag byte
	link     string
}

func tarGz(t *testing.T, members []member) string {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, m := range members {
		tf := m.typeflag
		if tf == 0 {
			tf = tar.TypeReg
		}
		hdr := &tar.Header{Name: m.name, Mode: 0o755, Size: int64(len(m.body)), Typeflag: tf, Linkname: m.link}
		if tf != tar.TypeReg {
			hdr.Size = 0
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if tf == tar.TypeReg {
			tw.Write([]byte(m.body))
		}
	}
	tw.Close()
	gz.Close()
	path := filepath.Join(t.TempDir(), "p.tar.gz")
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func zipFile(t *testing.T, members []member) string {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, m := range members {
		w, err := zw.Create(m.name)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(m.body))
	}
	zw.Close()
	path := filepath.Join(t.TempDir(), "p.zip")
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestStageRejectsHostileArchives(t *testing.T) {
	manifest := strings.ReplaceAll(scriptManifest, "%s", "x")
	tests := []struct {
		name    string
		archive func(t *testing.T) string
		want    string
	}{
		{"path traversal", func(t *testing.T) string {
			return tarGz(t, []member{{name: "../../evil", body: "x"}})
		}, "escapes"},
		{"absolute path", func(t *testing.T) string {
			return tarGz(t, []member{{name: "/etc/evil", body: "x"}})
		}, "escapes"},
		{"symlink", func(t *testing.T) string {
			return tarGz(t, []member{{name: "bin", typeflag: tar.TypeSymlink, link: "/usr/bin"}})
		}, "link or special file"},
		{"zip traversal", func(t *testing.T) string {
			return zipFile(t, []member{{name: "../evil", body: "x"}})
		}, "escapes"},
		{"no manifest", func(t *testing.T) string {
			return tarGz(t, []member{{name: "README", body: "hi"}})
		}, "no plugin.yaml"},
		{"not an archive", func(t *testing.T) string {
			p := filepath.Join(t.TempDir(), "x.tar.gz")
			os.WriteFile(p, []byte("hello world"), 0o600)
			return p
		}, "neither"},
		{"binary does not match manifest", func(t *testing.T) string {
			m := strings.Replace(scriptManifest, "%s", runtime.GOOS, 1)
			m = strings.Replace(m, "%s", runtime.GOARCH, 1)
			m = strings.Replace(m, "%s", strings.Repeat("a", 64), 1)
			return tarGz(t, []member{{name: ManifestFile, body: m}, {name: "tug-provider-script", body: "#!/bin/sh\n"}})
		}, "checksum mismatch"},
		{"manifest without this platform", func(t *testing.T) string {
			return tarGz(t, []member{{name: ManifestFile, body: manifest}})
		}, "no build for"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := t.TempDir()
			d := NewDirs(data)
			_, err := d.Stage(tt.archive(t), "")
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Stage = %v, want error mentioning %q", err, tt.want)
			}
			if _, err := os.Stat(filepath.Join(filepath.Dir(data), "evil")); err == nil {
				t.Fatal("a file was written outside the plugin directory")
			}
			entries, _ := os.ReadDir(d.Installed)
			if len(entries) != 0 {
				t.Fatalf("leftovers after a failed stage: %v", entries)
			}
		})
	}
}

func TestBinaryPlatform(t *testing.T) {
	exe, err := os.Executable() // the test binary itself
	if err != nil {
		t.Fatal(err)
	}
	goos, goarch := binaryPlatform(exe)
	if goos != runtime.GOOS || goarch != runtime.GOARCH {
		t.Fatalf("binaryPlatform(test binary) = %s/%s, want %s/%s", goos, goarch, runtime.GOOS, runtime.GOARCH)
	}
	script := filepath.Join(t.TempDir(), "s")
	os.WriteFile(script, []byte("#!/bin/sh\n"), 0o755)
	if !matchesPlatform(script, Platform{OS: "windows", Arch: "arm64"}) {
		t.Fatal("scripts should match every platform")
	}
	if matchesPlatform(exe, Platform{OS: "plan9", Arch: "386"}) {
		t.Fatal("native binary matched a foreign platform")
	}
}
