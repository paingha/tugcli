package doctor

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	authnv1 "k8s.io/api/authentication/v1"
	authzv1 "k8s.io/api/authorization/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/paingha/tugcli/internal/credentials"
	"github.com/paingha/tugcli/pkg/provider"
)

// Thresholds for warnings.
const (
	clockSkewWarn  = 30 * time.Second
	clockSkewFail  = 5 * time.Minute
	certExpiryWarn = 14 * 24 * time.Hour
)

// permissionChecks are the everyday actions summarised by the permissions
// check, as (verb, resource, subresource, group, label).
var permissionChecks = []struct {
	verb, resource, subresource, group, label string
}{
	{"list", "pods", "", "", "list pods"},
	{"get", "pods", "log", "", "logs"},
	{"create", "pods", "exec", "", "exec"},
	{"create", "pods", "portforward", "", "port-forward"},
	{"patch", "deployments", "", "apps", "patch deployments"},
	{"delete", "pods", "", "", "delete pods"},
}

func checkCluster(ctx context.Context, cfg Config, kubectlVersion string) []Result {
	cl := cfg.Cluster
	var out []Result
	add := func(r Result) { out = append(out, r) }

	u, err := url.Parse(cl.Server)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		add(cluster("endpoint", Fail, fmt.Sprintf("%q is not an https URL", cl.Server), "run `tug sync`, or import the cluster again"))
		return out
	}
	host, port := u.Hostname(), u.Port()
	if port == "" {
		port = "443"
		if u.Scheme == "http" {
			port = "80"
		}
	}
	addr := net.JoinHostPort(host, port)

	// The provider's access list comes first: when it blocks this address,
	// the TCP check below fails and stops everything after it.
	if cfg.Access != nil {
		checkAccess(ctx, cfg, add)
	}

	proxied := cl.ProxyURL != ""
	if !proxied {
		if p, err := cfg.Proxy(&http.Request{URL: u}); err == nil && p != nil {
			proxied = true
		}
	}
	if proxied {
		add(cluster("network", Skip, "reached through a proxy; the API check below covers the connection", ""))
	} else if !networkChecks(ctx, cfg, host, addr, add) {
		return out
	}
	if !proxied && u.Scheme == "https" && !checkTLS(ctx, cfg, host, addr, add) {
		return out
	}

	if cfg.Credential != nil {
		res, err := cfg.Credential(ctx)
		if err != nil {
			fix := "run `tug sync`, or import the account again"
			if pe := provider.AsError(err); pe.Hint != "" {
				fix = pe.Hint
			}
			add(cluster("credentials", Fail, err.Error(), fix))
			return out
		}
		add(cluster("credentials", OK, describeCredential(res, cfg.Now()), ""))
	}

	if cfg.RESTConfig == nil {
		return out
	}
	rc, err := cfg.RESTConfig(ctx)
	if err != nil {
		add(cluster("API server", Fail, err.Error(), ""))
		return out
	}
	// Bound every request, not only /version: a stalled review must not keep
	// doctor from reporting.
	rc = rest.CopyConfig(rc)
	if limit := 2 * cfg.Timeout; rc.Timeout == 0 || rc.Timeout > limit {
		rc.Timeout = limit
	}
	serverVersion, ok := checkAPI(ctx, cfg, rc, add)
	if !ok {
		return out
	}
	checkVersionSkew(kubectlVersion, serverVersion, add)
	cs, err := kubernetes.NewForConfig(rc)
	if err != nil {
		add(cluster("permissions", Fail, err.Error(), ""))
		return out
	}
	checkIdentity(ctx, cs, add)
	checkPermissions(ctx, cs, cfg.Namespace, add)
	return out
}

// networkChecks resolves the host and opens a TCP connection. It reports
// whether the cluster is reachable.
func networkChecks(ctx context.Context, cfg Config, host, addr string, add func(Result)) bool {
	if cfg.Tunnel != "" {
		dctx, cancel := context.WithTimeout(ctx, 3*cfg.Timeout) // a tunnel may sign in first
		defer cancel()
		conn, err := cfg.Dial(dctx, "tcp", addr)
		if err != nil {
			add(cluster("tunnel", Fail, fmt.Sprintf("cannot reach %s through %s: %v", addr, cfg.Tunnel, err),
				"check that the jump host, instance or proxy is up and that you can sign in to it; `tug tunnel test` retries the tunnel alone"))
			return false
		}
		conn.Close()
		add(cluster("tunnel", OK, "reached "+addr+" through "+cfg.Tunnel, ""))
		return true
	}
	if net.ParseIP(host) == nil {
		lctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
		addrs, err := cfg.Resolver.LookupHost(lctx, host)
		cancel()
		if err != nil {
			add(cluster("DNS", Fail, fmt.Sprintf("cannot resolve %s: %v", host, err),
				"check your network or VPN; a private endpoint resolves only inside its network"))
			return false
		}
		add(cluster("DNS", OK, host+" is "+strings.Join(addrs, ", "), ""))
	}
	dctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()
	conn, err := cfg.Dial(dctx, "tcp", addr)
	if err != nil {
		add(cluster("TCP", Fail, fmt.Sprintf("cannot connect to %s: %v", addr, err),
			"the API server may allow only some addresses or networks; check its access list (pass --egress-ip for providers that can) or connect through your VPN"))
		return false
	}
	conn.Close()
	add(cluster("TCP", OK, "connected to "+addr, ""))
	return true
}

// checkTLS verifies the server certificate against the CA tug holds. It
// reports false when TLS fails, since every later check needs it.
func checkTLS(ctx context.Context, cfg Config, host, addr string, add func(Result)) bool {
	cl := cfg.Cluster
	tlsCfg := &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}
	if cl.TLSServerName != "" {
		tlsCfg.ServerName = cl.TLSServerName
	}
	if len(cl.CAData) > 0 {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(cl.CAData) {
			add(cluster("TLS", Fail, "the stored CA is not valid PEM", "run `tug sync` to fetch the CA again"))
			return false
		}
		tlsCfg.RootCAs = pool
	}
	if cl.InsecureSkipTLSVerify {
		add(cluster("TLS", Warn, "certificate verification is turned off for this cluster", "remove insecure-skip-tls-verify from the source kubeconfig and import it again"))
		return true
	}
	dctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()
	raw, err := cfg.Dial(dctx, "tcp", addr)
	if err != nil {
		add(cluster("TLS", Fail, err.Error(), ""))
		return false
	}
	conn := tls.Client(raw, tlsCfg)
	defer conn.Close()
	if err := conn.HandshakeContext(dctx); err != nil {
		add(tlsFailure(err))
		return false
	}
	certs := conn.ConnectionState().PeerCertificates
	if len(certs) == 0 {
		add(cluster("TLS", Fail, "the server sent no certificate", ""))
		return false
	}
	left := certs[0].NotAfter.Sub(cfg.Now())
	if left < certExpiryWarn {
		add(cluster("TLS", Warn, fmt.Sprintf("the server certificate expires in %s", roundDuration(left)), "ask the cluster's administrator to renew it"))
		return true
	}
	add(cluster("TLS", OK, "certificate verified, valid until "+certs[0].NotAfter.Format("2006-01-02"), ""))
	return true
}

func tlsFailure(err error) Result {
	var (
		unknown  x509.UnknownAuthorityError
		hostname x509.HostnameError
		invalid  x509.CertificateInvalidError
	)
	switch {
	case errors.As(err, &unknown):
		return cluster("TLS", Fail, "the server's certificate is not signed by the CA tug holds for this cluster",
			"run `tug sync` to fetch the current CA; if it persists, the endpoint may not be your cluster")
	case errors.As(err, &hostname):
		return cluster("TLS", Fail, "the certificate does not match the endpoint: "+hostname.Error(),
			"run `tug sync`; the endpoint or its name may have changed")
	case errors.As(err, &invalid) && invalid.Reason == x509.Expired:
		return cluster("TLS", Fail, "the server certificate has expired or the local clock is wrong: "+invalid.Error(),
			"check this machine's clock, then ask the cluster's administrator to renew the certificate")
	default:
		return cluster("TLS", Fail, "TLS handshake failed: "+err.Error(), "")
	}
}

// describeCredential says what kind of credential tug holds and when it
// expires.
func describeCredential(r *credentials.Resolved, now time.Time) string {
	kind := "bearer token"
	switch {
	case r.Delegate != nil:
		kind = "from " + r.Delegate.Command
	case len(r.CertPEM) > 0:
		kind = "client certificate"
	}
	if r.ExpiresAt == nil {
		return kind + ", no expiry"
	}
	return fmt.Sprintf("%s, expires in %s", kind, roundDuration(r.ExpiresAt.Sub(now)))
}

// checkAPI requests /version with the cluster's credentials, reports the
// server version, and compares the server's clock with ours.
func checkAPI(ctx context.Context, cfg Config, rc *rest.Config, add func(Result)) (string, bool) {
	client, err := rest.HTTPClientFor(rc)
	if err != nil {
		add(cluster("API server", Fail, err.Error(), ""))
		return "", false
	}
	rctx, cancel := context.WithTimeout(ctx, cfg.Timeout*2)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodGet, strings.TrimSuffix(rc.Host, "/")+"/version", nil)
	if err != nil {
		add(cluster("API server", Fail, err.Error(), ""))
		return "", false
	}
	sent := cfg.Now()
	resp, err := client.Do(req)
	if err != nil {
		add(cluster("API server", Fail, "request failed: "+err.Error(), ""))
		return "", false
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	switch {
	case resp.StatusCode == http.StatusOK:
	case resp.StatusCode == http.StatusUnauthorized:
		add(cluster("API server", Fail, "the API server rejected the credential (401)",
			"run `tug auth token --cluster "+cfg.Cluster.Name+" --refresh`; if that fails, import the account again"))
		return "", false
	case resp.StatusCode >= 500:
		add(cluster("API server", Fail, fmt.Sprintf("GET /version answered %s, so the API server is not serving requests", resp.Status),
			"try again shortly; if it persists, check the cluster's control plane with its provider"))
		return "", false
	case resp.StatusCode == http.StatusForbidden:
		add(cluster("API server", Warn, "reachable, but this identity may not read /version", ""))
	default:
		add(cluster("API server", Warn, fmt.Sprintf("GET /version answered %s", resp.Status), ""))
	}

	var v struct {
		GitVersion string `json:"gitVersion"`
	}
	_ = json.Unmarshal(body, &v)
	detail := "reachable and accepting tug's credential"
	if v.GitVersion != "" {
		detail = "Kubernetes " + v.GitVersion
	}
	if resp.StatusCode == http.StatusOK {
		add(cluster("API server", OK, detail, ""))
	}
	checkClockSkew(resp.Header.Get("Date"), sent, add)
	return v.GitVersion, true
}

func checkClockSkew(date string, sent time.Time, add func(Result)) {
	serverTime, err := http.ParseTime(date)
	if err != nil {
		add(cluster("clock", Skip, "the server sent no Date header", ""))
		return
	}
	skew := sent.Sub(serverTime)
	if skew < 0 {
		skew = -skew
	}
	// HTTP dates have one-second resolution.
	skew = skew.Truncate(time.Second)
	switch {
	case skew >= clockSkewFail:
		add(cluster("clock", Fail, fmt.Sprintf("this machine's clock is %s off the server's; tokens and certificates will be rejected", skew),
			"turn on automatic time synchronisation"))
	case skew >= clockSkewWarn:
		add(cluster("clock", Warn, fmt.Sprintf("this machine's clock is %s off the server's", skew), "turn on automatic time synchronisation"))
	default:
		add(cluster("clock", OK, "in step with the server", ""))
	}
}

// checkVersionSkew applies kubectl's policy: a client within one minor
// version of the server.
func checkVersionSkew(client, server string, add func(Result)) {
	cMinor, cOK := minor(client)
	sMinor, sOK := minor(server)
	if !cOK || !sOK {
		return
	}
	diff := cMinor - sMinor
	if diff < 0 {
		diff = -diff
	}
	if diff > 1 {
		add(cluster("version skew", Warn, fmt.Sprintf("kubectl %s is %d minor versions from the server's %s; kubectl supports one", client, diff, server),
			fmt.Sprintf("use kubectl 1.%d to 1.%d", sMinor-1, sMinor+1)))
		return
	}
	add(cluster("version skew", OK, fmt.Sprintf("kubectl %s with server %s", client, server), ""))
}

// minor extracts the minor version from "v1.35.2-gke.100".
func minor(v string) (int, bool) {
	parts := strings.SplitN(strings.TrimPrefix(v, "v"), ".", 3)
	if len(parts) < 2 || parts[0] != "1" {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimRightFunc(parts[1], func(r rune) bool { return r < '0' || r > '9' }))
	return n, err == nil
}

func checkIdentity(ctx context.Context, cs kubernetes.Interface, add func(Result)) {
	review, err := cs.AuthenticationV1().SelfSubjectReviews().Create(ctx, &authnv1.SelfSubjectReview{}, metav1.CreateOptions{})
	switch {
	case apierrors.IsNotFound(err):
		add(cluster("identity", Skip, "the server does not support SelfSubjectReview", ""))
	case err != nil:
		add(cluster("identity", Warn, "asking who tug is signed in as: "+err.Error(), ""))
	default:
		ui := review.Status.UserInfo
		detail := ui.Username
		if len(ui.Groups) > 0 {
			detail += " (groups: " + strings.Join(ui.Groups, ", ") + ")"
		}
		add(cluster("identity", OK, detail, ""))
	}
}

func checkPermissions(ctx context.Context, cs kubernetes.Interface, ns string, add func(Result)) {
	if ns == "" {
		ns = "default"
	}
	var allowed, denied []string
	for _, p := range permissionChecks {
		review := &authzv1.SelfSubjectAccessReview{Spec: authzv1.SelfSubjectAccessReviewSpec{
			ResourceAttributes: &authzv1.ResourceAttributes{
				Namespace: ns, Verb: p.verb, Group: p.group, Resource: p.resource, Subresource: p.subresource,
			},
		}}
		res, err := cs.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, review, metav1.CreateOptions{})
		if err != nil {
			add(cluster("permissions", Warn, "checking permissions: "+err.Error(), ""))
			return
		}
		if res.Status.Allowed {
			allowed = append(allowed, p.label)
		} else {
			denied = append(denied, p.label)
		}
	}
	detail := "in " + ns + ": "
	if len(allowed) > 0 {
		detail += "can " + strings.Join(allowed, ", ")
	}
	if len(denied) > 0 {
		if len(allowed) > 0 {
			detail += "; "
		}
		detail += "cannot " + strings.Join(denied, ", ")
	}
	status := OK
	fix := ""
	if len(allowed) == 0 || allowed[0] != "list pods" {
		status = Warn
		fix = "ask the cluster's administrator for access, or switch namespace with `tug ns`"
	}
	add(cluster("permissions", status, detail, fix))
}

func checkAccess(ctx context.Context, cfg Config, add func(Result)) {
	findings, err := cfg.Access(ctx)
	if err != nil {
		add(cluster("provider access list", Warn, "checking: "+err.Error(), ""))
		return
	}
	for _, f := range findings {
		status := OK
		switch f.Severity {
		case provider.SeverityWarning:
			status = Warn
		case provider.SeverityError:
			status = Fail
		}
		add(cluster("provider access list", status, f.Message, f.Fix))
	}
}

func roundDuration(d time.Duration) string {
	switch {
	case d >= 48*time.Hour:
		return strconv.Itoa(int(d.Hours()/24)) + " days"
	case d >= 2*time.Hour:
		return strconv.Itoa(int(d.Hours())) + " hours"
	case d >= 2*time.Minute:
		return strconv.Itoa(int(d.Minutes())) + " minutes"
	default:
		return d.Truncate(time.Second).String()
	}
}
