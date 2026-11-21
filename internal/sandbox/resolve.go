package sandbox

import (
	"bufio"
	"bytes"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// rules are a policy resolved to real paths and ports, ready for the
// platform to enforce. Linux's launcher reads them as JSON.
type rules struct {
	// Programs are executables the plugin may run: itself, its script
	// interpreter and its tools.
	Programs []string `json:"programs,omitempty"`
	// ProgramDirs are directories whose executables it may run.
	ProgramDirs []string `json:"programDirs,omitempty"`
	// Libraries are directories it may read and load code from.
	Libraries []string `json:"libraries,omitempty"`
	// Read lists files and directories it may read.
	Read []string `json:"read,omitempty"`
	// Write lists files and directories it may read and write.
	Write []string `json:"write,omitempty"`
	// WritePrefixes lets it write files whose paths start with these, for
	// caches with random names. Only macOS can enforce prefixes.
	WritePrefixes []string `json:"writePrefixes,omitempty"`
	// Sockets lists Unix sockets it may connect to.
	Sockets []string `json:"sockets,omitempty"`
	// Ports lists TCP ports it may connect to.
	Ports []uint16 `json:"ports,omitempty"`
}

// proxyVars name the proxies a plugin may be told to use; their ports are
// allowed so that proxied requests still work.
var proxyVars = []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "ALL_PROXY", "all_proxy"}

// resolve turns a policy into rules for a plugin executable bin, run with
// env, whose private temporary directory is tmp.
func resolve(p Policy, bin string, env []string, tmp string) rules {
	lookup := lookupFunc(env)
	var r rules
	addProgram(&r, bin, lookup, 0)
	r.Libraries = appendReal(r.Libraries, p.Dir)
	r.Write = appendReal(r.Write, tmp)
	for _, tool := range p.Tools {
		var path string
		if strings.ContainsRune(tool, '/') {
			path, _ = expand(tool, lookup)
		} else {
			path = lookPath(tool, lookup("PATH"))
		}
		if path == "" {
			continue // the plugin reports a missing tool better than tug can
		}
		addProgram(&r, path, lookup, 0)
		// Tools often read files installed beside them.
		r.Read = appendReal(r.Read, filepath.Dir(realPath(path)))
	}
	for _, list := range []struct {
		from []string
		to   *[]string
	}{{p.Read, &r.Read}, {p.Write, &r.Write}, {p.Sockets, &r.Sockets}} {
		for _, entry := range list.from {
			expanded, ok := expand(entry, lookup)
			if !ok {
				continue
			}
			// A variable such as KUBECONFIG may hold a list of paths.
			for _, path := range filepath.SplitList(expanded) {
				if filepath.IsAbs(path) {
					*list.to = appendReal(*list.to, path)
				}
			}
		}
	}
	for _, name := range []string{"SSL_CERT_FILE", "SSL_CERT_DIR"} {
		for _, path := range filepath.SplitList(lookup(name)) {
			if filepath.IsAbs(path) {
				r.Read = appendReal(r.Read, path)
			}
		}
	}
	r.Ports = append(slices.Clone(defaultPorts), p.Ports...)
	for _, name := range proxyVars {
		if port := proxyPort(lookup(name)); port != 0 {
			r.Ports = append(r.Ports, port)
		}
	}
	addSystem(&r)
	r.dedupe()
	return r
}

// dedupe sorts every list and drops repeats, so equal policies give equal
// rules.
func (r *rules) dedupe() {
	for _, list := range []*[]string{&r.Programs, &r.ProgramDirs, &r.Libraries, &r.Read, &r.Write, &r.WritePrefixes, &r.Sockets} {
		slices.Sort(*list)
		*list = slices.Compact(*list)
	}
	slices.Sort(r.Ports)
	r.Ports = slices.Compact(r.Ports)
}

// addProgram allows the executable at path and, when it is a script, the
// interpreter its #! line names, following /usr/bin/env to the program it
// runs.
func addProgram(r *rules, path string, lookup func(string) string, depth int) {
	r.Programs = appendReal(r.Programs, path)
	if depth > 2 {
		return
	}
	interp, args := shebang(path)
	if interp == "" {
		return
	}
	addProgram(r, interp, lookup, depth+1)
	if filepath.Base(interp) != "env" {
		return
	}
	for _, a := range args {
		if strings.HasPrefix(a, "-") || strings.Contains(a, "=") {
			continue // env's options and variable assignments
		}
		target := a
		if !strings.ContainsRune(a, '/') {
			target = lookPath(a, lookup("PATH"))
		}
		if target != "" {
			addProgram(r, target, lookup, depth+1)
		}
		return
	}
}

// shebang returns the interpreter and arguments of a script's #! line, or
// nothing for other files.
func shebang(path string) (string, []string) {
	f, err := os.Open(path)
	if err != nil {
		return "", nil
	}
	defer f.Close()
	line, err := bufio.NewReader(f).ReadSlice('\n')
	if err != nil && len(line) == 0 {
		return "", nil
	}
	rest, ok := bytes.CutPrefix(line, []byte("#!"))
	if !ok {
		return "", nil
	}
	fields := strings.Fields(string(rest))
	if len(fields) == 0 || !filepath.IsAbs(fields[0]) {
		return "", nil
	}
	return fields[0], fields[1:]
}

// expand resolves ~ and environment variables in a policy path with the
// plugin's environment. It reports false when a variable is unset or the
// result is not absolute.
func expand(path string, lookup func(string) string) (string, bool) {
	if path == "~" || strings.HasPrefix(path, "~/") {
		home := lookup("HOME")
		if home == "" {
			return "", false
		}
		path = home + path[1:]
	}
	unset := false
	path = os.Expand(path, func(name string) string {
		v := lookup(name)
		if v == "" {
			unset = true
		}
		return v
	})
	if unset || !filepath.IsAbs(path) {
		return "", false
	}
	return filepath.Clean(path), true
}

// lookPath finds an executable called name in the directories of pathList,
// as a shell would; exec.LookPath would search tug's PATH instead of the
// plugin's.
func lookPath(name, pathList string) string {
	for _, dir := range filepath.SplitList(pathList) {
		if !filepath.IsAbs(dir) {
			continue // a relative entry depends on the working directory
		}
		path := filepath.Join(dir, name)
		if fi, err := os.Stat(path); err == nil && fi.Mode().IsRegular() && fi.Mode()&0o111 != 0 {
			return path
		}
	}
	return ""
}

// proxyPort returns the port of a proxy URL such as http://proxy:3128, or
// zero when there is none.
func proxyPort(v string) uint16 {
	if v == "" {
		return 0
	}
	if !strings.Contains(v, "://") {
		v = "http://" + v
	}
	u, err := url.Parse(v)
	if err != nil || u.Host == "" {
		return 0
	}
	if p := u.Port(); p != "" {
		n, err := strconv.ParseUint(p, 10, 16)
		if err != nil {
			return 0
		}
		return uint16(n)
	}
	switch u.Scheme {
	case "https":
		return 443
	case "http":
		return 80
	case "socks5", "socks5h":
		return 1080
	}
	return 0
}

// appendReal appends path and, when it is reached through a link, the real
// path it leads to, which is what macOS's sandbox matches.
func appendReal(list []string, path string) []string {
	if path == "" {
		return list
	}
	list = append(list, filepath.Clean(path))
	if resolved := realPath(path); resolved != filepath.Clean(path) {
		list = append(list, resolved)
	}
	return list
}

// realPath resolves links in path, or returns it cleaned when it does not
// exist.
func realPath(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	return filepath.Clean(path)
}

// lookupFunc returns a function that reads a variable from env, the way
// os.Getenv reads the process's own.
func lookupFunc(env []string) func(string) string {
	vars := make(map[string]string, len(env))
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok {
			vars[k] = v // later entries win, as they do for exec
		}
	}
	return func(name string) string { return vars[name] }
}
