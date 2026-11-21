package kube

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
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/paingha/tugcli/internal/catalog"
	"github.com/paingha/tugcli/internal/credentials"
	"github.com/paingha/tugcli/internal/guard"
	"github.com/paingha/tugcli/internal/tunnel"
)

// fakeTunnels routes every tunnel the same way.
type fakeTunnels struct{ route tunnel.Route }

func (f fakeTunnels) Route(context.Context, string) (tunnel.Route, error) { return f.route, nil }

// localProxy is a server like tsh's local Kubernetes proxy: its certificate
// is its own CA, it accepts only that certificate from clients, and the
// cluster is named in the path.
func localProxy(t *testing.T, prefix string) (*httptest.Server, tunnel.Endpoint) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "localhost"},
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
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	clientCAs := x509.NewCertPool()
	clientCAs.AppendCertsFromPEM(certPEM)

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, "the client sent the cluster's credential to the local proxy")
			return
		}
		if r.URL.Path != prefix+"/version" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"gitVersion":"v1.35.1"}`)
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{pair}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: clientCAs}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv, tunnel.Endpoint{Server: srv.URL + prefix, CAData: certPEM, ServerName: "root", CertData: certPEM, KeyData: keyPEM}
}

func TestTunnelEndpoint(t *testing.T) {
	const prefix = "/v1/teleport/cm9vdA/ZWtzLXByb2Q"
	srv, ep := localProxy(t, prefix)
	// The catalog's endpoint and CA are the Teleport proxy's, and the broker
	// holds no credential: the tunnel's endpoint replaces them all.
	cl := &catalog.Cluster{ID: "teleport/work/root/eks-prod", Name: "eks-prod", Server: "https://teleport.example.com:443",
		CAData: []byte("not the local proxy's CA"), TLSServerName: "kube-teleport-proxy-alpn.teleport.example.com", Tunnel: "tsh://teleport.example.com:443/root/eks-prod"}
	f := &Factory{
		Cluster:  cl,
		Broker:   &credentials.Broker{Store: credentials.NewMemoryStore()},
		Guard:    &guard.Policy{Cluster: cl.Name, ReadOnly: true, Now: time.Now},
		CacheDir: t.TempDir(),
		Tunnels:  fakeTunnels{tunnel.Route{Endpoint: &ep}},
	}
	cfg, err := f.RESTConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Host != srv.URL+prefix || cfg.ServerName != "root" || cfg.ExecProvider != nil || cfg.BearerToken != "" {
		t.Fatalf("RESTConfig host %q, server name %q, exec %v; want the tunnel's endpoint and no credential", cfg.Host, cfg.ServerName, cfg.ExecProvider)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	v, err := cs.Discovery().ServerVersion()
	if err != nil {
		t.Fatal(err)
	}
	if v.GitVersion != "v1.35.1" {
		t.Fatalf("ServerVersion = %s, want v1.35.1 from the local proxy", v.GitVersion)
	}
	// The guard still applies.
	err = cs.CoreV1().Namespaces().Delete(context.Background(), "payments", metav1.DeleteOptions{})
	if denied := (*guard.DeniedError)(nil); !errors.As(err, &denied) {
		t.Fatalf("delete through the tunnel = %v, want the read-only guard to refuse it", err)
	}
}
