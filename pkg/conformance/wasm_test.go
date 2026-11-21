package conformance

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// moduleImport is a function a test module imports, taking params i32s and
// returning results i32s.
type moduleImport struct {
	module, name    string
	params, results int
}

// encodeModule writes a WebAssembly module with the given function imports
// and exports, none of which need bodies: exports name a memory or an
// imported function.
func encodeModule(imports []moduleImport, exports map[string]externKind) []byte {
	leb := func(v int) []byte {
		var out []byte
		for {
			c := byte(v & 0x7f)
			v >>= 7
			if v != 0 {
				out = append(out, c|0x80)
				continue
			}
			return append(out, c)
		}
	}
	name := func(s string) []byte { return append(leb(len(s)), s...) }
	section := func(id byte, body []byte) []byte { return slices.Concat([]byte{id}, leb(len(body)), body) }
	var types, imps []byte
	for i, imp := range imports {
		types = slices.Concat(types, []byte{0x60}, leb(imp.params), slices.Repeat([]byte{valI32}, imp.params), leb(imp.results), slices.Repeat([]byte{valI32}, imp.results))
		imps = slices.Concat(imps, name(imp.module), name(imp.name), []byte{byte(kindFunc)}, leb(i))
	}
	var exps []byte
	names := make([]string, 0, len(exports))
	for n := range exports {
		names = append(names, n)
	}
	slices.Sort(names)
	for _, n := range names {
		exps = slices.Concat(exps, name(n), []byte{byte(exports[n])}, leb(0))
	}
	return slices.Concat(
		[]byte("\x00asm\x01\x00\x00\x00"),
		section(0, slices.Concat(name("name"), []byte{0})), // a custom section, skipped
		section(1, slices.Concat(leb(len(imports)), types)),
		section(2, slices.Concat(leb(len(imports)), imps)),
		section(5, []byte{1, 0, 1}),
		section(7, slices.Concat(leb(len(exports)), exps)),
	)
}

func TestCheckModule(t *testing.T) {
	command := map[string]externKind{"_start": kindFunc, "memory": kindMemory}
	wasi := moduleImport{wasiModule, "fd_write", 4, 1}
	request := moduleImport{"tug_http", "request", 5, 1}
	tests := []struct {
		name    string
		module  []byte
		status  Status
		message string
	}{
		{"WASI only", encodeModule([]moduleImport{wasi}, command), Pass, "no network access"},
		{"WASI and tug_http", encodeModule([]moduleImport{wasi, request, {"tug_http", "close", 1, 1}}, command), Pass, "tug_http close, request"},
		{"wrong signature", encodeModule([]moduleImport{{"tug_http", "request", 4, 1}}, command), Fail, "as (i32, i32, i32, i32) -> (i32); tug provides it as (i32, i32, i32, i32, i32) -> (i32)"},
		{"unknown host function", encodeModule([]moduleImport{{"tug_http", "socket", 1, 1}}, command), Fail, "tug_http.socket, which tug does not provide"},
		{"another host's imports", encodeModule([]moduleImport{{"env", "abort", 0, 0}}, command), Fail, "imports env.abort; tug provides only wasi_snapshot_preview1 and tug_http"},
		{"not a command", encodeModule(nil, map[string]externKind{"memory": kindMemory}), Fail, "no _start function"},
		{"memory not exported", encodeModule(nil, map[string]externKind{"_start": kindFunc}), Fail, `does not export its memory`},
		{"not WebAssembly", []byte("#!/bin/sh\n"), Fail, "not a WebAssembly module"},
		{"a component", []byte("\x00asm\x0d\x00\x01\x00"), Fail, "version 1"},
		{"truncated", encodeModule([]moduleImport{wasi}, command)[:20], Fail, "truncated"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "plugin.wasm")
			if err := os.WriteFile(path, tt.module, 0o600); err != nil {
				t.Fatal(err)
			}
			got := CheckModule(path)
			if got.Status != tt.status || !strings.Contains(got.Detail, tt.message) {
				t.Fatalf("CheckModule = %s %q, want %s with %q", got.Status, got.Detail, tt.status, tt.message)
			}
		})
	}
}
