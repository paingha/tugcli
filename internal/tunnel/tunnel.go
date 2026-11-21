// Package tunnel reaches API servers that are not directly reachable:
// through an SSH jump host, a SOCKS or HTTP proxy, an AWS SSM session,
// Google IAP or Teleport's tsh proxy kube. Every tunnel is handed to
// Kubernetes clients as a proxy URL, because client-go's exec, attach and
// port-forward upgrades honour a proxy but not a custom dialer. Tunnels that
// are not proxies already are served as a SOCKS5 proxy on a loopback port,
// guarded by random credentials. A tunnel that ends in a local proxy holding
// the cluster's credential, as tsh's does, also hands out the endpoint
// clients must use there, as a Route.
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
var Kinds = []string{"ssh", "socks5", "socks5h", "http", "https", "ssm", "iap", "tsh"}

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

// Endpoint is an API server address and TLS identity that a tunnel hands
// out in place of the cluster's own. A tunnel has one when it ends in a
// local proxy, such as tsh's, that terminates TLS with a certificate
// authority of its own, accepts only a client certificate it issued, and
// signs in to the cluster itself. Both are made each time the local proxy
// starts, so they cannot be kept in the catalog.
type Endpoint struct {
	// Server is the URL to send requests to.
	Server string
	// CAData and ServerName verify the local proxy's certificate.
	CAData     []byte
	ServerName string
	// CertData and KeyData are the client certificate the local proxy
	// accepts, in PEM. They take the place of the cluster's credential.
	CertData, KeyData []byte
}

// Route is how a Kubernetes client reaches an API server through a tunnel.
type Route struct {
	// Proxy is the proxy URL to give the client.
	Proxy *url.URL
	// Endpoint, when set, replaces the cluster's server, CA, server name and
	// credential.
	Endpoint *Endpoint
}

// opened is a tunnel that a builder has opened.
type opened struct {
	dial DialFunc
	// closer, when set, is closed with the tunnel.
	closer io.Closer
	// endpoint is set for tunnels that end in a local proxy.
	endpoint *Endpoint
	// done, when set, is closed if the tunnel ends on its own, such as a
	// local proxy process that exits; the pool then opens it again.
	done <-chan struct{}
}

// builder opens one tunnel of a kind. ctx bounds the opening only; the
// tunnel stays open until it is closed.
type builder func(ctx context.Context, u *url.URL, p *Pool) (opened, error)

// builders holds the kinds that need a dialer of their own; ssm, iap and tsh
// register themselves from their files.
var builders = map[string]builder{
	"ssh": dialOnly(newSSH),
}

// endpointKinds are the kinds whose tunnels end in a local proxy that
// replaces the cluster's endpoint and credential.
var endpointKinds []string

// ReplacesEndpoint reports whether spec's tunnel ends in a local proxy that
// replaces the cluster's endpoint and credential, so that the cluster's own
// CA and credential are not what clients use.
func ReplacesEndpoint(spec string) bool {
	u, err := url.Parse(spec)
	return err == nil && slices.Contains(endpointKinds, u.Scheme)
}

// dialOnly adapts the builder of a kind whose tunnels only dial.
func dialOnly(open func(u *url.URL, p *Pool) (DialFunc, io.Closer, error)) builder {
	return func(_ context.Context, u *url.URL, p *Pool) (opened, error) {
		dial, closer, err := open(u, p)
		return opened{dial: dial, closer: closer}, err
	}
}

// Pool keeps tunnels open for the life of a process, so a command or tugd
// reuses one SSH connection or local proxy per tunnel.
type Pool struct {
	log    *slog.Logger
	home   string
	getenv func(string) string

	mu      sync.Mutex
	tunnels map[string]*poolEntry
	closed  bool
}

// poolEntry is a tunnel the pool has opened, or is opening. Its fields are
// set under the pool's lock before ready is closed.
type poolEntry struct {
	ready    chan struct{}
	err      error
	lp       *localProxy
	endpoint *Endpoint
	done     <-chan struct{}
}

// ended reports whether an open tunnel has ended on its own.
func (e *poolEntry) ended() bool {
	select {
	case <-e.ready:
	default:
		return false
	}
	if e.err != nil || e.done == nil {
		return false
	}
	select {
	case <-e.done:
		return true
	default:
		return false
	}
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
		tunnels: map[string]*poolEntry{},
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
	r, err := p.Route(ctx, spec)
	return r.Proxy, err
}

// Route returns how a Kubernetes client reaches its cluster through spec:
// the proxy to use, and the endpoint that replaces the cluster's when the
// tunnel ends in a local proxy. The pool opens each tunnel once; callers
// that ask while it opens wait for it, and a tunnel that has ended on its
// own is opened again.
func (p *Pool) Route(ctx context.Context, spec string) (Route, error) {
	u, err := Parse(spec)
	if err != nil {
		return Route{}, err
	}
	if slices.Contains(proxyKinds, u.Scheme) {
		return Route{Proxy: u}, nil
	}
	for {
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			return Route{}, errors.New("the tunnel pool is closed")
		}
		e, ok := p.tunnels[spec]
		if ok && e.ended() {
			delete(p.tunnels, spec)
			p.mu.Unlock()
			p.log.Info("the tunnel ended; opening it again", "tunnel", Redacted(spec))
			_ = e.lp.close()
			continue
		}
		if !ok {
			e = &poolEntry{ready: make(chan struct{})}
			p.tunnels[spec] = e
			p.mu.Unlock()
			p.open(ctx, u, spec, e)
		} else {
			p.mu.Unlock()
		}
		select {
		case <-e.ready:
		case <-ctx.Done():
			return Route{}, ctx.Err()
		}
		if e.err != nil {
			return Route{}, e.err
		}
		return Route{Proxy: e.lp.url(), Endpoint: e.endpoint}, nil
	}
}

// open opens spec's tunnel behind a local SOCKS5 proxy and fills in e.
func (p *Pool) open(ctx context.Context, u *url.URL, spec string, e *poolEntry) {
	defer close(e.ready)
	o, err := builders[u.Scheme](ctx, u, p)
	var lp *localProxy
	if err == nil {
		if lp, err = startLocalProxy(o.dial, o.closer, p.log.With("tunnel", Redacted(spec))); err != nil && o.closer != nil {
			_ = o.closer.Close()
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if err == nil && p.closed {
		_ = lp.close()
		err = errors.New("the tunnel pool is closed")
	}
	if err != nil {
		e.err = err
		if p.tunnels[spec] == e {
			delete(p.tunnels, spec)
		}
		return
	}
	e.lp, e.endpoint, e.done = lp, o.endpoint, o.done
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
	for spec, e := range p.tunnels {
		// A tunnel still opening is closed by open, which sees p.closed.
		if e.lp != nil {
			errs = append(errs, e.lp.close())
		}
		delete(p.tunnels, spec)
	}
	return errors.Join(errs...)
}
