// Package teleport is tug's first-party provider plugin for Kubernetes
// clusters behind Teleport. It works through tsh, which the user has already
// signed in with: clusters come from tsh kube ls, and credentials are
// delegated to tsh kube credentials, so Teleport's certificates stay with
// tsh. The endpoint, CA and TLS server name are the ones tsh kube login
// writes into a kubeconfig, worked out from the tsh profile. When kubectl
// cannot reach the proxy directly, each cluster is listed with a tsh://
// tunnel, through which tug runs tsh proxy kube.
package teleport

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base32"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"sigs.k8s.io/yaml"

	"github.com/paingha/tugcli/pkg/provider"
)

// Name and version of the plugin.
const (
	Name    = "teleport"
	Version = "0.1.1"
)

// Account settings.
const (
	// ConfigProxy is the Teleport proxy address, as for tsh --proxy. It
	// defaults to the proxy of the active tsh profile.
	ConfigProxy = "proxy"
	// ConfigCluster is the Teleport cluster, root or leaf. It defaults to
	// the tsh profile's current cluster.
	ConfigCluster = "cluster"
)

// InstallHint tells users where to get tsh.
const InstallHint = "install tsh, Teleport's client, from https://goteleport.com/download/client-tools/"

// HomeEnv moves tsh's profile directory from ~/.tsh.
const HomeEnv = "TELEPORT_HOME"

// execAPIVersion is the ExecCredential version tsh kube credentials prints,
// and the one tsh kube login writes into the kubeconfig.
const execAPIVersion = "client.authentication.k8s.io/v1beta1"

// Values tsh uses when it writes a kubeconfig. They come from lib/client,
// lib/defaults, lib/kube/relay and api/constants in
// github.com/gravitational/teleport.
const (
	// kubeListenPort is the proxy's Kubernetes port when the profile names
	// none.
	kubeListenPort = 3026
	// kubeSNIPrefix marks Kubernetes traffic to a proxy with TLS routing.
	kubeSNIPrefix = "kube-teleport-proxy-alpn."
	// apiDomain stands in for a proxy that has no host name.
	apiDomain = "teleport.cluster.local"
	// relaySNIPrefix and relaySNISuffix wrap the hash that tells a Teleport
	// relay which Kubernetes cluster a connection is for.
	relaySNIPrefix = "cluster-"
	relaySNISuffix = ".kuberelay." + apiDomain
)

// relayHashEncoding is the base32 alphabet of Teleport's relay server
// names: RFC 4648's extended hex alphabet, in lower case, without padding.
var relayHashEncoding = base32.NewEncoding("0123456789abcdefghijklmnopqrstuv").WithPadding(base32.NoPadding)

// Provider implements the plugin.
type Provider struct {
	// getenv reads the environment; tests replace it.
	getenv func(string) string
}

var _ provider.Provider = (*Provider)(nil)

// New returns the plugin.
func New() *Provider { return &Provider{getenv: os.Getenv} }

// Info implements provider.Provider.
func (p *Provider) Info() provider.Info {
	return provider.Info{
		Name:         Name,
		Version:      Version,
		Description:  "Kubernetes clusters behind Teleport, through tsh",
		Capabilities: provider.CapList | provider.CapCredential,
	}
}

// tsh output, trimmed to the fields the plugin uses.
type (
	// status is `tsh status --format=json`.
	status struct {
		Active *activeProfile `json:"active"`
	}
	activeProfile struct {
		ProxyURL string `json:"profile_url"`
		Username string `json:"username"`
		Cluster  string `json:"cluster"`
	}
	// kubeCluster is one entry of `tsh kube ls --format=json`.
	kubeCluster struct {
		Name   string            `json:"kube_cluster_name"`
		Labels map[string]string `json:"labels"`
	}
)

// profile is the part of tsh's profile file, <home>/<proxy host>.yaml, that
// decides what tsh kube login writes. tsh records these at tsh login from
// what the proxy advertises.
type profile struct {
	WebProxyAddr                  string `json:"web_proxy_addr"`
	KubeProxyAddr                 string `json:"kube_proxy_addr"`
	RelayAddr                     string `json:"relay_addr"`
	DefaultRelayAddr              string `json:"default_relay_addr"`
	Username                      string `json:"user"`
	TLSRoutingEnabled             bool   `json:"tls_routing_enabled"`
	TLSRoutingConnUpgradeRequired bool   `json:"tls_routing_conn_upgrade_required"`
	LoadAllCAs                    bool   `json:"load_all_cas"`
	PrivateKeyPolicy              string `json:"private_key_policy"`
}

// List returns the Kubernetes clusters of one Teleport cluster, as tsh kube
// ls shows them to the signed-in user.
func (p *Provider) List(ctx context.Context, req provider.ListRequest) ([]provider.Cluster, error) {
	proxy := req.Account.ConfigString(ConfigProxy)
	active, err := activeStatus(ctx, proxy)
	if err != nil {
		return nil, err
	}
	u, err := url.Parse(active.ProxyURL)
	if err != nil || u.Hostname() == "" {
		return nil, provider.Errorf(provider.Internal, "tsh status gave the proxy as %q, which is not a URL", active.ProxyURL)
	}
	home, err := p.home()
	if err != nil {
		return nil, err
	}
	// tsh names the profile, and its key directory, after the proxy's host.
	profileName := u.Hostname()
	prof, err := readProfile(home, profileName)
	if err != nil {
		return nil, err
	}
	if proxy == "" {
		// tsh kube login pins the credential command to the profile's proxy
		// with --proxy, so a later tsh login elsewhere does not break it.
		proxy = cmp.Or(prof.WebProxyAddr, u.Host)
	}
	cluster := cmp.Or(req.Account.ConfigString(ConfigCluster), active.Cluster, profileName)

	// Without a direct way in, tug reaches the clusters through tsh proxy
	// kube, named by the profile's own proxy address.
	localProxy := prof.needsLocalProxy()
	if prof.KubeProxyAddr == "" && prof.relayAddr() == "" {
		return nil, provider.Errorf(provider.Unavailable, "the tsh profile for %s has no Kubernetes proxy address", profileName).
			WithHint(fmt.Sprintf("if Kubernetes access has been enabled since you signed in, run `tsh login --proxy=%s`", proxy))
	}
	ca, err := rootCAs(filepath.Join(home, "keys", profileName), cmp.Or(prof.Username, active.Username), prof.LoadAllCAs, proxy)
	if err != nil {
		return nil, err
	}

	out, err := tsh(ctx, provider.Unavailable, "", "kube", "ls", "--format=json", "--proxy="+proxy, "--cluster="+cluster)
	if err != nil {
		return nil, err
	}
	var kubes []kubeCluster
	if err := json.Unmarshal(out, &kubes); err != nil {
		return nil, provider.Errorf(provider.Internal, "reading tsh kube ls: %v", err)
	}

	clusters := make([]provider.Cluster, 0, len(kubes))
	seen := map[string]bool{}
	for _, kc := range kubes {
		if kc.Name == "" {
			continue
		}
		if seen[kc.Name] {
			// tsh kube credentials picks a cluster by name, so it cannot
			// reach the second of two clusters with the same name.
			fmt.Fprintf(os.Stderr, "teleport: skipping a second Kubernetes cluster named %s in %s\n", kc.Name, cluster)
			continue
		}
		seen[kc.Name] = true
		server, serverName := prof.endpoint(cluster, kc.Name)
		cred := p.delegate(proxy, cluster, kc.Name)
		pc := provider.Cluster{
			ID:            cluster + "/" + kc.Name,
			Name:          kc.Name,
			Endpoints:     provider.Endpoints{Public: server},
			CAData:        ca,
			TLSServerName: serverName,
			Labels:        map[string]string{},
			// The credential is a command, not a secret. Returning it here
			// keeps the --proxy it was listed through.
			Credential: &cred,
		}
		if localProxy {
			pc.Tunnel = tshTunnel(cmp.Or(prof.WebProxyAddr, u.Host), cluster, kc.Name)
		}
		for k, v := range kc.Labels {
			// tug sets the region label from Region.
			if strings.EqualFold(k, "region") {
				pc.Region = v
				continue
			}
			pc.Labels[k] = v
		}
		pc.Labels["teleport-cluster"] = cluster
		clusters = append(clusters, pc)
	}
	return clusters, nil
}

// tshTunnel returns the tunnel that reaches a cluster through tsh proxy
// kube. tsh's local proxy then holds the Teleport certificate, and its own
// CA and client certificate take the place of the listed ones.
func tshTunnel(proxy, cluster, kube string) string {
	return "tsh://" + proxy + "/" + url.PathEscape(cluster) + "/" + url.PathEscape(kube)
}

// Credential returns the tsh kube credentials command for a listed cluster.
// It reads only tsh's profile files, so that an expired Teleport session
// reaches tsh, which can sign the user in again.
func (p *Provider) Credential(_ context.Context, req provider.CredentialRequest) (provider.Credential, error) {
	cluster, kube, ok := strings.Cut(req.ClusterID, "/")
	if !ok || cluster == "" || kube == "" {
		return provider.Credential{}, provider.Errorf(provider.NotFound, "%q is not a cluster ID this plugin lists, which are <Teleport cluster>/<Kubernetes cluster>", req.ClusterID)
	}
	return p.delegate(p.pinnedProxy(req.Account), cluster, kube), nil
}

// delegate returns the exec credential tsh kube login writes for a cluster.
func (p *Provider) delegate(proxy, cluster, kube string) provider.Credential {
	args := []string{"kube", "credentials", "--kube-cluster=" + kube, "--teleport-cluster=" + cluster}
	if proxy != "" {
		args = append(args, "--proxy="+proxy)
	}
	c := provider.Delegate("tsh", args...)
	c.Exec.APIVersion = execAPIVersion
	c.Exec.InstallHint = InstallHint
	if home := p.getenv(HomeEnv); home != "" {
		c.Exec.Env = map[string]string{HomeEnv: home}
	}
	return c
}

// pinnedProxy returns the proxy the credential command should name: the
// account's, or else the current tsh profile's. When neither can be read it
// returns "", and tsh uses its current profile when it runs.
func (p *Provider) pinnedProxy(acct provider.Account) string {
	if proxy := acct.ConfigString(ConfigProxy); proxy != "" {
		return proxy
	}
	home, err := p.home()
	if err != nil {
		return ""
	}
	name, err := os.ReadFile(filepath.Join(home, "current-profile"))
	if err != nil {
		return ""
	}
	prof, err := readProfile(home, strings.TrimSpace(string(name)))
	if err != nil {
		return ""
	}
	return prof.WebProxyAddr
}

// home returns tsh's profile directory.
func (p *Provider) home() (string, error) {
	if dir := p.getenv(HomeEnv); dir != "" {
		return dir, nil
	}
	dir, err := os.UserHomeDir()
	if err != nil {
		return "", provider.Errorf(provider.InvalidConfig, "finding tsh's profile directory: %v", err).
			WithHint("set " + HomeEnv + " to tsh's profile directory")
	}
	return filepath.Join(dir, ".tsh"), nil
}

// activeStatus returns the tsh profile for proxy, or the active one.
func activeStatus(ctx context.Context, proxy string) (activeProfile, error) {
	args := []string{"status", "--format=json"}
	if proxy != "" {
		args = append(args, "--proxy="+proxy)
	}
	// tsh status fails when nobody is signed in or the session has expired.
	// Checking first also keeps tsh kube ls from starting a new sign-in.
	out, err := tsh(ctx, provider.Unauthenticated, loginHint(proxy), args...)
	if err != nil {
		return activeProfile{}, err
	}
	var st status
	if err := json.Unmarshal(out, &st); err != nil {
		return activeProfile{}, provider.Errorf(provider.Internal, "reading tsh status: %v", err)
	}
	if st.Active == nil {
		return activeProfile{}, provider.Errorf(provider.Unauthenticated, "tsh has no active profile").WithHint(loginHint(proxy))
	}
	return *st.Active, nil
}

func loginHint(proxy string) string {
	if proxy == "" {
		proxy = "<your Teleport proxy>"
	}
	return fmt.Sprintf("run `tsh login --proxy=%s`", proxy)
}

// readProfile reads the tsh profile file for the proxy host name.
func readProfile(home, name string) (profile, error) {
	path := filepath.Join(home, name+".yaml")
	b, err := os.ReadFile(path)
	if err != nil {
		return profile{}, provider.Errorf(provider.Unauthenticated, "reading the tsh profile: %v", err).WithHint(loginHint(name))
	}
	var prof profile
	if err := yaml.Unmarshal(b, &prof); err != nil {
		return profile{}, provider.Errorf(provider.Internal, "reading %s: %v", path, err)
	}
	return prof, nil
}

// needsLocalProxy reports what tsh's Profile.RequireKubeLocalProxy does:
// kubectl cannot connect directly when the private key lives on a hardware
// key, or when the proxy sits behind a load balancer that needs tsh to
// upgrade the connection. tsh kube credentials then refuses, and tsh proxy
// kube is the way in.
func (p profile) needsLocalProxy() bool {
	if strings.HasPrefix(p.PrivateKeyPolicy, "hardware_key") {
		return true
	}
	return p.KubeProxyAddr == p.WebProxyAddr && p.TLSRoutingConnUpgradeRequired
}

// relayAddr returns the Teleport relay in use: the one chosen at tsh login,
// or the cluster's default, or none when it was turned off.
func (p profile) relayAddr() string {
	switch p.RelayAddr {
	case "":
		return p.DefaultRelayAddr
	case "none":
		return ""
	default:
		return p.RelayAddr
	}
}

// endpoint returns the server URL and TLS server name tsh kube login writes
// for a Kubernetes cluster: through the relay when there is one, otherwise
// to the proxy's Kubernetes address, with an SNI prefix when the proxy uses
// TLS routing.
func (p profile) endpoint(cluster, kube string) (server, serverName string) {
	if relay := p.relayAddr(); relay != "" {
		return (&url.URL{Scheme: "https", Host: relay}).String(), relayServerName(cluster, kube)
	}
	host, port := splitAddr(p.KubeProxyAddr, kubeListenPort)
	server = "https://" + net.JoinHostPort(host, strconv.Itoa(port))
	if p.TLSRoutingEnabled {
		serverName = kubeServerName(host)
	}
	return server, serverName
}

// splitAddr splits an address as Teleport's utils.ParseAddr does, with or
// without a scheme, using port def when it has none.
func splitAddr(addr string, def int) (host string, port int) {
	if !strings.Contains(addr, "://") {
		addr = "tcp://" + addr
	}
	u, err := url.Parse(addr)
	if err != nil {
		return "", def
	}
	port, err = strconv.Atoi(u.Port())
	if err != nil {
		port = def
	}
	return u.Hostname(), port
}

// kubeServerName is tsh's GetKubeTLSServerName: the proxy host behind the
// Kubernetes SNI prefix, or Teleport's internal domain when the host is not
// a name.
func kubeServerName(host string) string {
	if host == "" || host == "localhost" || net.ParseIP(host) != nil {
		return kubeSNIPrefix + apiDomain
	}
	return kubeSNIPrefix + host
}

// relayServerName is the server name that routes a relay connection to one
// Kubernetes cluster: a hash of the two SHA-256 digests of the Teleport and
// Kubernetes cluster names.
func relayServerName(cluster, kube string) string {
	c := sha256.Sum256([]byte(cluster))
	k := sha256.Sum256([]byte(kube))
	h := sha256.New()
	h.Write(c[:])
	h.Write(k[:])
	return relaySNIPrefix + relayHashEncoding.EncodeToString(h.Sum(nil)) + relaySNISuffix
}

// rootCAs returns the CA certificates tsh kube login writes into a
// kubeconfig, from the profile's key directory: those of the root cluster,
// whose proxy kubectl connects to, or every trusted cluster's when the
// profile was signed in with --load-all-cas.
func rootCAs(keyDir, user string, all bool, proxy string) ([]byte, error) {
	var root string
	if !all {
		var err error
		if root, err = rootCluster(keyDir, user, proxy); err != nil {
			return nil, err
		}
	}
	casDir := filepath.Join(keyDir, "cas")
	entries, err := os.ReadDir(casDir)
	if err != nil {
		return nil, provider.Errorf(provider.Unauthenticated, "reading tsh's trusted certificates: %v", err).WithHint(loginHint(proxy))
	}
	var out []byte
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		path := filepath.Join(casDir, e.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, provider.Errorf(provider.Internal, "reading tsh's trusted certificates: %v", err)
		}
		for {
			var block *pem.Block
			if block, data = pem.Decode(data); block == nil {
				break
			}
			if block.Type != "CERTIFICATE" || len(block.Headers) != 0 {
				continue
			}
			if !all {
				cert, err := x509.ParseCertificate(block.Bytes)
				if err != nil {
					return nil, provider.Errorf(provider.Internal, "reading %s: %v", path, err)
				}
				// Teleport names a cluster's certificate authorities after
				// the cluster.
				if cert.Subject.CommonName != root {
					continue
				}
			}
			out = append(out, pem.EncodeToMemory(block)...)
		}
	}
	if len(out) == 0 {
		return nil, provider.Errorf(provider.Unauthenticated, "tsh has no certificate authority for the Teleport cluster %s", cmp.Or(root, "of this profile")).
			WithHint(loginHint(proxy))
	}
	return out, nil
}

// rootCluster returns the root cluster's name, which tsh reads from the
// issuer of the user's Teleport certificate.
func rootCluster(keyDir, user, proxy string) (string, error) {
	// Newer tsh versions name the certificate <user>.crt, older ones
	// <user>-x509.pem.
	for _, name := range []string{user + ".crt", user + "-x509.pem"} {
		path := filepath.Join(keyDir, name)
		data, err := os.ReadFile(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", provider.Errorf(provider.Internal, "reading the Teleport certificate: %v", err)
		}
		block, _ := pem.Decode(data)
		if block == nil {
			return "", provider.Errorf(provider.Internal, "%s holds no certificate", path)
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return "", provider.Errorf(provider.Internal, "reading %s: %v", path, err)
		}
		if cert.Issuer.CommonName == "" {
			return "", provider.Errorf(provider.Internal, "%s does not name its Teleport cluster", path)
		}
		return cert.Issuer.CommonName, nil
	}
	return "", provider.Errorf(provider.Unauthenticated, "tsh has no Teleport certificate for %s in %s", user, keyDir).WithHint(loginHint(proxy))
}

// tsh runs tsh and returns what it printed on stdout. tsh gets no standard
// input, so it fails rather than waiting for an answer. A failure is
// reported with code and hint, and tsh's own message.
func tsh(ctx context.Context, code provider.Code, hint string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "tsh", args...)
	var stderr bytes.Buffer
	// tsh's warnings go to the plugin's stderr too, which tug logs.
	cmd.Stderr = io.MultiWriter(&stderr, os.Stderr)
	out, err := cmd.Output()
	switch {
	case err == nil:
		return out, nil
	case errors.Is(err, exec.ErrNotFound):
		return nil, provider.Errorf(provider.Unavailable, "tsh is not installed or not on PATH").WithHint(InstallHint)
	case ctx.Err() != nil:
		return nil, fmt.Errorf("running tsh %s: %w", args[0], ctx.Err())
	}
	return nil, provider.Errorf(code, "tsh %s: %s", subcommand(args), toolMessage(stderr.Bytes(), err)).WithHint(hint)
}

// subcommand returns the words of args before the first flag.
func subcommand(args []string) string {
	for i, a := range args {
		if strings.HasPrefix(a, "-") {
			return strings.Join(args[:i], " ")
		}
	}
	return strings.Join(args, " ")
}

// toolMessage returns the reason tsh gave for failing: its ERROR line, or
// else the last line it printed, or else how it exited.
func toolMessage(stderr []byte, err error) string {
	var last string
	for line := range strings.Lines(string(stderr)) {
		line = strings.TrimSpace(line)
		if msg, ok := strings.CutPrefix(line, "ERROR: "); ok {
			return msg
		}
		if line != "" {
			last = line
		}
	}
	return cmp.Or(last, err.Error())
}
