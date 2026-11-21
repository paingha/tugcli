package plugin

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
)

// hostPattern is one spec.network entry, as a WebAssembly plugin is held to
// it: a host name or IP address, or "*." and a domain for every name under
// that domain (but not the domain itself), with an optional port. Without a
// port it allows 443, the https port, only.
type hostPattern struct {
	host     string // lower case; for a wildcard, the domain after "*."
	wildcard bool
	port     string
}

// parseHostPattern reads a spec.network entry such as api.example.com,
// *.example.com, api.example.com:8443 or [2001:db8::1]:443.
func parseHostPattern(s string) (hostPattern, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return hostPattern{}, errors.New("it is empty")
	}
	if strings.Contains(s, "/") {
		return hostPattern{}, errors.New("write the host alone, without a scheme or path")
	}
	host, port := s, "443"
	switch {
	case strings.HasPrefix(s, "["):
		h, p, err := net.SplitHostPort(s)
		if err != nil {
			h, p = strings.TrimSuffix(strings.TrimPrefix(s, "["), "]"), "443"
		}
		host, port = h, p
	case strings.Count(s, ":") == 1:
		h, p, err := net.SplitHostPort(s)
		if err != nil {
			return hostPattern{}, err
		}
		host, port = h, p
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return hostPattern{}, fmt.Errorf("port %q is not a number from 1 to 65535", port)
	}
	p := hostPattern{host: host, port: port}
	if rest, ok := strings.CutPrefix(host, "*."); ok {
		p.host, p.wildcard = rest, true
	}
	if ip := net.ParseIP(p.host); ip != nil {
		if p.wildcard {
			return hostPattern{}, errors.New("a wildcard needs a domain name, not an IP address")
		}
		p.host = ip.String()
		return p, nil
	}
	if !validHostName(p.host) {
		return hostPattern{}, errors.New("it is not a host name such as api.example.com")
	}
	return p, nil
}

// validHostName reports whether s is a DNS name of letters, digits and
// dashes, as in a URL.
func validHostName(s string) bool {
	if s == "" || len(s) > 253 {
		return false
	}
	for label := range strings.SplitSeq(s, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, r := range label {
			if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
				return false
			}
		}
	}
	return true
}

// String writes the pattern as a manifest would.
func (p hostPattern) String() string {
	host := p.host
	if p.wildcard {
		host = "*." + host
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if p.port == "443" {
		return host
	}
	return host + ":" + p.port
}

func (p hostPattern) allows(host, port string) bool {
	if port != p.port {
		return false
	}
	if p.wildcard {
		return strings.HasSuffix(host, "."+p.host)
	}
	return host == p.host
}

// allowList is a WebAssembly plugin's spec.network.
type allowList []hostPattern

func newAllowList(entries []string) (allowList, error) {
	var l allowList
	var errs []error
	for _, e := range entries {
		p, err := parseHostPattern(e)
		if err != nil {
			errs = append(errs, fmt.Errorf("spec.network entry %q: %w", e, err))
			continue
		}
		l = append(l, p)
	}
	return l, errors.Join(errs...)
}

// check returns why u may not be requested, or nil when it may: it must be
// https, to a host and port the list allows.
func (l allowList) check(u *url.URL) error {
	if u.Scheme != "https" {
		return fmt.Errorf("%s is not https; WebAssembly plugins may use https only", u.Redacted())
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if host == "" {
		return fmt.Errorf("%s has no host", u.Redacted())
	}
	if ip := net.ParseIP(host); ip != nil {
		host = ip.String()
	}
	port := u.Port()
	if port == "" {
		port = "443"
	}
	for _, p := range l {
		if p.allows(host, port) {
			return nil
		}
	}
	shown := host
	if port != "443" {
		shown = net.JoinHostPort(host, port)
	}
	if len(l) == 0 {
		return fmt.Errorf("%s is not in the plugin's spec.network, which lists no hosts", shown)
	}
	allowed := make([]string, len(l))
	for i, p := range l {
		allowed[i] = p.String()
	}
	return fmt.Errorf("%s is not in the plugin's spec.network (%s)", shown, strings.Join(allowed, ", "))
}
