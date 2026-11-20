package tunnel

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"
)

// sshKeepAlive is how often an idle SSH connection is checked, so a dead
// jump host is noticed before the next request needs it.
const sshKeepAlive = 30 * time.Second

// sshTunnel keeps one connection to a jump host and opens a channel through
// it for each connection, reconnecting when the jump host drops it.
type sshTunnel struct {
	addr       string
	config     *ssh.ClientConfig
	closeAgent func()

	mu     sync.Mutex
	client *ssh.Client
	stop   chan struct{}
	wg     sync.WaitGroup
	closed bool
}

// newSSH reads ssh://[user@]host[:port][?identity=<key>&known_hosts=<file>].
// Keys come from ssh-agent (SSH_AUTH_SOCK) and the identity file, or the
// usual ~/.ssh/id_* files. The host key must already be in known_hosts:
// tug never trusts a jump host it has not seen.
func newSSH(u *url.URL, p *Pool) (DialFunc, io.Closer, error) {
	name := u.User.Username()
	if name == "" {
		cur, err := user.Current()
		if err != nil {
			return nil, nil, fmt.Errorf("tunnel %s: name the SSH user, as in ssh://user@host: %w", u.Redacted(), err)
		}
		name = filepath.Base(cur.Username) // DOMAIN\user on Windows
	}
	port := u.Port()
	if port == "" {
		port = "22"
	}
	addr := net.JoinHostPort(u.Hostname(), port)
	q := u.Query()

	signers, closeAgent := p.sshSigners(q["identity"])
	if len(signers) == 0 {
		closeAgent()
		return nil, nil, fmt.Errorf("tunnel %s: no SSH key to sign in with; add one to ssh-agent, or name it with ?identity=<path>", u.Redacted())
	}
	files := q["known_hosts"]
	if len(files) == 0 {
		files = []string{filepath.Join(p.home, ".ssh", "known_hosts")}
	}
	hostKeys, err := knownhosts.New(files...)
	if err != nil {
		closeAgent()
		return nil, nil, fmt.Errorf("tunnel %s: reading known hosts: %w; connect once with ssh to add the jump host's key", u.Redacted(), err)
	}
	t := &sshTunnel{
		addr: addr,
		config: &ssh.ClientConfig{
			User:              name,
			Auth:              []ssh.AuthMethod{ssh.PublicKeys(signers...)},
			HostKeyCallback:   friendlyHostKeys(hostKeys, addr),
			HostKeyAlgorithms: knownAlgorithms(hostKeys, addr),
			Timeout:           15 * time.Second,
		},
		closeAgent: closeAgent,
		stop:       make(chan struct{}),
	}
	return t.dial, t, nil
}

// sshSigners gathers keys from ssh-agent and key files. Keys protected by a
// passphrase are skipped: tug cannot ask for one here, so they belong in the
// agent.
func (p *Pool) sshSigners(identities []string) ([]ssh.Signer, func()) {
	var signers []ssh.Signer
	closeAgent := func() {}
	if sock := p.getenv("SSH_AUTH_SOCK"); sock != "" && runtime.GOOS != "windows" {
		if c, err := net.Dial("unix", sock); err == nil {
			if s, err := agent.NewClient(c).Signers(); err == nil {
				signers = append(signers, s...)
			}
			closeAgent = func() { _ = c.Close() }
		} else {
			p.log.Debug("ssh-agent is not reachable", "err", err)
		}
	}
	if len(identities) == 0 {
		for _, n := range []string{"id_ed25519", "id_ecdsa", "id_rsa"} {
			identities = append(identities, filepath.Join(p.home, ".ssh", n))
		}
	}
	for _, path := range identities {
		b, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		s, err := ssh.ParsePrivateKey(b)
		var missing *ssh.PassphraseMissingError
		if errors.As(err, &missing) {
			p.log.Debug("skipping a key with a passphrase; add it to ssh-agent", "key", path)
			continue
		}
		if err != nil {
			p.log.Debug("skipping an unreadable key", "key", path, "err", err)
			continue
		}
		signers = append(signers, s)
	}
	return signers, closeAgent
}

// friendlyHostKeys explains an unknown or changed host key.
func friendlyHostKeys(cb ssh.HostKeyCallback, addr string) ssh.HostKeyCallback {
	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		err := cb(hostname, remote, key)
		var ke *knownhosts.KeyError
		switch {
		case errors.As(err, &ke) && len(ke.Want) == 0:
			return fmt.Errorf("the host key of %s is not in known_hosts; connect once with ssh to check it and add it", addr)
		case errors.As(err, &ke):
			return fmt.Errorf("the host key of %s does not match known_hosts; it may have been replaced, or someone is in the way", addr)
		}
		return err
	}
}

// knownAlgorithms lists the key types known_hosts holds for addr, so the
// jump host offers one of those rather than a type tug cannot check.
func knownAlgorithms(cb ssh.HostKeyCallback, addr string) []string {
	probe, err := ssh.NewPublicKey(ed25519.PublicKey(make([]byte, ed25519.PublicKeySize)))
	if err != nil {
		return nil
	}
	var ke *knownhosts.KeyError
	if err := cb(addr, &net.TCPAddr{}, probe); !errors.As(err, &ke) {
		return nil
	}
	var algos []string
	for _, k := range ke.Want {
		switch t := k.Key.Type(); t {
		case ssh.KeyAlgoRSA:
			algos = append(algos, ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256, ssh.KeyAlgoRSA)
		default:
			algos = append(algos, t)
		}
	}
	return algos
}

func (t *sshTunnel) dial(ctx context.Context, network, address string) (net.Conn, error) {
	for attempt := 0; ; attempt++ {
		c, err := t.connect(ctx)
		if err != nil {
			return nil, err
		}
		conn, err := c.DialContext(ctx, network, address)
		if err == nil {
			return conn, nil
		}
		// A dropped jump host connection fails every channel; reconnect once.
		t.forget(c)
		if attempt == 1 || ctx.Err() != nil {
			return nil, fmt.Errorf("opening %s through %s: %w", address, t.addr, err)
		}
	}
}

func (t *sshTunnel) connect(ctx context.Context) (*ssh.Client, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return nil, errors.New("the SSH tunnel is closed")
	}
	if t.client != nil {
		return t.client, nil
	}
	var d net.Dialer
	raw, err := d.DialContext(ctx, "tcp", t.addr)
	if err != nil {
		return nil, fmt.Errorf("reaching the jump host %s: %w", t.addr, err)
	}
	sc, chans, reqs, err := ssh.NewClientConn(raw, t.addr, t.config)
	if err != nil {
		raw.Close()
		return nil, fmt.Errorf("signing in to the jump host %s as %s: %w", t.addr, t.config.User, err)
	}
	c := ssh.NewClient(sc, chans, reqs)
	t.client = c
	t.wg.Go(func() { t.keepAlive(c) })
	return c, nil
}

func (t *sshTunnel) keepAlive(c *ssh.Client) {
	tick := time.NewTicker(sshKeepAlive)
	defer tick.Stop()
	for {
		select {
		case <-t.stop:
			return
		case <-tick.C:
			if _, _, err := c.SendRequest("keepalive@openssh.com", true, nil); err != nil {
				t.forget(c)
				return
			}
		}
	}
}

// forget drops c so the next dial reconnects.
func (t *sshTunnel) forget(c *ssh.Client) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.client == c {
		t.client = nil
	}
	_ = c.Close()
}

// Close ends the connection to the jump host.
func (t *sshTunnel) Close() error {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil
	}
	t.closed = true
	close(t.stop)
	c := t.client
	t.client = nil
	t.mu.Unlock()
	var err error
	if c != nil {
		err = c.Close()
	}
	t.wg.Wait()
	t.closeAgent()
	return err
}
