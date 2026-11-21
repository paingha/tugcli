package iap

import (
	"context"
	"crypto/tls"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/paingha/tugcli/pkg/provider"
)

func TestRelayFor(t *testing.T) {
	withCert := instanceConfig
	withCert.CertFile, withCert.KeyFile = "client.pem", "client-key.pem"
	tests := []struct {
		name string
		cfg  Config
		want string
	}{
		{"default", instanceConfig, "wss://tunnel.cloudproxy.app/v4"},
		{"client certificate", withCert, "wss://mtls.tunnel.cloudproxy.app/v4"},
	}
	for _, tt := range tests {
		if got := relayFor(tt.cfg); got != tt.want {
			t.Errorf("relayFor(%s) = %s, want %s", tt.name, got, tt.want)
		}
	}
}

func TestDialWithClientCertificate(t *testing.T) {
	pki := newTestPKI(t)
	r := newFakePKIRelay(t, pki, tls.RequireAndVerifyClientCert, "mtls.tunnel.cloudproxy.app")
	cfg := instanceConfig
	cfg.CertFile, cfg.KeyFile = pki.clientCert(t, "tug-test-client")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	nc, err := Dial(ctx, cfg, "bastion:22", r.hostOptions()...)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	c := nc.(*conn)
	defer c.Close()

	if got := get(r, func() []string { return r.dialed }); len(got) != 1 || got[0] != "mtls.tunnel.cloudproxy.app:443" {
		t.Errorf("the tunnel dialled %v, want [mtls.tunnel.cloudproxy.app:443]", got)
	}
	req := r.request(0)
	if req.Host != "mtls.tunnel.cloudproxy.app" || req.TLS.ServerName != "mtls.tunnel.cloudproxy.app" {
		t.Errorf("Host, TLS server name = %s, %s; want mtls.tunnel.cloudproxy.app", req.Host, req.TLS.ServerName)
	}
	if got := peerName(req); got != "tug-test-client" {
		t.Errorf("client certificate = %q, want tug-test-client", got)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer "+testToken {
		t.Errorf("Authorization = %q, want the bearer token as well as the certificate", got)
	}
	if _, err := c.Write([]byte("mtls")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := readN(t, c, 4); string(got) != "mtls" {
		t.Errorf("read %q, want mtls", got)
	}

	// A renewed certificate is presented when the session resumes.
	pki.writeClientCert(t, "tug-renewed-client", cfg.CertFile, cfg.KeyFile)
	s := r.session()
	r.set(func() { s.dropAt = 5 })
	if _, err := c.Write([]byte("!")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := readN(t, c, 1); string(got) != "!" {
		t.Errorf("read %q, want !", got)
	}
	waitFor(t, "the tunnel to resume", func() bool { return len(get(r, func() []*http.Request { return r.requests })) > 1 })
	if got := peerName(r.request(1)); got != "tug-renewed-client" {
		t.Errorf("client certificate after resuming = %q, want tug-renewed-client", got)
	}
}

// peerName is the common name of the client certificate a request came
// with, or "".
func peerName(req *http.Request) string {
	if req.TLS == nil || len(req.TLS.PeerCertificates) == 0 {
		return ""
	}
	return req.TLS.PeerCertificates[0].Subject.CommonName
}

func TestMTLSRelayRefusesTunnelWithoutCertificate(t *testing.T) {
	pki := newTestPKI(t)
	r := newFakePKIRelay(t, pki, tls.RequireAndVerifyClientCert, "mtls.tunnel.cloudproxy.app")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := Dial(ctx, instanceConfig, "bastion:22", r.hostOptions(WithRelayURL(mtlsRelayURL))...)
	if err == nil || !strings.Contains(err.Error(), "reaching the IAP relay at mtls.tunnel.cloudproxy.app") {
		t.Errorf("Dial without a certificate = %v, want the TLS handshake to fail", err)
	}
}

func TestDialWithBadClientCertificate(t *testing.T) {
	pki := newTestPKI(t)
	certFile, keyFile := pki.clientCert(t, "tug-test-client")
	otherCert, _ := pki.clientCert(t, "someone-else")
	tests := []struct {
		name      string
		cert, key string
	}{
		{"missing certificate", filepath.Join(t.TempDir(), "missing.pem"), keyFile},
		{"missing key", certFile, filepath.Join(t.TempDir(), "missing-key.pem")},
		{"key of another certificate", otherCert, keyFile},
		{"key in place of the certificate", keyFile, keyFile},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := instanceConfig
			cfg.CertFile, cfg.KeyFile = tt.cert, tt.key
			_, err := Dial(context.Background(), cfg, "bastion:22", WithTokenSource(oauth2.StaticTokenSource(&oauth2.Token{AccessToken: testToken})))
			var pe *provider.Error
			if !errors.As(err, &pe) || pe.Code != provider.InvalidConfig || !strings.Contains(pe.Message, tt.cert) || !strings.Contains(pe.Hint, "cert and key") {
				t.Errorf("Dial() error = %v, want InvalidConfig naming %s with a hint", err, tt.cert)
			}
		})
	}
}
