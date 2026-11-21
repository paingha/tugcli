package conformance

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/paingha/tugcli/pkg/provider"
)

// wasiModule is the import module of WASI preview 1, which tug provides in
// full to WebAssembly plugins.
const wasiModule = "wasi_snapshot_preview1"

// hostABI is each tug_http function and its number of parameters. Every
// parameter and every result is an i32, and each function has one result.
// docs/plugins.md describes them.
var hostABI = map[string]int{
	"request":       5,
	"response_head": 4,
	"read_body":     4,
	"close":         1,
	"last_error":    3,
}

const valI32 = 0x7f

// moduleCheck names the result of CheckModule.
const moduleCheck = "WebAssembly module imports only WASI and tug_http"

func (r *runner) checkModule() {
	r.results = append(r.results, CheckModule(r.target.Module))
}

// CheckModule checks a WebAssembly plugin without running it: it must be a
// WASI command, exporting _start and its memory, and import nothing but WASI
// preview 1 and the tug_http functions as documented. Run makes this check
// when Target.Module is set.
func CheckModule(path string) Result {
	res := Result{Name: moduleCheck, Status: Fail}
	b, err := os.ReadFile(path)
	if err != nil {
		res.Detail = err.Error()
		return res
	}
	m, err := parseModule(b)
	if err != nil {
		res.Detail = fmt.Sprintf("%s: %v", path, err)
		return res
	}
	var problems []string
	var host []string
	for _, imp := range m.imports {
		switch {
		case imp.kind != kindFunc:
			problems = append(problems, fmt.Sprintf("imports %s %s.%s; a plugin defines its own memory, tables and globals", imp.kind, imp.module, imp.name))
		case imp.module == wasiModule:
		case imp.module == provider.HostModule:
			want, ok := hostABI[imp.name]
			if !ok {
				problems = append(problems, fmt.Sprintf("imports %s.%s, which tug does not provide", imp.module, imp.name))
				continue
			}
			if ft := m.funcType(imp.index); ft == nil || len(ft.params) != want || !allI32(ft.params) || !slices.Equal(ft.results, []byte{valI32}) {
				problems = append(problems, fmt.Sprintf("imports %s.%s as %s; tug provides it as %s", imp.module, imp.name, m.funcType(imp.index), signature(want)))
				continue
			}
			host = append(host, imp.name)
		default:
			problems = append(problems, fmt.Sprintf("imports %s.%s; tug provides only %s and %s", imp.module, imp.name, wasiModule, provider.HostModule))
		}
	}
	if k, ok := m.exports["_start"]; !ok || k != kindFunc {
		problems = append(problems, "exports no _start function, so it is not a WASI command")
	}
	if k, ok := m.exports["memory"]; !ok || k != kindMemory {
		problems = append(problems, `does not export its memory as "memory"`)
	}
	if len(problems) > 0 {
		res.Detail = "the module " + strings.Join(problems, "; ")
		return res
	}
	res.Status = Pass
	if len(host) == 0 {
		res.Detail = "a WASI command with no network access"
		return res
	}
	slices.Sort(host)
	res.Detail = fmt.Sprintf("a WASI command using %s %s", provider.HostModule, strings.Join(slices.Compact(host), ", "))
	return res
}

func allI32(types []byte) bool {
	return !slices.ContainsFunc(types, func(t byte) bool { return t != valI32 })
}

func signature(params int) string {
	return (&funcType{params: bytes.Repeat([]byte{valI32}, params), results: []byte{valI32}}).String()
}

// externKind is what an import or export is.
type externKind byte

const (
	kindFunc externKind = iota
	kindTable
	kindMemory
	kindGlobal
	kindTag
)

func (k externKind) String() string {
	switch k {
	case kindFunc:
		return "function"
	case kindTable:
		return "table"
	case kindMemory:
		return "memory"
	case kindGlobal:
		return "global"
	case kindTag:
		return "tag"
	default:
		return fmt.Sprintf("kind %d", byte(k))
	}
}

type funcType struct{ params, results []byte }

func (f *funcType) String() string {
	if f == nil {
		return "an unknown type"
	}
	name := func(ts []byte) string {
		out := make([]string, len(ts))
		for i, t := range ts {
			switch t {
			case 0x7f:
				out[i] = "i32"
			case 0x7e:
				out[i] = "i64"
			case 0x7d:
				out[i] = "f32"
			case 0x7c:
				out[i] = "f64"
			default:
				out[i] = fmt.Sprintf("0x%02x", t)
			}
		}
		return "(" + strings.Join(out, ", ") + ")"
	}
	return name(f.params) + " -> " + name(f.results)
}

type wasmImport struct {
	module, name string
	kind         externKind
	index        uint32 // the type index, for a function
}

// wasmModule is what the checks need of a module: its function types,
// imports and exports.
type wasmModule struct {
	types   []funcType
	imports []wasmImport
	exports map[string]externKind
}

func (m *wasmModule) funcType(i uint32) *funcType {
	if int(i) >= len(m.types) {
		return nil
	}
	return &m.types[i]
}

var errTruncated = errors.New("the module is truncated")

// parseModule reads the type, import and export sections of a WebAssembly
// binary and skips the rest.
func parseModule(b []byte) (*wasmModule, error) {
	if len(b) < 8 || !bytes.Equal(b[:4], []byte("\x00asm")) {
		return nil, errors.New("not a WebAssembly module")
	}
	if !bytes.Equal(b[4:8], []byte{1, 0, 0, 0}) {
		return nil, fmt.Errorf("WebAssembly binary version %x; tug runs core modules, version 1", b[4:8])
	}
	m := &wasmModule{exports: map[string]externKind{}}
	r := &wasmReader{b: b[8:]}
	for !r.done() {
		id, err := r.byte()
		if err != nil {
			return nil, err
		}
		size, err := r.u32()
		if err != nil {
			return nil, err
		}
		body, err := r.bytes(size)
		if err != nil {
			return nil, err
		}
		sr := &wasmReader{b: body}
		switch id {
		case 1:
			err = m.readTypes(sr)
		case 2:
			err = m.readImports(sr)
		case 7:
			err = m.readExports(sr)
		}
		if err != nil {
			return nil, fmt.Errorf("section %d: %w", id, err)
		}
	}
	return m, nil
}

func (m *wasmModule) readTypes(r *wasmReader) error {
	n, err := r.u32()
	if err != nil {
		return err
	}
	for range n {
		form, err := r.byte()
		if err != nil {
			return err
		}
		if form != 0x60 {
			return fmt.Errorf("type form 0x%02x is not a function type", form)
		}
		var ft funcType
		if ft.params, err = r.vec(); err != nil {
			return err
		}
		if ft.results, err = r.vec(); err != nil {
			return err
		}
		m.types = append(m.types, ft)
	}
	return nil
}

func (m *wasmModule) readImports(r *wasmReader) error {
	n, err := r.u32()
	if err != nil {
		return err
	}
	for range n {
		var imp wasmImport
		if imp.module, err = r.name(); err != nil {
			return err
		}
		if imp.name, err = r.name(); err != nil {
			return err
		}
		k, err := r.byte()
		if err != nil {
			return err
		}
		imp.kind = externKind(k)
		switch imp.kind {
		case kindFunc:
			imp.index, err = r.u32()
		case kindTable:
			if _, err = r.byte(); err == nil { // the element type
				err = r.limits()
			}
		case kindMemory:
			err = r.limits()
		case kindGlobal:
			_, err = r.bytes(2) // value type and mutability
		case kindTag:
			_, err = r.byte()
			if err == nil {
				_, err = r.u32()
			}
		default:
			return fmt.Errorf("import %s.%s has unknown kind %d", imp.module, imp.name, k)
		}
		if err != nil {
			return err
		}
		m.imports = append(m.imports, imp)
	}
	return nil
}

func (m *wasmModule) readExports(r *wasmReader) error {
	n, err := r.u32()
	if err != nil {
		return err
	}
	for range n {
		name, err := r.name()
		if err != nil {
			return err
		}
		k, err := r.byte()
		if err != nil {
			return err
		}
		if _, err := r.u32(); err != nil {
			return err
		}
		m.exports[name] = externKind(k)
	}
	return nil
}

// wasmReader decodes the WebAssembly binary format.
type wasmReader struct{ b []byte }

func (r *wasmReader) done() bool { return len(r.b) == 0 }

func (r *wasmReader) byte() (byte, error) {
	if len(r.b) == 0 {
		return 0, errTruncated
	}
	c := r.b[0]
	r.b = r.b[1:]
	return c, nil
}

func (r *wasmReader) bytes(n uint32) ([]byte, error) {
	if uint64(n) > uint64(len(r.b)) {
		return nil, errTruncated
	}
	out := r.b[:n]
	r.b = r.b[n:]
	return out, nil
}

// u32 reads an unsigned LEB128 number of at most 32 bits.
func (r *wasmReader) u32() (uint32, error) {
	var v uint32
	for shift := 0; shift < 35; shift += 7 {
		c, err := r.byte()
		if err != nil {
			return 0, err
		}
		v |= uint32(c&0x7f) << shift
		if c&0x80 == 0 {
			return v, nil
		}
	}
	return 0, errors.New("a number is longer than 32 bits")
}

func (r *wasmReader) vec() ([]byte, error) {
	n, err := r.u32()
	if err != nil {
		return nil, err
	}
	return r.bytes(n)
}

func (r *wasmReader) name() (string, error) {
	b, err := r.vec()
	return string(b), err
}

// limits skips a table's or memory's limits: flags, a minimum and, when
// the flags say so, a maximum.
func (r *wasmReader) limits() error {
	flags, err := r.byte()
	if err != nil {
		return err
	}
	if _, err := r.u32(); err != nil {
		return err
	}
	if flags&1 != 0 {
		_, err = r.u32()
	}
	return err
}
