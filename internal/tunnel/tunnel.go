// Package tunnel reaches API servers that are not directly reachable:
// through an SSH jump host, a SOCKS or HTTP proxy, an AWS SSM session or
// Google IAP. Every tunnel is handed to Kubernetes clients as a proxy URL,
// because client-go's exec, attach and port-forward upgrades honour a proxy
// but not a custom dialer. Tunnels that are not proxies already are served
// as a SOCKS5 proxy on a loopback port, guarded by random credentials.
package tunnel

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
)

// DialFunc opens a connection to address through a tunnel.
type DialFunc func(ctx context.Context, network, address string) (net.Conn, error)

// Kinds are the tunnel URL schemes tug understands.
var Kinds = []string{"ssh", "socks5", "socks5h", "http", "https", "ssm", "iap"}

// proxyKinds are passed to clients as they are.
var proxyKinds = []string{"socks5", "socks5h", "http", "https"}

// Parse checks a tunnel spec, such as ssh://admin@bastion.example.com or
// ssm://i-0123456789abcdef0?region=eu-west-1.
func Parse(spec string) (*url.URL, error) {
	u, err := url.Parse(spec)
	if err != nil {
		return nil, fmt.Errorf("tunnel %q: %w", spec, err)
	}
	if !slices.Contains(Kinds, u.Scheme) {
		return nil, fmt.Errorf("tunnel %q: the scheme must be one of %s", spec, strings.Join(Kinds, ", "))
	}
	if _, ok := builders[u.Scheme]; !ok && !slices.Contains(proxyKinds, u.Scheme) {
		return nil, fmt.Errorf("tunnel %q: %s tunnels are not available in this build", spec, u.Scheme)
	}
	if u.Host == "" && u.Scheme != "iap" {
		return nil, fmt.Errorf("tunnel %q: name the host to go through", spec)
	}
	return u, nil
}

// Check is Parse for a spec tug keeps in its catalog, which holds no
// secrets: the spec must not carry a password.
func Check(spec string) (*url.URL, error) {
	u, err := Parse(spec)
	if err != nil {
		return nil, err
	}
	if _, ok := u.User.Password(); ok {
		return nil, fmt.Errorf("tunnel %s: leave the password out, since tug keeps no secrets in its catalog; use SSH keys, or a proxy that needs no password", u.Redacted())
	}
	return u, nil
}

// Redacted returns spec without any password it carries, for display.
func Redacted(spec string) string {
	u, err := url.Parse(spec)
	if err != nil {
		return spec
	}
	return u.Redacted()
}

// builder makes the dial function of one tunnel kind.
type builder func(u *url.URL, p *Pool) (DialFunc, io.Closer, error)

// builders holds the kinds that need a dialer of their own; ssm and iap
// register themselves from their files.
var builders = map[string]builder{
	"ssh": newSSH,
}

// Pool keeps tunnels open for the life of a process, so a command or tugd
// reuses one SSH connection or local proxy per tunnel.
type Pool struct {
	log    *slog.Logger
	home   string
	getenv func(string) string

	mu      sync.Mutex
	proxies map[string]*localProxy
	closed  bool
}

// Option configures a Pool.
type Option func(*Pool)

// WithLogger sets where the pool logs.
func WithLogger(l *slog.Logger) Option { return func(p *Pool) { p.log = l } }

// WithHome sets the home directory, where ~/.ssh lives.
func WithHome(dir string) Option { return func(p *Pool) { p.home = dir } }

// WithGetenv sets how the pool reads the environment, such as SSH_AUTH_SOCK.
func WithGetenv(getenv func(string) string) Option { return func(p *Pool) { p.getenv = getenv } }

// NewPool returns an empty pool.
func NewPool(opts ...Option) *Pool {
	home, _ := os.UserHomeDir()
	p := &Pool{
		log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		home:    home,
		getenv:  os.Getenv,
		proxies: map[string]*localProxy{},
	}
	for _, o := range opts {
		o(p)
	}
	return p
}

// ProxyURL returns the proxy a Kubernetes client should use to go through
// spec. SOCKS and HTTP tunnels are returned as they are; the others are
// opened, once per pool, behind a local SOCKS5 proxy.
func (p *Pool) ProxyURL(ctx context.Context, spec string) (*url.URL, error) {
	u, err := Parse(spec)
	if err != nil {
		return nil, err
	}
	if slices.Contains(proxyKinds, u.Scheme) {
		return u, nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, errors.New("the tunnel pool is closed")
	}
	if lp, ok := p.proxies[spec]; ok {
		return lp.url(), nil
	}
	dial, closer, err := builders[u.Scheme](u, p)
	if err != nil {
		return nil, err
	}
	lp, err := startLocalProxy(dial, closer, p.log.With("tunnel", Redacted(spec)))
	if err != nil {
		if closer != nil {
			_ = closer.Close()
		}
		return nil, err
	}
	p.proxies[spec] = lp
	return lp.url(), nil
}

// Dial opens one connection to address through spec, for checking a tunnel.
func (p *Pool) Dial(ctx context.Context, spec, address string) (net.Conn, error) {
	u, err := p.ProxyURL(ctx, spec)
	if err != nil {
		return nil, err
	}
	return dialProxy(ctx, u, address)
}

// Close stops every tunnel the pool opened.
func (p *Pool) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	var errs []error
	for spec, lp := range p.proxies {
		errs = append(errs, lp.close())
		delete(p.proxies, spec)
	}
	return errors.Join(errs...)
}
