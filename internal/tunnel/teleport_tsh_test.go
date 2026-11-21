package tunnel

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

const (
	tshSpec   = "tsh://teleport.example.com:443/root/eks-prod"
	tshPrefix = "/v1/teleport/cm9vdA/ZWtzLXByb2Q"
)

// tshScript stands in for tsh proxy kube. It records each run, checks its
// arguments, writes the kubeconfig the test prepared where tsh would, and
// waits to be stopped, as the real local proxy does.
const tshScript = `#!/bin/sh
echo "$$ $TELEPORT_KUBECONFIG $KUBECONFIG" >> "$FAKE_TSH_RECORD"
[ "$*" = "proxy kube eks-prod --cluster=root --proxy=teleport.example.com:443" ] || { echo "ERROR: unexpected arguments: $*" >&2; exit 1; }
cp "$FAKE_TSH_KUBECONFIG" "$TELEPORT_KUBECONFIG"
echo "Started local proxy for Kubernetes on 127.0.0.1:12345"
exec sleep 600
`

// fakeTsh puts script first on PATH as tsh.
func fakeTsh(t *testing.T, script string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake tsh is a shell script")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "tsh"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_TSH_RECORD", filepath.Join(t.TempDir(), "runs"))
}

// tshRun is one run of the fake tsh.
type tshRun struct {
	pid                    int
	kubeconfig, defaultKCF string
}

func tshRuns(t *testing.T) []tshRun {
	t.Helper()
	data, err := os.ReadFile(os.Getenv("FAKE_TSH_RECORD"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var runs []tshRun
	for line := range strings.Lines(string(data)) {
		f := strings.Fields(line)
		if len(f) != 3 {
			t.Fatalf("record line %q", line)
		}
		pid, err := strconv.Atoi(f[0])
		if err != nil {
			t.Fatal(err)
		}
		runs = append(runs, tshRun{pid: pid, kubeconfig: f[1], defaultKCF: f[2]})
	}
	return runs
}

// tshLocalProxy is the pair of servers tsh proxy kube runs: an HTTPS server
// whose certificate is its own CA, which takes only that certificate from
// clients, and a forward proxy that sends every CONNECT to it. It writes the
// kubeconfig tsh would, for the fake tsh to copy.
type tshLocalProxy struct {
	certPEM, keyPEM []byte
	forward         string
}

func newTshLocalProxy(t *testing.T) *tshLocalProxy {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "localhost", Organization: []string{"Teleport"}},
		DNSNames:              []string{"root"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	lp := &tshLocalProxy{
		certPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		keyPEM:  pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
	}
	pair, err := tls.X509KeyPair(lp.certPEM, lp.keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	clientCAs := x509.NewCertPool()
	clientCAs.AppendCertsFromPEM(lp.certPEM)
	api := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != tshPrefix+"/version" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		fmt.Fprint(w, `{"gitVersion":"v1.35.1"}`)
	}))
	api.TLS = &tls.Config{Certificates: []tls.Certificate{pair}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: clientCAs}
	api.StartTLS()
	t.Cleanup(api.Close)

	target := api.Listener.Addr().String()
	fwd := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		up, err := net.Dial("tcp", target)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		c, buf, err := http.NewResponseController(w).Hijack()
		if err != nil {
			up.Close()
			return
		}
		_, _ = io.WriteString(c, "HTTP/1.1 200 Connection established\r\n\r\n")
		go func() {
			_, _ = io.Copy(up, buf)
			up.Close()
		}()
		_, _ = io.Copy(c, up)
		c.Close()
	}))
	t.Cleanup(fwd.Close)
	lp.forward = fwd.Listener.Addr().String()

	cfg := clientcmdapi.NewConfig()
	cfg.Clusters["root-eks-prod"] = &clientcmdapi.Cluster{
		ProxyURL:                 "http://" + lp.forward,
		Server:                   "https://teleport.example.com:443" + tshPrefix,
		CertificateAuthorityData: lp.certPEM,
		TLSServerName:            "root",
	}
	cfg.AuthInfos["root-eks-prod"] = &clientcmdapi.AuthInfo{ClientCertificateData: lp.certPEM, ClientKeyData: lp.keyPEM}
	cfg.Contexts["root-eks-prod"] = &clientcmdapi.Context{Cluster: "root-eks-prod", AuthInfo: "root-eks-prod"}
	// A user's own cluster that tsh would copy over from the default
	// kubeconfig; the tunnel must pick tsh's.
	cfg.Clusters["kind-dev"] = &clientcmdapi.Cluster{Server: "https://127.0.0.1:6443"}
	cfg.Contexts["kind-dev"] = &clientcmdapi.Context{Cluster: "kind-dev", AuthInfo: "root-eks-prod"}
	cfg.CurrentContext = "root-eks-prod"
	data, err := clientcmd.Write(*cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_TSH_KUBECONFIG", path)
	return lp
}

// get sends GET /version through r as a Kubernetes client would.
func get(t *testing.T, r Route) string {
	t.Helper()
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(r.Endpoint.CAData)
	pair, err := tls.X509KeyPair(r.Endpoint.CertData, r.Endpoint.KeyData)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		Proxy:           http.ProxyURL(r.Proxy),
		TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: r.Endpoint.ServerName, Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12},
	}}
	resp, err := client.Get(r.Endpoint.Server + "/version")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return string(body)
}

func TestTshTunnel(t *testing.T) {
	lp := newTshLocalProxy(t)
	fakeTsh(t, tshScript)
	p := newTestPool(t)

	r, err := p.Route(context.Background(), tshSpec)
	if err != nil {
		t.Fatal(err)
	}
	ep := r.Endpoint
	if ep == nil || ep.Server != "https://teleport.example.com:443"+tshPrefix || ep.ServerName != "root" ||
		string(ep.CAData) != string(lp.certPEM) || string(ep.CertData) != string(lp.certPEM) || string(ep.KeyData) != string(lp.keyPEM) {
		t.Fatalf("Route endpoint = %+v, want the local proxy's from tsh's kubeconfig", ep)
	}
	// Through tug's SOCKS proxy and tsh's forward proxy, with the local
	// proxy's CA and client certificate.
	if body := get(t, r); body != `{"gitVersion":"v1.35.1"}` {
		t.Fatalf("GET /version = %q", body)
	}

	runs := tshRuns(t)
	if len(runs) != 1 {
		t.Fatalf("tsh ran %d times, want once", len(runs))
	}
	run := runs[0]
	if _, err := os.Stat(run.defaultKCF); !errors.Is(err, fs.ErrNotExist) || filepath.Dir(run.defaultKCF) != filepath.Dir(run.kubeconfig) {
		t.Errorf("tsh's KUBECONFIG = %s (%v), want a file that does not exist beside its own kubeconfig", run.defaultKCF, err)
	}
	if runtime.GOOS != "windows" {
		if fi, err := os.Stat(filepath.Dir(run.kubeconfig)); err != nil || fi.Mode().Perm() != 0o700 {
			t.Errorf("tsh's kubeconfig directory: %v, %v; want mode 0700", fi, err)
		}
	}

	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Dir(run.kubeconfig)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("tsh's kubeconfig directory after Close: %v, want it removed", err)
	}
}

func TestTshTunnelOpensAgainWhenTshExits(t *testing.T) {
	newTshLocalProxy(t)
	fakeTsh(t, tshScript)
	p := newTestPool(t)
	ctx := context.Background()

	first, err := p.Route(ctx, tshSpec)
	if err != nil {
		t.Fatal(err)
	}
	proc, err := os.FindProcess(tshRuns(t)[0].pid)
	if err != nil {
		t.Fatal(err)
	}
	if err := proc.Kill(); err != nil {
		t.Fatal(err)
	}
	var second Route
	waitFor(t, func() bool {
		second, err = p.Route(ctx, tshSpec)
		return err == nil && second.Proxy.String() != first.Proxy.String()
	})
	if runs := tshRuns(t); len(runs) != 2 {
		t.Fatalf("tsh ran %d times, want twice", len(runs))
	}
	if body := get(t, second); body != `{"gitVersion":"v1.35.1"}` {
		t.Fatalf("GET /version through the new tsh = %q", body)
	}
}

func TestTshTunnelFailures(t *testing.T) {
	t.Run("signed out", func(t *testing.T) {
		fakeTsh(t, "#!/bin/sh\necho 'ERROR: Not logged in.' >&2\nexit 1\n")
		_, err := newTestPool(t).Route(context.Background(), tshSpec)
		if err == nil || !strings.Contains(err.Error(), "exited before it started: Not logged in.") {
			t.Fatalf("Route = %v, want tsh's reason", err)
		}
	})
	t.Run("no tsh", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		_, err := newTestPool(t).Route(context.Background(), tshSpec)
		if err == nil || !strings.Contains(err.Error(), "tsh is not installed") || !strings.Contains(err.Error(), tshInstallHint) {
			t.Fatalf("Route = %v, want where to get tsh", err)
		}
	})
	t.Run("no clusters", func(t *testing.T) {
		_, err := newTestPool(t).Route(context.Background(), "tsh://teleport.example.com:443/root")
		if err == nil || !strings.Contains(err.Error(), "name the Teleport and Kubernetes clusters") {
			t.Fatalf("Route = %v, want the spec explained", err)
		}
	})
}

func TestTshClusters(t *testing.T) {
	tests := []struct {
		spec, cluster, kube string
	}{
		{"tsh://teleport.example.com:443/root/eks-prod", "root", "eks-prod"},
		{"tsh://teleport.example.com/leaf.example.com/a%2Fb", "leaf.example.com", "a/b"},
		{"tsh://teleport.example.com/root", "", ""},
		{"tsh://teleport.example.com/root/eks/extra", "", ""},
		{"tsh://teleport.example.com", "", ""},
	}
	for _, tt := range tests {
		u, err := Parse(tt.spec)
		if err != nil {
			t.Fatal(err)
		}
		cluster, kube, err := tshClusters(u)
		if cluster != tt.cluster || kube != tt.kube || (err == nil) != (tt.cluster != "") {
			t.Errorf("tshClusters(%s) = %q, %q, %v; want %q, %q", tt.spec, cluster, kube, err, tt.cluster, tt.kube)
		}
	}
}
