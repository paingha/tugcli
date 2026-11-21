package iap

import (
	"crypto/tls"

	"github.com/paingha/tugcli/pkg/provider"
)

// mtlsRelayURL is the relay gcloud uses for certificate-based access
// (context-aware access), from MTLS_URL_HOST in the Cloud SDK's
// iap_tunnel_websocket_utils.py. gcloud adds no headers for it; the client
// certificate is the difference.
const mtlsRelayURL = "wss://mtls.tunnel.cloudproxy.app/v4"

// relayFor returns the relay for cfg, as gcloud's _CreateWebSocketUrl
// chooses it: the mTLS relay when the tunnel presents a client
// certificate, and the default relay otherwise.
func relayFor(cfg Config) string {
	if cfg.CertFile != "" {
		return mtlsRelayURL
	}
	return DefaultRelayURL
}

// clientCertificate returns a tls.Config GetClientCertificate that reads
// the certificate and key from their files at each handshake, so a tunnel
// that resumes after the files are renewed presents the new certificate.
func clientCertificate(certFile, keyFile string) func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
	return func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
		c, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			return nil, provider.Errorf(provider.InvalidConfig, "loading the IAP client certificate %s and key %s: %v", certFile, keyFile, err).
				WithHint("cert and key in the iap:// URL name a PEM certificate and its unencrypted PEM private key")
		}
		return &c, nil
	}
}

// clientTLS returns the TLS settings for the relay: base, or the
// defaults, plus cfg's client certificate when it has one. It checks that
// the certificate loads.
func clientTLS(base *tls.Config, cfg Config) (*tls.Config, error) {
	if cfg.CertFile == "" {
		return base, nil
	}
	load := clientCertificate(cfg.CertFile, cfg.KeyFile)
	if _, err := load(nil); err != nil {
		return nil, err
	}
	c := &tls.Config{}
	if base != nil {
		c = base.Clone()
	}
	c.GetClientCertificate = load
	return c, nil
}
