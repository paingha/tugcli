package iap

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// testPKI is a certificate authority for tests: it issues the fake relay's
// server certificate, for the host names the tunnel chooses, and client
// certificates for mTLS.
type testPKI struct {
	pool   *x509.CertPool
	cert   *x509.Certificate
	key    *ecdsa.PrivateKey
	serial int64
}

func newTestPKI(t *testing.T) *testPKI {
	t.Helper()
	key := newKey(t)
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "tug test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return &testPKI{pool: pool, cert: cert, key: key, serial: 1}
}

func newKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// issue signs a certificate for key.
func (p *testPKI) issue(t *testing.T, tmpl *x509.Certificate, key *ecdsa.PrivateKey) []byte {
	t.Helper()
	p.serial++
	tmpl.SerialNumber = big.NewInt(p.serial)
	tmpl.NotBefore = time.Now().Add(-time.Hour)
	tmpl.NotAfter = time.Now().Add(time.Hour)
	tmpl.KeyUsage = x509.KeyUsageDigitalSignature
	der, err := x509.CreateCertificate(rand.Reader, tmpl, p.cert, &key.PublicKey, p.key)
	if err != nil {
		t.Fatal(err)
	}
	return der
}

// serverCert returns a server certificate for hosts and 127.0.0.1.
func (p *testPKI) serverCert(t *testing.T, hosts ...string) tls.Certificate {
	t.Helper()
	key := newKey(t)
	der := p.issue(t, &x509.Certificate{
		Subject:     pkix.Name{CommonName: hosts[0]},
		DNSNames:    hosts,
		IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)},
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}, key)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// writeClientCert issues a client certificate named cn and writes it and
// its key as PEM to certFile and keyFile.
func (p *testPKI) writeClientCert(t *testing.T, cn, certFile, keyFile string) {
	t.Helper()
	key := newKey(t)
	der := p.issue(t, &x509.Certificate{
		Subject:     pkix.Name{CommonName: cn},
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}, key)
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
}

// clientCert issues a client certificate named cn into a new directory and
// returns the paths of the certificate and key.
func (p *testPKI) clientCert(t *testing.T, cn string) (certFile, keyFile string) {
	t.Helper()
	dir := t.TempDir()
	certFile, keyFile = filepath.Join(dir, "client.pem"), filepath.Join(dir, "client-key.pem")
	p.writeClientCert(t, cn, certFile, keyFile)
	return certFile, keyFile
}
