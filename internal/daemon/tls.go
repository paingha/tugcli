package daemon

import (
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
	"os"
	"path/filepath"
	"time"

	"github.com/paingha/tugcli/internal/config"
)

// tlsLifetime is how long tugd's serving certificate is valid; it is
// replaced when less than tlsRenewBefore remains.
const (
	tlsLifetime    = 365 * 24 * time.Hour
	tlsRenewBefore = 30 * 24 * time.Hour
)

// The proxy serves TLS even on loopback: client-go only sends credentials,
// exec plugins included, to https servers.
func certFile(runtimeDir string) string { return filepath.Join(runtimeDir, "tugd-tls.crt") }
func keyFile(runtimeDir string) string  { return filepath.Join(runtimeDir, "tugd-tls.key") }

// loadOrCreateTLS returns tugd's serving certificate and its PEM, which
// session kubeconfigs trust as their CA. It is kept in the private runtime
// directory and reused, so kubeconfigs stay valid when tugd restarts.
func loadOrCreateTLS(runtimeDir string, now time.Time) (tls.Certificate, []byte, error) {
	certPEM, certErr := os.ReadFile(certFile(runtimeDir))
	keyPEM, keyErr := os.ReadFile(keyFile(runtimeDir))
	if certErr == nil && keyErr == nil {
		if cert, err := tls.X509KeyPair(certPEM, keyPEM); err == nil && cert.Leaf != nil && now.Add(tlsRenewBefore).Before(cert.Leaf.NotAfter) {
			return cert, certPEM, nil
		}
	} else if !errors.Is(certErr, os.ErrNotExist) && certErr != nil {
		return tls.Certificate{}, nil, certErr
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 62))
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "tugd local API proxy"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(tlsLifetime),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
		DNSNames:              []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := config.WriteFileAtomic(keyFile(runtimeDir), keyPEM, 0o600); err != nil {
		return tls.Certificate{}, nil, err
	}
	if err := config.WriteFileAtomic(certFile(runtimeDir), certPEM, 0o600); err != nil {
		return tls.Certificate{}, nil, err
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("loading the new certificate: %w", err)
	}
	return cert, certPEM, nil
}
