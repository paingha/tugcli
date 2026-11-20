package config

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestEnsurePrivateDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "run")
	if err := EnsurePrivateDir(dir); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		return
	}
	if fi, err := os.Stat(dir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("created %v, %v; want mode 0700", fi.Mode(), err)
	}
}

func TestCheckPrivateDir(t *testing.T) {
	if err := CheckPrivateDir(filepath.Join(t.TempDir(), "missing")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing directory: %v, want ErrNotExist", err)
	}
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CheckPrivateDir(file); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("a file: %v", err)
	}
	if runtime.GOOS == "windows" {
		return // links need privileges, and there are no Unix modes
	}

	readable := t.TempDir()
	if err := os.Chmod(readable, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := CheckPrivateDir(readable); err != nil {
		t.Fatalf("others may read, but not write: %v", err)
	}
	for _, mode := range []os.FileMode{0o777, 0o770, 0o702} {
		dir := t.TempDir()
		if err := os.Chmod(dir, mode); err != nil {
			t.Fatal(err)
		}
		if err := CheckPrivateDir(dir); err == nil || !strings.Contains(err.Error(), "can write to it") {
			t.Errorf("mode %#o: %v", mode, err)
		}
		if err := EnsurePrivateDir(dir); err == nil {
			t.Errorf("EnsurePrivateDir accepted an existing directory with mode %#o", mode)
		}
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(t.TempDir(), link); err != nil {
		t.Fatal(err)
	}
	if err := CheckPrivateDir(link); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("a link: %v", err)
	}
}
