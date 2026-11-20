package browser

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestOpenRunsBrowserEnv(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script browser")
	}
	dir := t.TempDir()
	out := filepath.Join(dir, "opened")
	script := filepath.Join(dir, "browser")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf '%s' \"$1\" > '"+out+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	getenv := func(k string) string {
		if k == Env {
			return script
		}
		return ""
	}
	const url = "https://idp.example.com/authorize?a=1&b=2"
	if err := Open(context.Background(), url, getenv); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		got, err := os.ReadFile(out)
		if err == nil && string(got) == url {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the browser command got %q, %v; want %q", got, err, url)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestOpenReportsAMissingCommand(t *testing.T) {
	getenv := func(string) string { return filepath.Join(t.TempDir(), "no-such-browser") }
	if err := Open(context.Background(), "https://example.com", getenv); err == nil {
		t.Fatal("Open with a missing command succeeded")
	}
}
