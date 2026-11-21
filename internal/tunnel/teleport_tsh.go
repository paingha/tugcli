package tunnel

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"k8s.io/client-go/tools/clientcmd"
)

func init() {
	builders["tsh"] = newTsh
	endpointKinds = append(endpointKinds, "tsh")
}

// tshInstallHint says where to get tsh.
const tshInstallHint = "install tsh, Teleport's client, from https://goteleport.com/download/client-tools/"

// tshStartTimeout bounds how long tsh proxy kube may take to start, which
// includes fetching certificates from Teleport.
const tshStartTimeout = time.Minute

// tshKubeconfigEnv names the file tsh proxy kube writes its kubeconfig to.
const tshKubeconfigEnv = "TELEPORT_KUBECONFIG"

// newTsh reads tsh://<proxy>/<teleport cluster>/<kube cluster> and runs
// tsh proxy kube for that cluster. tsh's local proxy holds the Teleport
// certificate, upgrades connections through a load balancer when it has
// to, and uses a hardware key when the profile needs one. tug passes
// connections through its forward proxy, and the endpoint, CA and client
// certificate tsh writes into its kubeconfig replace the cluster's.
func newTsh(ctx context.Context, u *url.URL, p *Pool) (opened, error) {
	cluster, kube, err := tshClusters(u)
	if err != nil {
		return opened{}, err
	}
	dir, err := os.MkdirTemp("", "tug-tsh-")
	if err != nil {
		return opened{}, fmt.Errorf("tunnel %s: %w", u.Redacted(), err)
	}
	kubeconfig := filepath.Join(dir, "kubeconfig")
	log := p.log.With("tunnel", u.Redacted())
	out := &tshOutput{log: log}
	cmd := exec.Command("tsh", "proxy", "kube", kube, "--cluster="+cluster, "--proxy="+u.Host)
	cmd.Env = append(os.Environ(),
		tshKubeconfigEnv+"="+kubeconfig,
		// tsh copies the default kubeconfig into its own; point it at a file
		// that does not exist, so that nothing else ends up there.
		"KUBECONFIG="+filepath.Join(dir, "none"),
	)
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		_ = os.RemoveAll(dir)
		if errors.Is(err, exec.ErrNotFound) {
			return opened{}, fmt.Errorf("tunnel %s: tsh is not installed or not on PATH; %s", u.Redacted(), tshInstallHint)
		}
		return opened{}, fmt.Errorf("tunnel %s: starting tsh proxy kube: %w", u.Redacted(), err)
	}
	t := &tshProxy{cmd: cmd, dir: dir, done: make(chan struct{})}
	go func() {
		err := cmd.Wait()
		log.Debug("tsh proxy kube exited", "err", err)
		close(t.done)
	}()

	proxyURL, ep, err := t.wait(ctx, kubeconfig)
	if err != nil {
		_ = t.Close()
		if msg := out.message(); msg != "" {
			err = fmt.Errorf("%w: %s", err, msg)
		}
		return opened{}, fmt.Errorf("tunnel %s: %w", u.Redacted(), err)
	}
	log.Debug("tsh proxy kube started", "forward proxy", proxyURL.Host, "server", ep.Server)
	dial := func(ctx context.Context, _, address string) (net.Conn, error) {
		return dialConnect(ctx, proxyURL, address)
	}
	return opened{dial: dial, closer: t, endpoint: ep, done: t.done}, nil
}

// tshClusters reads the Teleport and Kubernetes cluster names from the path.
func tshClusters(u *url.URL) (cluster, kube string, err error) {
	parts := strings.Split(strings.Trim(u.EscapedPath(), "/"), "/")
	if len(parts) == 2 {
		cluster, err = url.PathUnescape(parts[0])
		if err == nil {
			kube, err = url.PathUnescape(parts[1])
		}
	}
	if err != nil || cluster == "" || kube == "" {
		return "", "", fmt.Errorf("tunnel %s: name the Teleport and Kubernetes clusters, as in tsh://teleport.example.com:443/<Teleport cluster>/<Kubernetes cluster>", u.Redacted())
	}
	return cluster, kube, nil
}

// tshProxy is a running tsh proxy kube.
type tshProxy struct {
	cmd  *exec.Cmd
	dir  string
	done chan struct{}
	once sync.Once
}

// wait waits for tsh to write its kubeconfig, and reads the forward proxy
// and the endpoint from it.
func (t *tshProxy) wait(ctx context.Context, path string) (*url.URL, *Endpoint, error) {
	ctx, cancel := context.WithTimeout(ctx, tshStartTimeout)
	defer cancel()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		// tsh writes the file in place, so a read may see part of it;
		// keep trying until it parses.
		if proxyURL, ep, err := readTshKubeconfig(path); err == nil {
			return proxyURL, ep, nil
		}
		select {
		case <-t.done:
			return nil, nil, errors.New("tsh proxy kube exited before it started")
		case <-ctx.Done():
			return nil, nil, fmt.Errorf("waiting for tsh proxy kube to start: %w", ctx.Err())
		case <-tick.C:
		}
	}
}

// Close stops tsh and removes its kubeconfig.
func (t *tshProxy) Close() error {
	var err error
	t.once.Do(func() {
		if kerr := t.cmd.Process.Kill(); kerr != nil && !errors.Is(kerr, os.ErrProcessDone) {
			err = kerr
		}
		select {
		case <-t.done:
		case <-time.After(5 * time.Second):
		}
		err = errors.Join(err, os.RemoveAll(t.dir))
	})
	return err
}

// readTshKubeconfig reads the kubeconfig tsh proxy kube writes: its current
// context is the cluster, whose proxy-url is tsh's forward proxy, and whose
// server, CA, server name and client certificate belong to tsh's local
// proxy.
func readTshKubeconfig(path string) (*url.URL, *Endpoint, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	cfg, err := clientcmd.Load(data)
	if err != nil {
		return nil, nil, err
	}
	kctx, ok := cfg.Contexts[cfg.CurrentContext]
	if !ok {
		return nil, nil, errors.New("tsh's kubeconfig has no current context")
	}
	cl, auth := cfg.Clusters[kctx.Cluster], cfg.AuthInfos[kctx.AuthInfo]
	if cl == nil || auth == nil {
		return nil, nil, errors.New("tsh's kubeconfig has no cluster or user for its current context")
	}
	proxyURL, err := url.Parse(cl.ProxyURL)
	if err != nil || proxyURL.Scheme != "http" || proxyURL.Host == "" {
		return nil, nil, fmt.Errorf("tsh's kubeconfig names %q as its local proxy, which is not an http URL", cl.ProxyURL)
	}
	ep := &Endpoint{
		Server:     cl.Server,
		CAData:     cl.CertificateAuthorityData,
		ServerName: cl.TLSServerName,
		CertData:   auth.ClientCertificateData,
		KeyData:    auth.ClientKeyData,
	}
	if ep.Server == "" || len(ep.CAData) == 0 || len(ep.CertData) == 0 || len(ep.KeyData) == 0 {
		return nil, nil, errors.New("tsh's kubeconfig lacks the local proxy's server, CA or client certificate")
	}
	return proxyURL, ep, nil
}

// tshOutput logs what tsh prints, a line at a time, and keeps the reason it
// gives for failing.
type tshOutput struct {
	log *slog.Logger

	mu      sync.Mutex
	partial []byte
	last    string
	errLine string
}

func (o *tshOutput) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.partial = append(o.partial, p...)
	for {
		i := bytes.IndexByte(o.partial, '\n')
		if i < 0 {
			break
		}
		line := strings.TrimSpace(string(o.partial[:i]))
		o.partial = o.partial[i+1:]
		if line == "" {
			continue
		}
		o.log.Debug("tsh", "line", line)
		o.last = line
		if msg, ok := strings.CutPrefix(line, "ERROR: "); ok {
			o.errLine = msg
		}
	}
	return len(p), nil
}

// message returns tsh's ERROR line, or else the last line it printed.
func (o *tshOutput) message() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.errLine != "" {
		return o.errLine
	}
	return o.last
}

var _ io.Writer = (*tshOutput)(nil)
