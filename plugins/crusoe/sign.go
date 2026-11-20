package crusoe

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/paingha/tugcli/pkg/provider"
)

// signatureVersion is the version of Crusoe's request signature.
const signatureVersion = "1.0"

// timestampHeader carries the time a request was signed, which the signature
// covers so that a captured request cannot be replayed later.
const timestampHeader = "X-Crusoe-Timestamp"

// signer signs Crusoe Cloud API requests as
// https://docs.cloud.crusoe.ai/reference/api#authentication describes: an
// HMAC-SHA256, keyed with the decoded secret key, of the request path, the
// canonical query, the method and the timestamp, each followed by a newline,
// sent as "Bearer 1.0:<access key ID>:<signature>".
type signer struct {
	keyID string
	key   []byte
	now   func() time.Time
}

// newSigner decodes the secret key, which is URL-safe base64 without
// padding.
func newSigner(keyID, secretKey string, now func() time.Time) (*signer, error) {
	key, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(secretKey, "="))
	if err != nil || len(key) == 0 {
		return nil, provider.Errorf(provider.Unauthenticated, "the %s is not a Crusoe secret key", SecretSecretKey).WithHint(keyHint)
	}
	return &signer{keyID: keyID, key: key, now: now}, nil
}

// sign sets the timestamp and Authorization headers on req.
func (s *signer) sign(req *http.Request) error {
	query, err := canonicalQuery(req.URL.RawQuery)
	if err != nil {
		return provider.Errorf(provider.Internal, "signing %s: %v", req.URL.Path, err)
	}
	ts := s.now().UTC().Format(time.RFC3339)
	mac := hmac.New(sha256.New, s.key)
	mac.Write([]byte(req.URL.Path + "\n" + query + "\n" + req.Method + "\n" + ts + "\n"))
	req.Header.Set(timestampHeader, ts)
	req.Header.Set("Authorization", "Bearer "+signatureVersion+":"+s.keyID+":"+base64.RawURLEncoding.EncodeToString(mac.Sum(nil)))
	return nil
}

// canonicalQuery sorts the query parameters by name and joins them with &,
// escaped as Crusoe's own Go client escapes them.
func canonicalQuery(raw string) (string, error) {
	values, err := url.ParseQuery(raw)
	if err != nil {
		return "", err
	}
	return values.Encode(), nil
}
